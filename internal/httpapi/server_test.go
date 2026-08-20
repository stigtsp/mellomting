package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
	store, err := auth.NewStore(uf, pepper)
	if err != nil {
		t.Fatal(err)
	}

	client, err := backend.New(backend.Options{
		Name: "b1", Cfg: cfg.Backends["b1"], Network: backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: testLogger(),
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
	prox, err := proxy.New(cfg, router, map[string]*backend.Client{"b1": client}, testLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, testLogger(), store, router, prox)
	s.SetReady(true)

	return &env{srv: s, key: k1, key2: k2}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 10}))
}

func (e *env) do(t *testing.T, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
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
