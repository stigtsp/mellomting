package inflight

import (
	"sync"
	"testing"
	"time"
)

func TestRegistryTracksAndForgets(t *testing.T) {
	r := New()
	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("empty registry = %v", got)
	}

	q := r.Begin("req_1", "K1", "alice", "198.51.100.9", "codex/1.0", "POST /v1/chat/completions")
	q.Target("gen-1", "b1")
	q.Read(120)
	q.Phase(PhaseStreaming)
	q.Wrote(40)
	q.Wrote(60)
	q.Tokens(15)

	got := r.Snapshot()
	if len(got) != 1 {
		t.Fatalf("snapshot = %v, want one record", got)
	}
	rec := got[0]
	if rec.RequestID != "req_1" || rec.KeyName != "alice" || rec.Remote != "198.51.100.9" ||
		rec.UserAgent != "codex/1.0" || rec.Endpoint != "POST /v1/chat/completions" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Model != "gen-1" || rec.Backend != "b1" || rec.Phase != PhaseStreaming {
		t.Fatalf("record = %+v", rec)
	}
	if rec.BytesIn != 120 || rec.BytesOut != 100 || rec.Tokens != 15 {
		t.Fatalf("counters = %+v", rec)
	}
	if rec.AgeMillis < 0 {
		t.Fatalf("age = %d", rec.AgeMillis)
	}

	// A finished request is forgotten, which is what bounds the
	// registry by concurrency rather than by traffic.
	q.End()
	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("ended request still listed: %v", got)
	}
}

// The oldest request is the one an operator is looking for, so it comes
// first.
func TestSnapshotOrdersOldestFirst(t *testing.T) {
	r := New()
	first := r.Begin("req_old", "K1", "a", "", "", "POST /v1/chat/completions")
	second := r.Begin("req_new", "K1", "a", "", "", "POST /v1/chat/completions")
	defer first.End()
	defer second.End()

	// Age is measured from Begin, so the first handle is at least as old.
	got := r.Snapshot()
	if len(got) != 2 {
		t.Fatalf("snapshot = %v", got)
	}
	if got[0].AgeMillis < got[1].AgeMillis {
		t.Fatalf("newest first: %v", got)
	}
}

// Every method is safe on a nil handle, so a daemon that tracks nothing
// runs the same code without a nil check at each call site.
func TestNilHandleIsInert(t *testing.T) {
	var r *Registry
	q := r.Begin("x", "", "", "", "", "")
	q.Target("m", "b")
	q.Read(1)
	q.Wrote(1)
	q.Tokens(1)
	q.Phase(PhaseWaiting)
	q.End()
	if got := r.Snapshot(); got != nil {
		t.Fatalf("nil registry snapshot = %v", got)
	}
}

// The pump writes response bytes from its own goroutine while the
// snapshot is read from the admin listener's.
func TestConcurrentUpdatesAndSnapshots(t *testing.T) {
	r := New()
	q := r.Begin("req_1", "K1", "a", "", "", "POST /v1/chat/completions")
	defer q.End()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 200 {
				q.Wrote(1)
				q.Phase(PhaseStreaming)
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				_ = r.Snapshot()
			}
		}()
	}
	wg.Wait()
	if got := r.Snapshot(); got[0].BytesOut != 8*200 {
		t.Fatalf("bytes lost under concurrency: %d", got[0].BytesOut)
	}
}

func TestPhaseAndTTFB(t *testing.T) {
	r := New()
	q := r.Begin("req_1", "K1", "alice", "", "", "POST /v1/chat/completions")
	time.Sleep(30 * time.Millisecond)
	if rec := r.Snapshot()[0]; rec.PhaseMillis < 30 || rec.TTFBMillis != 0 {
		t.Fatalf("before any change: %+v", rec)
	}
	q.Phase(PhaseWaiting)
	if rec := r.Snapshot()[0]; rec.PhaseMillis >= 30 {
		t.Fatalf("phase change kept the old phase's time: %+v", rec)
	}
	q.Wrote(10)
	first := r.Snapshot()[0].TTFBMillis
	if first < 30 {
		t.Fatalf("ttfb %d, want at least the 30ms before it", first)
	}
	time.Sleep(20 * time.Millisecond)
	q.Wrote(10)
	if got := r.Snapshot()[0].TTFBMillis; got != first {
		t.Fatalf("a later write moved ttfb from %d to %d", first, got)
	}
	var nilReq *Request
	if nilReq.TTFB() != 0 {
		t.Fatal("nil request has a ttfb")
	}
}
