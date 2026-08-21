package httpapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mellomting/internal/auth"
	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/proxy"
	"mellomting/internal/routing"
)

type env struct {
	srv  *Server
	key  string // a valid raw API key
	key2 string // a second valid key
}

// buildEnv assembles the full HTTP surface in front of a fake backend.
// mod, when non-nil, tweaks the effective configuration before wiring;
// k1lim is the per-key limit block of the model-a key.
func buildEnv(t *testing.T, behaviour http.HandlerFunc, mod func(*config.Config), k1lim auth.KeyLimits) *env {
	return buildEnvWithLog(t, behaviour, mod, k1lim, testLogger())
}

// buildEnvWithLog is buildEnv with a caller-supplied logger (used to
// assert bounded log output, e.g. T-M4's invalid-auth flood).
func buildEnvWithLog(t *testing.T, behaviour http.HandlerFunc, mod func(*config.Config), k1lim auth.KeyLimits, log *slog.Logger) *env {
	return buildEnvWithUsers(t, behaviour, mod, k1lim, log, nil)
}

// buildEnvWithUsers additionally lets the caller mutate the users file
// before the store is built (e.g. to add disabled or expired keys).
func buildEnvWithUsers(t *testing.T, behaviour http.HandlerFunc, mod func(*config.Config), k1lim auth.KeyLimits, log *slog.Logger, usersFn func(*auth.UsersFile)) *env {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/responses/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_x","object":"response"}`))
			return
		}
		behaviour(w, r)
	}))
	t.Cleanup(ts.Close)

	cfg := &config.Config{
		Server: config.Server{
			MaxBodyBytes:            1 << 20,
			MaxResponseBytes:        1 << 20,
			MaxInflightRequests:     4,
			MaxBufferedRequestBytes: 1 << 20,
			StreamIdleTimeout:       config.Duration(2 * time.Second),
			StreamWriteTimeout:      config.Duration(2 * time.Second),
		},
		Responses: config.Responses{
			AffinityTTL:        config.Duration(time.Hour),
			MaxAffinityEntries: 100,
		},
		Backends: map[string]config.Backend{
			"b1": {
				BaseURL:           ts.URL,
				UpstreamModel:     "Up/Model",
				MaxConcurrency:    2,
				QueueSize:         2,
				QueueTimeout:      config.Duration(200 * time.Millisecond),
				ConnectTimeout:    config.Duration(time.Second),
				HeaderTimeout:     config.Duration(2 * time.Second),
				RequestTimeout:    config.Duration(5 * time.Second),
				StreamIdleTimeout: config.Duration(2 * time.Second),
			},
		},
		Models: map[string]config.Model{
			"model-a": {Type: "generation", Strategy: "single", Backends: []config.BackendRef{{Name: "b1", Weight: 1}}},
			"model-b": {Type: "generation", Strategy: "single", Backends: []config.BackendRef{{Name: "b1", Weight: 1}}},
		},
	}
	if mod != nil {
		mod(cfg)
	}

	// Users store with two keys: one for model-a only, one wildcard.
	pepper := []byte("httpapi-test-pepper-16b")
	k1, id1, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	k2, id2, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	uf := &auth.UsersFile{Version: 1, Keys: []auth.Key{
		{ID: id1, Name: "a", SecretHash: auth.FormatHashValue(auth.Hash(pepper, k1)), Enabled: true, Models: []string{"model-a"}, Limits: k1lim},
		{ID: id2, Name: "b", SecretHash: auth.FormatHashValue(auth.Hash(pepper, k2)), Enabled: true, Models: []string{"*"}},
	}}
	if usersFn != nil {
		usersFn(uf)
	}
	store, err := auth.NewStore(uf, pepper)
	if err != nil {
		t.Fatal(err)
	}

	client, err := backend.New(backend.Options{
		Name: "b1", Cfg: cfg.Backends["b1"], Network: backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	router, err := routing.New(cfg, func(name string) int {
		if name == "b1" {
			return client.Inflight()
		}
		return 0
	})
	if err != nil {
		t.Fatal(err)
	}
	prox, err := proxy.New(cfg, router, map[string]*backend.Client{"b1": client}, log, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, log, store, router, prox)
	s.SetReady(true)

	return &env{srv: s, key: k1, key2: k2}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 10}))
}

