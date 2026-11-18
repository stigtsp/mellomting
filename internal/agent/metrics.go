package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"mellomting/internal/backendmetrics"
)

const (
	// MetricsPath is answered by the agent itself with its local
	// server's load report; it is never relayed (PLAN §104).
	MetricsPath = "/mellomting/backend-metrics"
	// maxAgentMetricsBytes bounds the reply the edge accepts.
	maxAgentMetricsBytes = 16 << 10
	// metricsCache spares the local server when several readers ask at
	// once; the edge asks every few seconds at most.
	metricsCache = 2 * time.Second
	// metricsTimeout bounds one read on either side.
	metricsTimeout = 2 * time.Second
)

// metricsHandler serves the local server's report, read only when the
// edge asks. enabled false is the agent operator refusing to share
// their server's load (attach.metrics: false): every ask is a 404.
func metricsHandler(enabled bool, fetch backendmetrics.Fetch) http.Handler {
	var mu sync.Mutex
	var at time.Time
	var body []byte
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !enabled {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) > metricsCache {
			ctx, cancel := context.WithTimeout(r.Context(), metricsTimeout)
			reading, ok, err := backendmetrics.Read(ctx, fetch)
			cancel()
			body = nil
			if err == nil && ok {
				body, _ = json.Marshal(reading)
			}
			at = time.Now()
		}
		if body == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
}

// localFetch reads the local server's own endpoints with its bearer
// token, never the edge's.
func localFetch(o Options) backendmetrics.Fetch {
	client := &http.Client{
		Transport:     backendTransport(o),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return func(ctx context.Context, path string, limit int64) (int, []byte, error) {
		u := *o.Backend
		u.Path = path
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return 0, nil, err
		}
		if o.BackendKey != "" {
			req.Header.Set("Authorization", "Bearer "+o.BackendKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
		return resp.StatusCode, b, err
	}
}

// decodeAgentMetrics accepts an agent's report only in the exact
// shape, within the size bound, and with every figure clamped: the
// agent's operator is not the edge's (PLAN §104.6).
func decodeAgentMetrics(r io.Reader) (*backendmetrics.Reading, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxAgentMetricsBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxAgentMetricsBytes {
		return nil, errors.New("agent metrics too large")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var out backendmetrics.Reading
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	if err := backendmetrics.Sanitize(&out); err != nil {
		return nil, err
	}
	// The edge's clock, not the agent's.
	out.At = time.Now()
	return &out, nil
}

// pollAgentMetrics asks one attachment for its report over rt, the
// attachment's raw transport: outside admission and outside the
// request and failure counters, so observing an agent never changes
// what routing or the operator sees of its traffic. It returns when to
// ask next.
func pollAgentMetrics(ctx context.Context, rt http.RoundTripper, a *attachment) time.Duration {
	ctx, cancel := context.WithTimeout(ctx, metricsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent.invalid"+MetricsPath, nil)
	if err != nil {
		return backendmetrics.Retry
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		a.metrics.Store(nil)
		return backendmetrics.Retry
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// An older agent, one whose operator refused, or a server that
		// publishes nothing.
		a.metrics.Store(nil)
		return backendmetrics.Absent
	}
	m, err := decodeAgentMetrics(resp.Body)
	if err != nil {
		a.metrics.Store(nil)
		return backendmetrics.Absent
	}
	a.metrics.Store(m)
	return backendmetrics.Interval
}

// PollMetrics asks every serving attachment for its report on its own
// schedule until ctx ends. The daemon runs it only when an admin socket
// can show the result.
func (g *Registry) PollMetrics(ctx context.Context) {
	due := map[*attachment]time.Time{}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now()
		g.mu.Lock()
		var ready []*attachment
		live := map[*attachment]bool{}
		for _, a := range g.atts {
			live[a] = true
			if a.client != nil && !a.draining.Load() && !now.Before(due[a]) {
				ready = append(ready, a)
				due[a] = now.Add(backendmetrics.Interval) // until the poll says otherwise
			}
		}
		g.mu.Unlock()
		for a := range due {
			if !live[a] {
				delete(due, a)
			}
		}
		for _, a := range ready {
			next := pollAgentMetrics(ctx, a.tr, a)
			due[a] = time.Now().Add(next)
		}
	}
}
