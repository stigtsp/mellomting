package proxy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/routing"
)

// dualEnv is a two-backend proxy (PLAN §93): b1 is the primary, b2 the
// fallback. maxAttempts bounds the retry budget for the test.
type dualEnv struct {
	p    *Proxy
	f1   *fakeVLLM
	f1n  atomic.Int64
	f2   *fakeVLLM
	f2n  atomic.Int64
	body string
}

func newDualEnv(t *testing.T, maxAttempts int, b1, b2 http.HandlerFunc) *dualEnv {
	return newDualEnvIdle(t, maxAttempts, 2*time.Second, b1, b2)
}

func newDualEnvIdle(t *testing.T, maxAttempts int, idle time.Duration, b1, b2 http.HandlerFunc) *dualEnv {
	t.Helper()
	e := &dualEnv{body: `{"model":"gen-1","messages":[{"role":"u","content":"x"}]}`}
	e.f1n.Store(0)
	e.f2n.Store(0)
	e.f1 = newFakeVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		e.f1n.Add(1)
		b1(w, r)
	})
	e.f2 = newFakeVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		e.f2n.Add(1)
		b2(w, r)
	})

	be := config.Backend{
		MaxConcurrency:    4,
		QueueSize:         4,
		QueueTimeout:      config.Duration(200 * time.Millisecond),
		ConnectTimeout:    config.Duration(time.Second),
		HeaderTimeout:     config.Duration(2 * time.Second),
		RequestTimeout:    config.Duration(5 * time.Second),
		StreamIdleTimeout: config.Duration(idle),
	}
	cfg := testConfig(e.f1.server.URL)
	beA := cfg.Backends["b1"]
	beA.UpstreamModel = "Up/A"
	beA.StreamIdleTimeout = config.Duration(idle)
	cfg.Backends["b1"] = beA
	be.BaseURL = e.f2.server.URL
	be.UpstreamModel = "Up/B"
	cfg.Backends["b2"] = be
	m := cfg.Models["gen-1"]
	m.Strategy = "least-inflight"
	m.Backends = []config.BackendRef{{Name: "b1"}, {Name: "b2"}}
	cfg.Models["gen-1"] = m
	cfg.Retry = config.Retry{
		MaxAttempts:    maxAttempts,
		InitialBackoff: config.Duration(2 * time.Millisecond),
		MaxBackoff:     config.Duration(4 * time.Millisecond),
		Jitter:         func() *bool { f := false; return &f }(),
	}

	cl1, err := backend.New(backend.Options{
		Name: "b1", Cfg: cfg.Backends["b1"], Network: backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cl2, err := backend.New(backend.Options{
		Name: "b2", Cfg: cfg.Backends["b2"], Network: backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	clients := map[string]*backend.Client{"b1": cl1, "b2": cl2}
	router, err := routing.New(cfg, func(name string) int {
		c, ok := clients[name]
		if !ok {
			return 0
		}
		return c.Inflight()
	})
	if err != nil {
		t.Fatal(err)
	}
	e.p, err = New(cfg, router, clients, discardLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// deadURL returns a loopback address that refuses connections.
func deadURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr
}

func (e *dualEnv) chat(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return run(t, e.p, http.MethodPost, "/v1/chat/completions", e.body, testKey())
}

// newDeadClient builds a b1 client pointed at a port that refuses
// connections (pre-stream connect failure).
func newDeadClient(t *testing.T) *backend.Client {
	t.Helper()
	c, err := backend.New(backend.Options{
		Name: "b1", Cfg: config.Backend{
			BaseURL:           deadURL(t),
			UpstreamModel:     "Up/A",
			MaxConcurrency:    4,
			QueueSize:         4,
			QueueTimeout:      config.Duration(200 * time.Millisecond),
			ConnectTimeout:    config.Duration(time.Second),
			HeaderTimeout:     config.Duration(2 * time.Second),
			RequestTimeout:    config.Duration(5 * time.Second),
			StreamIdleTimeout: config.Duration(2 * time.Second),
		},
		Network:          backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: 1 << 20,
		Log:              discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// PLAN §93: a pre-stream backend failure moves safely to the replica.
func TestPreStreamFailureFallsBack(t *testing.T) {
	t.Parallel()
	e := newDualEnv(t, 2, upstream500, okJSON)
	// Retarget b1 at a dead port so the first attempt fails pre-stream.
	e.p.clients["b1"] = newDeadClient(t)

	rec := e.chat(t)
	if rec.Code != 200 {
		t.Fatalf("status = %d body = %s (want 200 via fallback)", rec.Code, rec.Body.String())
	}
	if e.f1n.Load() != 0 || e.f2n.Load() != 1 {
		t.Fatalf("attempts a=%d b=%d, want 0/1", e.f1n.Load(), e.f2n.Load())
	}
	if e.f2.lastModel != "Up/B" {
		t.Fatalf("fallback did not rewrite the model: %q", e.f2.lastModel)
	}
}

// PLAN §93: a stream that fails after bytes reached the client is
// returned, never retried on another backend.
func TestStreamFailureIsNeverRetried(t *testing.T) {
	t.Parallel()
	aSSE := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"partial\":1}\n\n"))
		// Handler returns: the connection closes mid-stream.
	}
	e := newDualEnv(t, 2, aSSE, okJSON)
	rec := run(t, e.p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true}`, testKey())
	if rec.Code != 200 {
		t.Fatalf("status = %d (headers already committed)", rec.Code)
	}
	if e.f1n.Load() != 1 || e.f2n.Load() != 0 {
		t.Fatalf("post-stream retry happened: a=%d b=%d, want 1/0", e.f1n.Load(), e.f2n.Load())
	}
	if !strings.Contains(rec.Body.String(), `"partial":1`) {
		t.Fatalf("first event lost: %q", rec.Body.String())
	}
}

// T-T8: the stream-failure-never-retried property must hold for a real
// mid-stream ABORT (not just a clean EOF): the backend writes an event,
// then RSTs the TCP connection. The proxy has already committed 200 and
// relayed bytes, so it must truncate and return without ever trying the
// sibling replica.
func TestStreamAbortIsNeverRetried(t *testing.T) {
	t.Parallel()
	abortSSE := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"partial\":1}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetLinger(0) // RST, not a clean FIN.
		}
		_ = conn.Close()
	}
	e := newDualEnv(t, 2, abortSSE, okJSON)
	rec := run(t, e.p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true}`, testKey())
	if rec.Code != 200 {
		t.Fatalf("status = %d (headers already committed)", rec.Code)
	}
	if e.f1n.Load() != 1 || e.f2n.Load() != 0 {
		t.Fatalf("post-stream retry happened: a=%d b=%d, want 1/0", e.f1n.Load(), e.f2n.Load())
	}
	if !strings.Contains(rec.Body.String(), `"partial":1`) {
		t.Fatalf("first event lost: %q", rec.Body.String())
	}
}

// T-T8: the same never-retried property must hold when the stream is cut
// by stream_idle_timeout (PLAN §24): the backend writes an event, then
// goes silent until the proxy's read-idle bound fires. The stream is
// truncated; the sibling replica is never tried.
func TestStreamIdleTimeoutIsNeverRetried(t *testing.T) {
	t.Parallel()
	stall := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"partial\":1}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // hold the stream open until cancelled
	}
	e := newDualEnvIdle(t, 2, 60*time.Millisecond, stall, okJSON)
	rec := run(t, e.p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true}`, testKey())
	if rec.Code != 200 {
		t.Fatalf("status = %d (headers already committed)", rec.Code)
	}
	if e.f1n.Load() != 1 || e.f2n.Load() != 0 {
		t.Fatalf("post-stream retry happened: a=%d b=%d, want 1/0", e.f1n.Load(), e.f2n.Load())
	}
	if !strings.Contains(rec.Body.String(), `"partial":1`) {
		t.Fatalf("first event lost: %q", rec.Body.String())
	}
}

// PLAN §23: upstream 429 is retryable; a 500 (not on the list) is not.
func TestRetryableUpstreamStatuses(t *testing.T) {
	t.Parallel()
	// 429 then healthy replica: fallback succeeds.
	e := newDualEnv(t, 2, upstream429, okJSON)
	rec := e.chat(t)
	if rec.Code != 200 || e.f1n.Load() != 1 || e.f2n.Load() != 1 {
		t.Fatalf("429 fallback: status=%d a=%d b=%d", rec.Code, e.f1n.Load(), e.f2n.Load())
	}

	// 500 is not on the retry list: single attempt, sanitized 502.
	e2 := newDualEnv(t, 2, upstream500, okJSON)
	rec = e2.chat(t)
	if rec.Code != 502 || e2.f1n.Load() != 1 || e2.f2n.Load() != 0 {
		t.Fatalf("500 not retried: status=%d a=%d b=%d body=%s",
			rec.Code, e2.f1n.Load(), e2.f2n.Load(), rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream_unavailable") {
		t.Fatalf("body = %s", rec.Body.String())
	}

	// 400-class upstream rejections are never retried.
	e3 := newDualEnv(t, 2, upstream400, okJSON)
	rec = e3.chat(t)
	if rec.Code != 400 || e3.f1n.Load() != 1 || e3.f2n.Load() != 0 {
		t.Fatalf("400 not retried: status=%d a=%d b=%d", rec.Code, e3.f1n.Load(), e3.f2n.Load())
	}
}

// PLAN §23: the global attempt bound holds across fallback backends —
// no multiplicative amplification.
func TestAttemptsGloballyBounded(t *testing.T) {
	t.Parallel()
	e := newDualEnv(t, 3, upstream429, upstream429)
	rec := e.chat(t)
	if rec.Code != 429 {
		t.Fatalf("status = %d, want 429 after exhaustion", rec.Code)
	}
	// Attempt 1 -> b1, attempt 2 -> b2 (fallback), attempt 3 -> b2
	// again (same-backend retry): total 3, exactly the configured bound.
	if e.f1n.Load() != 1 || e.f2n.Load() != 2 {
		t.Fatalf("amplified attempts: a=%d b=%d, want 1/2 (bound 3)",
			e.f1n.Load(), e.f2n.Load())
	}
}

// PLAN §22: an exhausted admission queue falls back to another eligible
// backend instead of failing the request.
func TestQueueFullFallsBack(t *testing.T) {
	t.Parallel()
	e := newDualEnv(t, 2, okJSON, okJSON)
	// b1's queue is empty-sized: every admission is "queue full".
	cfg := e.p.cfg
	b := cfg.Backends["b1"]
	b.QueueSize = 0
	b.QueueTimeout = config.Duration(50 * time.Millisecond)
	cfg.Backends["b1"] = b
	// Rebuild b1's client with the zero queue.
	c, err := backend.New(backend.Options{
		Name: "b1", Cfg: b, Network: backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.p.clients["b1"] = c

	rec := e.chat(t)
	if rec.Code != 200 {
		t.Fatalf("status = %d body = %s (want 200 via queue fallback)", rec.Code, rec.Body.String())
	}
	if e.f2n.Load() != 1 {
		t.Fatalf("fallback not used: b=%d", e.f2n.Load())
	}
}

// PLAN §21.3: a stateful Responses request stays on its owning backend
// and is never redirected to a sibling replica.
func TestResponsesStayOnOwningBackend(t *testing.T) {
	t.Parallel()
	var ownerCalls int64
	behaviour := func(w http.ResponseWriter, r *http.Request) {
		// b1 serves the first /v1/responses call (the create) and GET
		// retrieves fine, but 429s on continuations.
		atomic.AddInt64(&ownerCalls, 1)
		if r.Method == http.MethodGet || (r.URL.Path == "/v1/responses" && atomic.LoadInt64(&ownerCalls) == 1) {
			responsesJSON(w, r)
			return
		}
		upstream429(w, r)
	}
	e := newDualEnv(t, 2, behaviour, okJSON)

	// Create a response on b1 (the replica selected first).
	rec := run(t, e.p, http.MethodPost, "/v1/responses",
		`{"model":"gen-1","input":"hi"}`, testKey())
	if rec.Code != 200 || e.f2n.Load() != 0 {
		t.Fatalf("create: status=%d b=%d (must land on b1)", rec.Code, e.f2n.Load())
	}
	if b, ok := e.p.affinity.Get("K1", "resp_123"); !ok || b != "b1" {
		t.Fatalf("affinity = %q ok=%v, want b1", b, ok)
	}

	// Continue with previous_response_id on a 429ing owner: it may be
	// retried on the owner, never on the sibling.
	rec = run(t, e.p, http.MethodPost, "/v1/responses",
		`{"model":"gen-1","previous_response_id":"resp_123","input":"more"}`, testKey())
	if rec.Code != 429 {
		t.Fatalf("status = %d (owner stays 429, sanitized)", rec.Code)
	}
	if atomic.LoadInt64(&ownerCalls) != 3 || e.f2n.Load() != 0 {
		t.Fatalf("left owning backend: owner=%d b=%d, want 3/0",
			ownerCalls, e.f2n.Load())
	}

	// Retrieve the same ID: still owned by b1.
	rec = run(t, e.p, http.MethodGet, "/v1/responses/resp_123", "", testKey())
	if rec.Code != 200 {
		t.Fatalf("retrieve status = %d", rec.Code)
	}
	if e.f2n.Load() != 0 {
		t.Fatalf("retrieve left the owner: b=%d", e.f2n.Load())
	}
}

func upstream400(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(400)
	_, _ = w.Write([]byte(`{"error":{"message":"rejected"}}`))
}