func (e *env) do(t *testing.T, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return e.doFrom(t, method, path, key, body, "192.0.2.1:1234")
}

// doFrom is do with a caller-chosen socket peer, which is how T-M4
// exercises per-source pre-auth limiting through the real HTTP stack.
func (e *env) doFrom(t *testing.T, method, path, key, body, remote string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = remote
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	switch key {
	case "bearer":
		r.Header.Set("Authorization", "Bearer "+e.key)
	case "bearer2":
		r.Header.Set("Authorization", "Bearer "+e.key2)
	case "xkey":
		r.Header.Set("X-Api-Key", e.key)
	case "badbearer":
		r.Header.Set("Authorization", "Bearer mtk_invalid_0000000000000000000000000000")
	}
	w := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(w, r)
	return w
}

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}, nil, auth.KeyLimits{})
	// No auth needed.
	w := e.do(t, http.MethodGet, "/healthz", "", "")
	if w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("healthz: %d %q", w.Code, w.Body.String())
	}
	w = e.do(t, http.MethodGet, "/readyz", "", "")
	if w.Code != 200 || w.Body.String() != "ready" {
		t.Fatalf("readyz: %d %q", w.Code, w.Body.String())
	}
	if rid := w.Header().Get("X-Request-ID"); !strings.HasPrefix(rid, "req_") {
		t.Fatalf("request id = %q", rid)
	}
}

// TestStreamFlushesThroughWrapper proves the route layer's
// committedWriter forwards http.Flusher (X4 defence-in-depth wrapper
// must not break SSE streaming or the health endpoints, which call
// Flush). Before the fix, /readyz panicked on the unguarded
// w.(http.Flusher) and streaming through httpapi never flushed.
func TestStreamFlushesThroughWrapper(t *testing.T) {
	t.Parallel()
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"id\":\"1\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}, nil, auth.KeyLimits{})

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","stream":true}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+e.key)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, r)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !rec.Flushed {
		t.Fatalf("streaming response never flushed through the committedWriter wrapper")
	}
	if !strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("stream body = %q", rec.Body.String())
	}
	// The wrapper's recover must not have tripped on the way out.
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type = %q", rec.Header().Get("Content-Type"))
	}
}

// TestNonStreamingWriteDeadlineBounded is the T-X9 integration check: a
// client that reads the response headers then stops reading a
// non-streaming completion must have its write bounded by
// stream_write_timeout (PLAN §9.1). The stalled write fills the socket
// buffer, the deadline fires, the handler returns, and net/http closes
// the connection with a truncated body instead of holding the goroutine
// and connection forever. Service must recover once the stalled client
// is gone.
func TestNonStreamingWriteDeadlineBounded(t *testing.T) {
	t.Parallel()
	// Large enough that a fully-delivered body is unambiguous.
	const big = 32 << 20
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(make([]byte, big))
	}, func(c *config.Config) {
		c.Server.MaxResponseBytes = 64 << 20
		c.Server.StreamWriteTimeout = config.Duration(200 * time.Millisecond)
	}, auth.KeyLimits{})

	ts := httptest.NewUnstartedServer(e.srv.Handler())
	ts.Start()
	defer ts.Close()

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	body := `{"model":"model-a","messages":[]}`
	fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: test\r\nContent-Type: application/json\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		e.key, len(body), body)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response headers: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Stop reading well past stream_write_timeout so the write deadline
	// fires while the socket buffer is full. Then drain everything the
	// server managed to deliver. If the write is bounded, the handler
	// gave up at the deadline and closed the connection: the client
	// receives only what was already buffered (a small fraction of
	// `big`). If the write is unbounded, the server resumes once the
	// client reads again and the full body is delivered.
	time.Sleep(500 * time.Millisecond)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _ := io.Copy(io.Discard, conn)
	// The bounded case delivers only the bytes that fit in the socket
	// buffers before the deadline fired (a few MB here); the unbounded
	// case delivers the whole body. Half the body size cleanly
	// separates the two regardless of kernel buffer autotuning.
	if n >= big/2 {
		t.Fatalf("stalled non-streaming write was not bounded: received %d of %d-byte body", n, big)
	}

	// Service must recover: a normal request after the stall succeeds,
	// proving resources were reclaimed rather than held forever.
	if w := e.do(t, http.MethodPost, "/v1/chat/completions", "bearer", `{"model":"model-a"}`); w.Code != 200 {
		t.Fatalf("post-stall request: %d %s", w.Code, w.Body.String())
	}
}

