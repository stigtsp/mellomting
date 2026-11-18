package backendmetrics

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

type fakeServer map[string]struct {
	status int
	file   string
}

func (f fakeServer) fetch(t *testing.T) Fetch {
	return func(_ context.Context, path string, limit int64) (int, []byte, error) {
		r, ok := f[path]
		if !ok {
			return 404, nil, nil
		}
		if r.status == 0 {
			return 0, nil, errors.New("connection refused")
		}
		b, err := os.ReadFile("testdata/" + r.file)
		if err != nil {
			t.Fatal(err)
		}
		return r.status, b, nil
	}
}

func TestPollVLLM(t *testing.T) {
	p := NewPoller(map[string]Fetch{"gpu1": fakeServer{"/metrics": {200, "vllm.txt"}}.fetch(t)}, nil)
	if next := p.pollOne(context.Background(), "gpu1"); next != Interval {
		t.Fatalf("next poll in %v, want %v", next, Interval)
	}
	if r, ok := p.Latest()["gpu1"]; !ok || r.Engine != "vllm" || r.Waiting != 18 {
		t.Fatalf("latest %+v", p.Latest())
	}
}

func TestPollerFallsBackToOllama(t *testing.T) {
	p := NewPoller(map[string]Fetch{"ol": fakeServer{"/api/ps": {200, "ollama_ps.json"}}.fetch(t)}, nil)
	if next := p.pollOne(context.Background(), "ol"); next != Interval {
		t.Fatalf("next %v", next)
	}
	if r := p.Latest()["ol"]; r.Engine != "ollama" || len(r.Loaded) != 2 {
		t.Fatalf("latest %+v", r)
	}
}

// Another Mellomting answers 404 on both paths: no metrics, and the
// poller backs off instead of asking every five seconds.
func TestPollerBacksOffWithoutMetrics(t *testing.T) {
	p := NewPoller(map[string]Fetch{"edge2": fakeServer{}.fetch(t)}, nil)
	if next := p.pollOne(context.Background(), "edge2"); next != Absent {
		t.Fatalf("next %v, want %v", next, Absent)
	}
	if _, ok := p.Latest()["edge2"]; ok {
		t.Fatal("a server without metrics has a reading")
	}
}

func TestPollerDropsReadingWhenServerFails(t *testing.T) {
	srv := fakeServer{"/metrics": {200, "vllm.txt"}}
	p := NewPoller(map[string]Fetch{"gpu1": srv.fetch(t)}, nil)
	p.pollOne(context.Background(), "gpu1")
	srv["/metrics"] = struct {
		status int
		file   string
	}{0, ""}
	if next := p.pollOne(context.Background(), "gpu1"); next != Retry {
		t.Fatalf("next %v, want %v", next, Retry)
	}
	if _, ok := p.Latest()["gpu1"]; ok {
		t.Fatal("stale reading kept after failure")
	}
}

func TestLatestDropsStale(t *testing.T) {
	p := NewPoller(map[string]Fetch{"gpu1": fakeServer{"/metrics": {200, "vllm.txt"}}.fetch(t)}, nil)
	p.pollOne(context.Background(), "gpu1")
	p.mu.Lock()
	r := p.latest["gpu1"]
	r.At = time.Now().Add(-Stale - time.Second)
	p.latest["gpu1"] = r
	p.mu.Unlock()
	if _, ok := p.Latest()["gpu1"]; ok {
		t.Fatal("stale reading returned")
	}
}
