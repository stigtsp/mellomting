package sim

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mellomting/internal/usagehist"
)

var start = time.Date(2026, 11, 18, 13, 23, 34, 0, time.UTC)

func TestSimDeterministic(t *testing.T) {
	a, _ := json.Marshal(New(7, start, nil).Snapshot(0))
	b, _ := json.Marshal(New(7, start, nil).Snapshot(0))
	if string(a) != string(b) {
		t.Fatal("same seed and time gave different data")
	}
	c, _ := json.Marshal(New(8, start, nil).Snapshot(0))
	if string(a) == string(c) {
		t.Fatal("seed ignored")
	}
}

func TestSimAdvancesWithClock(t *testing.T) {
	now := start
	s := New(1, start, func() time.Time { return now })
	first, _ := s.Stats(context.Background(), 0)
	now = now.Add(3 * time.Second)
	next, _ := s.Stats(context.Background(), first.Stats.Seq)
	if next.Stats.Seq != first.Stats.Seq+3 || len(next.Stats.Global.Buckets) != 3 {
		t.Fatalf("seq %d → %d, %d new buckets", first.Stats.Seq, next.Stats.Seq, len(next.Stats.Global.Buckets))
	}
	if first.Stats.Global.Refusals["key_concurrency"] == 0 || len(first.Agents) != 1 || first.Agents[0].Metrics.Waiting < 16 {
		t.Fatal("the screenshot scenario is missing")
	}
}

// Within one five-minute cycle every incident the attention box ranks
// shows up, so --simulate exercises all of it.
func TestSimScriptedIncidents(t *testing.T) {
	now := start
	s := New(3, start, func() time.Time { return now })
	var dropped, cooldown, queueing bool
	for range 300 {
		now = now.Add(time.Second)
		st, _ := s.Stats(context.Background(), 0)
		dropped = dropped || st.Global.Accounting.Dropped > 0
		for _, srv := range st.Servers {
			cooldown = cooldown || len(srv.Health) > 0
		}
		queueing = queueing || st.Agents[0].Metrics.Waiting > 0
	}
	if !dropped || !cooldown || !queueing {
		t.Fatalf("dropped=%v cooldown=%v queueing=%v", dropped, cooldown, queueing)
	}
}

func TestSimUsageHistory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.jsonl")
	f, _ := os.Create(p)
	if err := New(3, start, nil).WriteUsage(f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	h, err := usagehist.Open(p, start)
	if err != nil {
		t.Fatal(err)
	}
	from, _ := h.Covers()
	if start.Sub(from) < 6*24*time.Hour || len(h.Totals(usagehist.ByServer, usagehist.Span, start)) < 3 {
		t.Fatalf("history covers from %v, servers %v", from, h.Totals(usagehist.ByServer, usagehist.Span, start))
	}
	if BackendServer["auto-gpu1-6a1d0c3e9b27"] != "gpu1" {
		t.Fatal("no backend → server map for the generated names")
	}
}