func TestAuthMatrix(t *testing.T) {
	t.Parallel()
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1"}`))
	}, nil, auth.KeyLimits{})
	cases := []struct {
		name string
		key  string // "", "bearer", "bearer2", "xkey"
		code int
	}{
		{"no credentials", "", 401},
		{"unknown key", "unknown", 401},
		{"malformed header", "basic", 401},
		{"x-api-key ok", "xkey", 200},
		{"bearer ok", "bearer", 200},
		{"mismatched dual headers", "mismatch", 401},
		{"query param ignored", "query", 200},
	}
	for _, tc := range cases {
		w := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?api_key=should-be-ignored",
			strings.NewReader(`{"model":"model-a"}`))
		w.Header.Set("Content-Type", "application/json")
		switch tc.key {
		case "bearer":
			w.Header.Set("Authorization", "Bearer "+e.key)
		case "bearer2":
			w.Header.Set("Authorization", "Bearer "+e.key2)
		case "xkey":
			w.Header.Set("X-Api-Key", e.key)
		case "unknown":
			w.Header.Set("Authorization", "Bearer mtk_9X9X9X_unknownkeyunknownkeyunknownk")
		case "basic":
			w.Header.Set("Authorization", "Basic abc")
		case "mismatch":
			w.Header.Set("Authorization", "Bearer "+e.key)
			w.Header.Set("X-Api-Key", e.key2)
		case "query":
			w.Header.Set("Authorization", "Bearer "+e.key)
		}
		rr := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rr, w)
		if rr.Code != tc.code {
			t.Fatalf("%s: status = %d (want %d)", tc.name, rr.Code, tc.code)
		}
	}

	// 401 body is sanitized and carries an error envelope.
	w := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rr := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rr, w)
	var env struct {
		Err struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	if rr.Code != 401 || env.Err.Type != "authentication_error" {
		t.Fatalf("401 body = %s", rr.Body.String())
	}
}

func TestClassifyAuthError(t *testing.T) {
	t.Parallel()
	// T-Q5: the operator-facing class must survive a wrapped sentinel,
	// so classification uses errors.Is, never ==.
	wrappedDisabled := fmt.Errorf("key id: %w", auth.ErrDisabled)
	wrappedExpired := fmt.Errorf("key id: %w", auth.ErrExpired)
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"wrapped disabled", wrappedDisabled, "auth_disabled"},
		{"wrapped expired", wrappedExpired, "auth_expired"},
		{"bare disabled", auth.ErrDisabled, "auth_disabled"},
		{"bare expired", auth.ErrExpired, "auth_expired"},
		{"unknown", auth.ErrUnknownKey, "auth_unknown"},
		{"unrelated", errors.New("boom"), "auth_unknown"},
	}
	for _, tc := range cases {
		if got := classifyAuthError(tc.err); got != tc.want {
			t.Errorf("%s: classifyAuthError(%v) = %q, want %q", tc.name, tc.err, got, tc.want)
		}
	}
}

func TestAuthDisabledExpiredClassified(t *testing.T) {
	t.Parallel()
	// T-Q5 (T-T4): a disabled and an expired key are 401 like any other
	// bad key, but are logged under their own class so the operator can
	// distinguish them from a bogus-token flood.
	var buf bytes.Buffer
	capLog := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	dk, did, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ek, eid, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	e := buildEnvWithUsers(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}, func(cfg *config.Config) {
		cfg.Limits.AuthFailureLogRate = 1
		cfg.Limits.PreauthRequestsPerSecond = 1000
		cfg.Limits.PreauthBurst = 1000
		cfg.Limits.GlobalRequestsPerSecond = 1000
		cfg.Limits.GlobalBurst = 1000
	}, auth.KeyLimits{}, capLog, func(uf *auth.UsersFile) {
		past := time.Now().Add(-time.Hour)
		uf.Keys = append(uf.Keys,
			auth.Key{ID: did, Name: "disabled", SecretHash: auth.FormatHashValue(auth.Hash([]byte("httpapi-test-pepper-16b"), dk)), Enabled: false, Models: []string{"*"}},
			auth.Key{ID: eid, Name: "expired", SecretHash: auth.FormatHashValue(auth.Hash([]byte("httpapi-test-pepper-16b"), ek)), Enabled: true, ExpiresAt: &past, Models: []string{"*"}},
		)
	})

	cases := []struct {
		name, key, class string
	}{
		{"disabled", dk, "auth_disabled"},
		{"expired", ek, "auth_expired"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-a"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+tc.key)
		rr := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rr, r)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", tc.name, rr.Code)
		}
		if !strings.Contains(buf.String(), "class="+tc.class) {
			t.Fatalf("%s: no %s log line; captured log:\n%s", tc.name, tc.class, buf.String())
		}
	}
	// A valid key still works and adds no rejection line.
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-a"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+e.key)
	rr := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rr, r)
	if rr.Code != 200 {
		t.Fatalf("valid key: status = %d, want 200", rr.Code)
	}
	if n := strings.Count(buf.String(), "auth rejected"); n != 2 {
		t.Fatalf("auth-rejection lines = %d, want 2 (disabled + expired); log:\n%s", n, buf.String())
	}
}

