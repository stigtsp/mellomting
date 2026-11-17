package agent

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"mellomting/internal/auth"
	"mellomting/internal/config"
)

func TestAttachmentCapsCountDrainingSeparately(t *testing.T) {
	g := NewRegistry(config.Agents{MaxAttachments: 4, MaxAttachmentsPerKey: 2}, nil, 0, nil)
	add := func(key string, draining bool) {
		a := &attachment{name: fmt.Sprintf("a%d", len(g.atts)), keyID: key}
		a.draining.Store(draining)
		g.atts[a.name] = a
	}
	full := func(key string) string {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.fullLocked(key)
	}

	add("k1", false)
	add("k1", false)
	if full("k1") == "" {
		t.Fatal("two live attachments of k1 must reach max_attachments_per_key=2")
	}
	// k1 replaces one: the old one drains and no longer holds the slot.
	g.atts["a0"].draining.Store(true)
	if got := full("k1"); got != "" {
		t.Fatalf("a draining attachment must not block its replacement: %s", got)
	}
	// Draining connections are still bounded: twice the per-key cap.
	add("k1", true)
	add("k1", true)
	if got := full("k1"); got != "agents.max_attachments_per_key" {
		t.Fatalf("k1 with 1 live + 3 draining: got %q, want the per-key limit", got)
	}
	// The global cap counts live attachments, bounded at twice for all.
	add("k2", false)
	add("k2", false)
	add("k3", false)
	if got := full("k4"); got != "agents.max_attachments" {
		t.Fatalf("4 live attachments: got %q, want the global limit", got)
	}
}

// Agents racing for the last slots: exactly the cap's worth of
// reservations succeed, and the rest are refused before any upgrade.
func TestReserveIsAtomicAtTheCap(t *testing.T) {
	g := NewRegistry(config.Agents{MaxAttachments: 16, MaxAttachmentsPerKey: 3}, nil, 0, nil)
	key := &auth.Key{ID: "0000000000000001", Name: "gpu", Agent: "lab"}
	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if _, limit := g.Reserve(key, "127.0.0.1"); limit == "" {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 3 {
		t.Fatalf("%d reservations succeeded, want exactly 3", ok.Load())
	}
}
