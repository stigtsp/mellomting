package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"mellomting/internal/adminapi"
	"mellomting/internal/agent"
	"mellomting/internal/inflight"
)

func TestRenderRequestsAndAgents(t *testing.T) {
	snap := adminapi.Snapshot{
		Requests: []inflight.Record{
			{KeyName: "alice", Model: "lab/m1", Backend: "agent-gpu-1", Phase: inflight.PhaseStreaming, AgeMillis: 2500, Remote: "10.0.0.5"},
			{KeyName: "bob", Model: "local", Backend: "auto-local-0123456789ab", Phase: inflight.PhaseWaiting, AgeMillis: 100},
		},
		Agents: []agent.Record{{
			Name: "agent-gpu-1", KeyName: "gpu", Remote: "203.0.113.7", AgeMillis: 90_000, State: "serving",
			Admitted: 3, MaxConcurrency: 256, Queued: 1, Streams: 3, Requests: 42, Failures: 2,
			Models: []agent.Model{{Name: "lab/m1", ContextLength: 32768}, {Name: "lab/m2"}},
		}},
	}
	var buf bytes.Buffer
	render(&buf, snap, time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC), false, 0)
	out := buf.String()
	lines := strings.Split(out, "\n")

	if !strings.HasPrefix(lines[0], "mellomting 12:00:00 — 2 in flight, 1 agent attached") {
		t.Fatalf("header: %q", lines[0])
	}
	for _, want := range []string{
		"AGE", "AGENT", "agent-gpu-1", // the request routed to the agent names it
		"SLOTS", "3/256", "QUEUE", "STREAMS", "REQS", "FAIL", "lab/m1,lab/m2", "1m30s", "serving",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// A configured backend is not shown as an agent.
	for _, l := range lines {
		if strings.Contains(l, "bob") && strings.Contains(l, "auto-local") {
			t.Errorf("configured backend leaked into the AGENT column: %q", l)
		}
	}
	if strings.Contains(out, "\033[") {
		t.Error("a snapshot (--once) must not contain escape sequences")
	}
}

func TestRenderCapsRowsInLiveView(t *testing.T) {
	var snap adminapi.Snapshot
	for i := range 300 {
		snap.Requests = append(snap.Requests, inflight.Record{KeyName: "k", AgeMillis: int64(300 - i)})
	}
	var buf bytes.Buffer
	render(&buf, snap, time.Now(), true, 40)
	out := buf.String()
	if got := strings.Count(out, "\nk ") + strings.Count(out, " k "); got == 0 {
		t.Fatalf("no request rows rendered:\n%s", out)
	}
	if !strings.Contains(out, "… and 260 more") {
		t.Fatalf("live view must say how many rows it left out:\n%s", out)
	}
	// Without agents the header does not mention them.
	if strings.Contains(out, "attached") {
		t.Fatal("agents disabled: header must not mention agents")
	}
}

func TestRenderNothingInFlight(t *testing.T) {
	var buf bytes.Buffer
	render(&buf, adminapi.Snapshot{Agents: []agent.Record{}}, time.Now(), false, 0)
	if out := buf.String(); !strings.Contains(out, "0 in flight, 0 agents attached") || strings.Contains(out, "AGE") {
		t.Fatalf("empty snapshot: %q", out)
	}
}