func TestDuplicateXApiKeyRejected(t *testing.T) {
	t.Parallel()
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1"}`))
	}, nil, auth.KeyLimits{})

	// A single X-Api-Key authenticates (control).
	w := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-a"}`))
	w.Header.Set("Content-Type", "application/json")
	w.Header.Set("X-Api-Key", e.key)
	rr := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rr, w)
	if rr.Code != 200 {
		t.Fatalf("single X-Api-Key: status = %d (want 200)", rr.Code)
	}

	// Two X-Api-Key headers are ambiguous and must be rejected
	// (consistent with duplicate Authorization), never first-wins (T-L1).
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-a"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Add("X-Api-Key", e.key)
	r.Header.Add("X-Api-Key", e.key2)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, r)
	if rec.Code != 401 {
		t.Fatalf("duplicate X-Api-Key: status = %d (want 401)", rec.Code)
	}
}

// TestAllowHeadersConsistent asserts the 405 Allow header agrees with
// the route method guards (T-L2): GET /v1/responses must not advertise
// POST, POST /v1/models must not advertise GET, etc.
func TestAllowHeadersConsistent(t *testing.T) {
	t.Parallel()
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1"}`))
	}, nil, auth.KeyLimits{})
	cases := []struct {
		method, path, allow string
	}{
		{http.MethodPost, "/v1/models", "GET"},
		{http.MethodGet, "/v1/responses", "POST"},
		{http.MethodGet, "/v1/chat/completions", "POST"},
		{http.MethodGet, "/v1/completions", "POST"},
		{http.MethodGet, "/v1/embeddings", "POST"},
		{http.MethodPost, "/v1/responses/xyz", "GET"},
		{http.MethodPut, "/v1/responses/xyz/cancel", "POST"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+e.key)
		rr := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rr, r)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: status = %d (want 405)", tc.method, tc.path, rr.Code)
		}
		if got := rr.Header().Get("Allow"); got != tc.allow {
			t.Fatalf("%s %s: Allow = %q (want %q)", tc.method, tc.path, got, tc.allow)
		}
	}
}

func TestModelsACLFiltering(t *testing.T) {
	t.Parallel()
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}, nil, auth.KeyLimits{})
	// model-a key sees only model-a.
	w := e.do(t, http.MethodGet, "/v1/models", "bearer", "")
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Data) != 1 || list.Data[0].ID != "model-a" {
		t.Fatalf("models = %s", w.Body.String())
	}
	// Wildcard key sees both.
	w = e.do(t, http.MethodGet, "/v1/models", "bearer2", "")
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Data) != 2 {
		t.Fatalf("models = %s", w.Body.String())
	}
}

func TestNoCatchAllRoute(t *testing.T) {
	t.Parallel()
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hacked":true}`))
	}, nil, auth.KeyLimits{})
	// Backend admin paths MUST be 404, never proxied (PLAN §11.3).
	paths := []string{
		"/metrics",
		"/start_profile",
		"/stop_profile",
		"/v1/load_lora_adapter",
		"/docs",
		"/openapi.json",
		"/v1/internal",
		"/v1/models/extra",
	}
	for _, p := range paths {
		w := e.do(t, http.MethodGet, p, "bearer2", "")
		if w.Code != 404 {
			t.Fatalf("%s: status = %d body = %s (want 404)", p, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "hacked") {
			t.Fatalf("%s: backend content leaked", p)
		}
	}
	// Inference path with wrong method is 405, not proxied.
	w := e.do(t, http.MethodGet, "/v1/chat/completions", "bearer2", "")
	if w.Code != 405 {
		t.Fatalf("GET completions: status = %d (want 405)", w.Code)
	}
	// 404 envelope is OpenAI-shaped.
	if !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("405 body = %s", w.Body.String())
	}
}

