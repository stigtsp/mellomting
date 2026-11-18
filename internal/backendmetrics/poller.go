package backendmetrics

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

var (
	// Interval is how often a server that reports metrics is read.
	Interval = 5 * time.Second
	// Retry is how soon a server that failed to answer is read again.
	Retry = 30 * time.Second
	// Absent is how soon a server that publishes nothing is asked
	// again, in case it was restarted with metrics on.
	Absent = 5 * time.Minute
	// Stale is the age past which a reading is not shown.
	Stale = 3 * Interval
	// timeout bounds one read.
	timeout = 2 * time.Second
)

// Fetch reads path from one server and returns its status and at most
// limit bytes of body. It is the backend client's Probe, or a stub.
type Fetch func(ctx context.Context, path string, limit int64) (status int, body []byte, err error)

// Poller reads every server's report on its own schedule.
type Poller struct {
	fetch  map[string]Fetch
	log    *slog.Logger
	mu     sync.Mutex
	latest map[string]Reading
}

// NewPoller polls the given servers by name.
func NewPoller(servers map[string]Fetch, log *slog.Logger) *Poller {
	if log == nil {
		log = slog.Default()
	}
	return &Poller{fetch: servers, log: log, latest: make(map[string]Reading)}
}

// Run polls until ctx ends, one goroutine per server.
func (p *Poller) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for name := range p.fetch {
		wg.Go(func() {
			for {
				next := p.pollOne(ctx, name)
				// ±10 % so servers polled together drift apart.
				next += time.Duration((rand.Float64() - 0.5) * 0.2 * float64(next))
				select {
				case <-ctx.Done():
					return
				case <-time.After(next):
				}
			}
		})
	}
	wg.Wait()
}

// Latest returns the fresh readings by server name.
func (p *Poller) Latest() map[string]Reading {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]Reading, len(p.latest))
	for name, r := range p.latest {
		if time.Since(r.At) <= Stale {
			out[name] = r
		}
	}
	return out
}

// pollOne reads one server and returns when to read it next: /metrics
// for vLLM and SGLang, else /api/ps for Ollama. Nothing is logged above
// debug: a server without metrics is normal, not a fault.
func (p *Poller) pollOne(ctx context.Context, name string) time.Duration {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fetch := p.fetch[name]
	r, ok, err := Read(ctx, fetch)
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case err != nil:
		delete(p.latest, name)
		p.log.Debug("backend metrics unavailable", "server", name, "error", err)
		return Retry
	case !ok:
		delete(p.latest, name)
		return Absent
	}
	p.latest[name] = r
	return Interval
}

// Read takes one server's report: /metrics for vLLM and SGLang, else
// /api/ps for Ollama. ok is false when it publishes neither.
func Read(ctx context.Context, fetch Fetch) (Reading, bool, error) {
	status, body, err := fetch(ctx, "/metrics", MaxBody+1)
	if err != nil {
		return Reading{}, false, err
	}
	if status == 200 {
		r, ok, err := Parse(bytes.NewReader(body))
		if err != nil || ok {
			return r, ok, err
		}
	}
	status, body, err = fetch(ctx, "/api/ps", MaxOllamaBody+1)
	if err != nil {
		return Reading{}, false, err
	}
	if status != 200 {
		return Reading{}, false, nil
	}
	r, err := ParseOllamaPS(bytes.NewReader(body))
	if err != nil {
		// A 200 that is not Ollama's shape: nothing to show.
		return Reading{}, false, nil
	}
	return r, true, nil
}

// Servers lists the polled servers, sorted.
func (p *Poller) Servers() []string { return slices.Sorted(maps.Keys(p.fetch)) }
