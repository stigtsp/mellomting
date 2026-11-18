package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"mellomting/internal/backendmetrics"
	"mellomting/internal/testsupport"
)

const vllmPage = "vllm:num_requests_running 6\nvllm:num_requests_waiting 18\nvllm:kv_cache_usage_perc 0.94\n"

func TestAgentServesMetricsOnDemand(t *testing.T) {
	calls := 0
	fetch := func(_ context.Context, path string, _ int64) (int, []byte, error) {
		calls++
		if path == "/metrics" {
			return 200, []byte(vllmPage), nil
		}
		return 404, nil, nil
	}
	h := metricsHandler(true, fetch)
	for range 2 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, MetricsPath, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"waiting":18`) {
			t.Fatalf("status %d body %s", w.Code, w.Body)
		}
	}
	if calls != 1 {
		t.Fatalf("local server read %d times within the cache window", calls)
	}
}

func TestAgentMetricsRefused(t *testing.T) {
	h := metricsHandler(false, func(context.Context, string, int64) (int, []byte, error) {
		t.Fatal("read the local server although the operator refused")
		return 0, nil, nil
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, MetricsPath, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d", w.Code)
	}
}

// The metrics path is answered by the agent and never relayed to the
// local server.
func TestMetricsPathNotRelayed(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:9")
	h := backendHandler(Options{Backend: u, Log: testsupport.DiscardLogger()}, &stats{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, MetricsPath, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d (a relay attempt would be 502)", w.Code)
	}
}

func TestEdgeClampsAgentMetrics(t *testing.T) {
	if _, err := decodeAgentMetrics(strings.NewReader(`{"engine":"vllm","has_queue":true,"running":1,"surprise":1}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := decodeAgentMetrics(strings.NewReader(`{"engine":"vllm","x":"` + strings.Repeat("a", maxAgentMetricsBytes) + `"}`)); err == nil {
		t.Fatal("oversized reply accepted")
	}
	if _, err := decodeAgentMetrics(strings.NewReader(`{"engine":"evil"}`)); err == nil {
		t.Fatal("unknown engine accepted")
	}
	r, err := decodeAgentMetrics(strings.NewReader(`{"engine":"vllm","has_queue":true,"running":-4,"waiting":1e300,"kv_percent":250}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Running != 0 || r.Waiting != backendmetrics.MaxValue || r.KVPercent != 100 {
		t.Fatalf("not clamped: %+v", r)
	}
}

type countRT struct {
	n    int
	body string
	code int
}

func (c *countRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n++
	return &http.Response{StatusCode: c.code, Body: io.NopCloser(strings.NewReader(c.body)), Request: r}, nil
}

func TestMetricsPollNotCounted(t *testing.T) {
	rt := &countRT{code: 200, body: `{"engine":"sglang","has_queue":true,"running":3}`}
	a := &attachment{counters: &counters{}}
	next := pollAgentMetrics(context.Background(), rt, a)
	if rt.n != 1 || a.counters.requests.Load() != 0 || a.counters.failures.Load() != 0 {
		t.Fatalf("poll counted: rt=%d requests=%d", rt.n, a.counters.requests.Load())
	}
	if m := a.metrics.Load(); m == nil || m.Running != 3 || next != backendmetrics.Interval {
		t.Fatalf("metrics %+v next %v", m, next)
	}
	old := &countRT{code: 404}
	if next := pollAgentMetrics(context.Background(), old, a); next != backendmetrics.Absent || a.metrics.Load() != nil {
		t.Fatalf("old agent: next %v metrics %+v", next, a.metrics.Load())
	}
}