func TestFullChatFlow(t *testing.T) {
	t.Parallel()
	var sawModel string
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &env)
		sawModel = env.Model
		_, _ = w.Write([]byte(`{"id":"chatcmpl-9"}`))
	}, nil, auth.KeyLimits{})
	w := e.do(t, http.MethodPost, "/v1/chat/completions", "bearer", `{"model":"model-a","messages":[]}`)
	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "chatcmpl-9") {
		t.Fatalf("body = %s", w.Body.String())
	}
	if sawModel != "Up/Model" {
		t.Fatalf("upstream model = %q (want Up/Model)", sawModel)
	}
	if rid := w.Header().Get("X-Request-ID"); !strings.HasPrefix(rid, "req_") {
		t.Fatalf("request id = %q", rid)
	}

	// A key not allowed model-b must get 404 on both models and bodies.
	w = e.do(t, http.MethodPost, "/v1/chat/completions", "bearer", `{"model":"model-b"}`)
	if w.Code != 404 || !strings.Contains(w.Body.String(), "model_not_found_or_not_allowed") {
		t.Fatalf("acl: %d %s", w.Code, w.Body.String())
	}
}

// TestResponsesCrossKeyIsolation is the PLAN §21.1 acceptance test
// through the real HTTP surface: a response is bound to the key that
// created it. A different key must not be able to retrieve or cancel it
// even if it knows the ID and only one backend exists.
func TestResponsesCrossKeyIsolation(t *testing.T) {
	t.Parallel()
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		// POST create returns a response owned by the creating key.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_OWNED","object":"response"}`))
	}, nil, auth.KeyLimits{})

	// Key A (model-a) creates a response.
	rec := e.do(t, http.MethodPost, "/v1/responses", "bearer", `{"model":"model-a","input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("create status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Key B (wildcard) must not retrieve key A's response.
	rec = e.do(t, http.MethodGet, "/v1/responses/resp_OWNED", "bearer2", "")
	if rec.Code != 404 {
		t.Fatalf("cross-key retrieve status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	// Key B must not cancel key A's response either.
	rec = e.do(t, http.MethodPost, "/v1/responses/resp_OWNED/cancel", "bearer2", "")
	if rec.Code != 404 {
		t.Fatalf("cross-key cancel status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	// The owning key can still retrieve it.
	rec = e.do(t, http.MethodGet, "/v1/responses/resp_OWNED", "bearer", "")
	if rec.Code != 200 {
		t.Fatalf("owner retrieve status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestResponsesDotSegmentRejected(t *testing.T) {
	t.Parallel()
	// T-M2: a dot/.. segment in a response-ID path must be rejected and
	// must never reach the backend verbatim. The backend records every
	// path it sees so the test can prove nothing was forwarded.
	var mu sync.Mutex
	var paths []string
	createID := "resp_x"
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%q,"object":"response"}`, createID)
	}, nil, auth.KeyLimits{})

	// Register affinity entries that dot-segment IDs would need to reach
	// the backend. Without them the proxy would fail closed on the
	// affinity miss and the dot-segment rejection would be untestable.
	for _, id := range []string{"..", "."} {
		createID = id
		rec := e.do(t, http.MethodPost, "/v1/responses", "bearer", `{"model":"model-a","input":"hi"}`)
		if rec.Code != 200 {
			t.Fatalf("create (id=%q) status = %d, want 200 (body=%s)", id, rec.Code, rec.Body.String())
		}
	}

	// dot-segment forms: literal .., percent-encoded %2e%2e, a lone .,
	// and an embedded dot.
	for _, path := range []string{
		"/v1/responses/../cancel",
		"/v1/responses/%2e%2e/cancel",
		"/v1/responses/./cancel",
		"/v1/responses/..",
		"/v1/responses/.",
		"/v1/responses/resp.123",
		"/v1/responses/a/../b",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := e.do(t, method, path, "bearer", "")
			if rec.Code != 404 {
				t.Fatalf("%s %s: status = %d, want 404 (body=%s)", method, path, rec.Code, rec.Body.String())
			}
		}
	}

	// The backend must not have seen any of the malicious paths.
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		for _, bad := range []string{"/../", "/./", "..", "/."} {
			if strings.Contains(p, bad) {
				t.Fatalf("dot segment reached backend: %s (all=%v)", p, paths)
			}
		}
	}
}

