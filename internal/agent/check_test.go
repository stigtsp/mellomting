package agent_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"mellomting/internal/agent"
	"mellomting/internal/auth"
	"mellomting/internal/config"
)

func TestCheckEdge(t *testing.T) {
	var logs syncBuffer
	e := newEdgeWith(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}, "alice": {Models: []string{"*"}}},
		edgeOptions{log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))})
	check := func(key string) error {
		return agent.CheckEdge(context.Background(), agent.Options{Edge: e.url, Key: key})
	}
	if err := check(e.keys["gpu"]); err != nil {
		t.Fatalf("valid agent key: %v", err)
	}
	if err := check(e.keys["alice"]); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("client key: %v", err)
	}
	// The operator sees which key was turned away; the client sees only
	// a 401.
	if got := logs.String(); !strings.Contains(got, "agent attach refused") || !strings.Contains(got, "key=alice") {
		t.Fatalf("client key refusal not logged:\n%s", got)
	}
	if err := check("sk-gpu-0000000000000000-" + strings.Repeat("0", 64)); err == nil {
		t.Fatal("forged key accepted")
	}
	if got := e.registry.Snapshot(); len(got) != 0 {
		t.Fatalf("a check must not attach anything, got %v", got)
	}

	off := newEdgeWith(t, config.Agents{}, map[string]auth.Key{"gpu": {Agent: "lab"}}, edgeOptions{noAgents: true})
	err := agent.CheckEdge(context.Background(), agent.Options{Edge: off.url, Key: off.keys["gpu"]})
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("agents disabled: %v", err)
	}
}

// syncBuffer is a bytes.Buffer safe for a logger and a test reading it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