func TestResponsesSingleSegmentStillRoutes(t *testing.T) {
	t.Parallel()
	// T-M2: allowed single-segment IDs keep working after the `.`
	// character was removed from the response-ID alphabet.
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_x","object":"response"}`))
	}, nil, auth.KeyLimits{})

	// Create registers the response in the key's affinity so retrieve and
	// cancel can route.
	rec := e.do(t, http.MethodPost, "/v1/responses", "bearer", `{"model":"model-a","input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("create status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	rec = e.do(t, http.MethodGet, "/v1/responses/resp_x", "bearer", "")
	if rec.Code != 200 {
		t.Fatalf("retrieve status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	rec = e.do(t, http.MethodPost, "/v1/responses/resp_x/cancel", "bearer", "")
	if rec.Code != 200 {
		t.Fatalf("cancel status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestPreauthSourceFloodDoesNotDrainGlobal(t *testing.T) {
	t.Parallel()
	// T-M4: a host flooding with bogus bearer tokens must be throttled
	// by the per-source pre-auth limiter without draining the shared
	// global bucket, so a legitimate key from another source keeps
	// working. The global bucket is tiny (burst 5) so a flood would
	// exhaust it pre-fix; the preauth bucket is burst 1 so the flooding
	// source is cut off after its first attempt.
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion"}`))
	}, func(cfg *config.Config) {
		cfg.Limits.GlobalRequestsPerSecond = 1
		cfg.Limits.GlobalBurst = 5
		cfg.Limits.PreauthRequestsPerSecond = 1
		cfg.Limits.PreauthBurst = 1
	}, auth.KeyLimits{})

	// The flooding host blasts bogus bearer tokens.
	var flood429s, flood401s int
	for i := 0; i < 10; i++ {
		rec := e.doFrom(t, http.MethodPost, "/v1/chat/completions", "badbearer", `{"model":"model-a"}`, "10.0.0.1:1234")
		switch rec.Code {
		case http.StatusUnauthorized:
			flood401s++
		case http.StatusTooManyRequests:
			flood429s++
		default:
			t.Fatalf("flood attempt: status = %d, want 401 or 429 (body=%s)", rec.Code, rec.Body.String())
		}
	}
	// The flooder must actually be throttled, not just 401'd forever.
	if flood429s == 0 {
		t.Fatal("flooding host was never pre-auth throttled")
	}

	// A legitimate key from a different source must not be 429'd: the
	// shared global bucket still has tokens because the flood never
	// reached it.
	rec := e.doFrom(t, http.MethodPost, "/v1/chat/completions", "bearer", `{"model":"model-a","input":"hi"}`, "10.0.0.2:1234")
	if rec.Code != 200 {
		t.Fatalf("legit key status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestAuthFailureLogBounded(t *testing.T) {
	t.Parallel()
	// T-M4 (PLAN §33): during a bogus-token flood the invalid-auth warn
	// output must be bounded, not one line per token. The sampler allows
	// a small burst then counts suppresseds on subsequent lines.
	var buf bytes.Buffer
	capLog := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	e := buildEnvWithLog(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion"}`))
	}, func(cfg *config.Config) {
		cfg.Limits.AuthFailureLogRate = 1
		cfg.Limits.PreauthRequestsPerSecond = 1000
		cfg.Limits.PreauthBurst = 1000
		cfg.Limits.GlobalRequestsPerSecond = 1000
		cfg.Limits.GlobalBurst = 1000
	}, auth.KeyLimits{}, capLog)

	const attempts = 200
	for i := 0; i < attempts; i++ {
		rec := e.doFrom(t, http.MethodPost, "/v1/chat/completions", "badbearer", `{"model":"model-a"}`, "10.0.0.1:1234")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401 (body=%s)", i, rec.Code, rec.Body.String())
		}
	}
	lines := strings.Count(buf.String(), "auth rejected")
	if lines == 0 {
		t.Fatal("no auth-rejection warn line was ever emitted")
	}
	if lines >= attempts {
		t.Fatalf("auth-failure logging is unbounded: %d warn lines for %d attempts", lines, attempts)
	}
	// With a 1/s rate and a burst of 3, a tight flood can emit only a
	// handful of lines; allow a small margin for scheduler noise.
	if lines > 8 {
		t.Fatalf("auth-failure warn lines = %d, want bounded (~3)", lines)
	}
}

func TestInflightLimit(t *testing.T) {
	t.Parallel()
	// A second request while the first holds its slot must 503 once the
	// inflight bound (4) is exhausted. We hold slots with a blocking
	// backend.
	done := make(chan struct{})
	defer close(done)
	// Buffered for the four held requests so an arrival can never be
	// dropped by a timing race with the receiver.
	arrived := make(chan struct{}, 4)
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		// Reaching the handler means the proxy admitted the request
		// (inflight + backend slot held); record it, then block.
		arrived <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-done:
		}
		_, _ = w.Write([]byte(`{}`))
	}, func(c *config.Config) {
		// High backend concurrency so all four held requests are
		// in-flight at the backend (blocked on done) simultaneously,
		// and long timeouts so they stay held well past the poll
		// window. This isolates the proxy inflight bound.
		b := c.Backends["b1"]
		b.MaxConcurrency = 16
		b.QueueSize = 16
		b.HeaderTimeout = config.Duration(30 * time.Second)
		b.RequestTimeout = config.Duration(30 * time.Second)
		c.Backends["b1"] = b
	}, auth.KeyLimits{})
	// Hold the four inflight slots with blocking backend requests.
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-a"}`))
		req.Header.Set("Authorization", "Bearer "+e.key2)
		rr := httptest.NewRecorder()
		go func() { e.srv.Handler().ServeHTTP(rr, req) }()
	}
	// Wait until all four are admitted (deterministic: no request may
	// reach the backend while the inflight bound is full).
	by := time.Now().Add(5 * time.Second)
	for i := 0; i < 4; i++ {
		select {
		case <-arrived:
		case <-time.After(time.Until(by)):
			t.Fatalf("admitted %d < 4 by deadline", i)
		}
	}
	// The next request must be rejected at the proxy bound.
	w := e.do(t, http.MethodPost, "/v1/chat/completions", "bearer2", `{"model":"model-a"}`)
	if w.Code != 503 {
		t.Fatalf("inflight bound: status = %d (want 503)", w.Code)
	}
}

// PLAN §35 + T-X8: a key with no limits block must not be able to occupy
// every global inflight slot and starve other keys. The key store applies
// the conservative per-key concurrency default (auth.DefaultConcurrentRequests),
// so one key is capped below the global inflight bound and a sibling key is
// still served while the first is saturated.
func TestKeyDefaultConcurrencyNoStarve(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	arrived := make(chan struct{}, auth.DefaultConcurrentRequests+4)
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		// The aggressor key is identified by its passthrough User-Agent
		// (client Authorization is stripped before forwarding, PLAN §17).
		if r.UserAgent() == "tx8-aggressor" {
			arrived <- struct{}{}
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}
		_, _ = w.Write([]byte(`{}`))
	}, func(c *config.Config) {
		// Global inflight well above the per-key default, so a single
		// key could fill it if the default were not applied.
		c.Server.MaxInflightRequests = 16
		b := c.Backends["b1"]
		b.MaxConcurrency = 16
		b.QueueSize = 16
		b.QueueTimeout = config.Duration(30 * time.Second)
		c.Backends["b1"] = b
	}, auth.KeyLimits{})

	// The no-limits key holds exactly DefaultConcurrentRequests in flight.
	for i := 0; i < auth.DefaultConcurrentRequests; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-a"}`))
		req.Header.Set("Authorization", "Bearer "+e.key)
		req.Header.Set("User-Agent", "tx8-aggressor")
		rr := httptest.NewRecorder()
		go func() { e.srv.Handler().ServeHTTP(rr, req) }()
	}
	by := time.Now().Add(5 * time.Second)
	for i := 0; i < auth.DefaultConcurrentRequests; i++ {
		select {
		case <-arrived:
		case <-time.After(time.Until(by)):
			t.Fatalf("admitted %d < %d by deadline", i, auth.DefaultConcurrentRequests)
		}
	}

	// One more from the same key is rejected at its per-key concurrency
	// bound (429), proving the default was applied.
	w := e.do(t, http.MethodPost, "/v1/chat/completions", "bearer", `{"model":"model-a"}`)
	if w.Code != 429 {
		t.Fatalf("no-limits key beyond default: %d (want 429)", w.Code)
	}

	// A sibling key is still served while the first is saturated.
	w = e.do(t, http.MethodPost, "/v1/chat/completions", "bearer2", `{"model":"model-a"}`)
	if w.Code != 200 {
		t.Fatalf("sibling key starved: %d (want 200)", w.Code)
	}
}

// PLAN §34: global request-rate exhaustion returns 429 with a
// Retry-After, before per-key limits are even consulted.
func TestGlobalRateLimit429(t *testing.T) {
	t.Parallel()
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}, func(c *config.Config) {
		c.Limits.GlobalRequestsPerSecond = 0.5
		c.Limits.GlobalBurst = 1
	}, auth.KeyLimits{})
	w := e.do(t, http.MethodPost, "/v1/chat/completions", "bearer", `{"model":"model-a"}`)
	if w.Code != 200 {
		t.Fatalf("first: %d", w.Code)
	}
	w = e.do(t, http.MethodPost, "/v1/chat/completions", "bearer", `{"model":"model-a"}`)
	if w.Code != 429 {
		t.Fatalf("second: %d body=%s (want 429)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "too_many_requests") {
		t.Fatalf("body = %s", w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Fatal("Retry-After header missing on 429")
	}
}

// PLAN §34: a per-key RPS limit 429s that key while sibling keys are
// unaffected; the response carries a Retry-After.
func TestKeyRateLimit429(t *testing.T) {
	t.Parallel()
	behaviour := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}
	e := buildEnv(t, behaviour, nil, auth.KeyLimits{RequestsPerSecond: 1, Burst: 1})
	w := e.do(t, http.MethodPost, "/v1/chat/completions", "bearer", `{"model":"model-a"}`)
	if w.Code != 200 {
		t.Fatalf("first: %d", w.Code)
	}
	w = e.do(t, http.MethodPost, "/v1/chat/completions", "bearer", `{"model":"model-a"}`)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("second: %d retry-after=%q (want 429 + Retry-After)", w.Code, w.Header().Get("Retry-After"))
	}
	// The unlimited sibling key is unaffected by the sibling's budget.
	w = e.do(t, http.MethodPost, "/v1/chat/completions", "bearer2", `{"model":"model-a"}`)
	if w.Code != 200 {
		t.Fatalf("sibling key: %d (want 200)", w.Code)
	}
}

// PLAN §35: a key with concurrent_requests=1 cannot hold more than one
// in-flight request; a different key is unaffected.
func TestKeyConcurrencyLimit429(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	defer close(done)
	firstAdmitted := make(chan struct{}, 1)
	var holder int32 // only the first request may hold the backend
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		// Only the first request (key 1's held request) blocks; later
		// requests must be able to complete while it is held.
		if atomic.CompareAndSwapInt32(&holder, 0, 1) {
			select {
			case firstAdmitted <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
			case <-done:
			}
		}
		_, _ = w.Write([]byte(`{}`))
	}, nil, auth.KeyLimits{ConcurrentRequests: 1, RequestsPerSecond: 10, Burst: 10})

	// Key 1 holds its single slot (independent goroutine + recorder).
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-a"}`))
	req.Header.Set("Authorization", "Bearer "+e.key)
	rr := httptest.NewRecorder()
	go func() { e.srv.Handler().ServeHTTP(rr, req) }()
	select {
	case <-firstAdmitted:
	case <-time.After(3 * time.Second):
		t.Fatal("held request never admitted")
	}

	// Key 1's second request must be rejected.
	w := e.do(t, http.MethodPost, "/v1/chat/completions", "bearer", `{"model":"model-a"}`)
	if w.Code != 429 {
		t.Fatalf("key-1 second in-flight: %d (want 429)", w.Code)
	}
	// A different key is unaffected by key-1's bound.
	w = e.do(t, http.MethodPost, "/v1/chat/completions", "bearer2", `{"model":"model-a"}`)
	if w.Code != 200 {
		t.Fatalf("other key: %d (want 200)", w.Code)
	}
}
