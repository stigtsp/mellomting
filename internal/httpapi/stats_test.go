package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mellomting/internal/auth"
	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/proxy"
	"mellomting/internal/routing"
	"mellomting/internal/stats"
	"mellomting/internal/testsupport"
)

// refusalEnv builds a server whose backend is never reached: every
// request under test is refused before forwarding, so no listener is
// needed.
func refusalEnv(t *testing.T, lim auth.KeyLimits) (*Server, *stats.Store, string) {
	t.Helper()
	cfg := &config.Config{
		Server: config.Server{
			MaxBodyBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxInflightRequests: 4,
			MaxBufferedRequestBytes: 1 << 20,
			StreamIdleTimeout:       config.Duration(time.Second), StreamWriteTimeout: config.Duration(time.Second),
		},
		Responses: config.Responses{AffinityTTL: config.Duration(time.Hour), MaxAffinityEntries: 10},
		Backends: map[string]config.Backend{"b1": {
			BaseURL: "http://127.0.0.1:9", UpstreamModel: "m", MaxConcurrency: 1, QueueSize: 0,
			ConnectTimeout: config.Duration(100 * time.Millisecond), HeaderTimeout: config.Duration(time.Second),
			RequestTimeout: config.Duration(time.Second), StreamIdleTimeout: config.Duration(time.Second),
		}},
		Models: map[string]config.Model{"model-a": {Type: "generation", Strategy: "single", Backends: []config.BackendRef{{Name: "b1", Weight: 1}}}},
	}
	pepper := []byte("httpapi-test-pepper-16b")
	k, id, err := auth.Generate("alice")
	if err != nil {
		t.Fatal(err)
	}
	store, err := auth.NewStore(&auth.UsersFile{Version: 1, Keys: []auth.Key{
		{ID: id, Name: "alice", SecretHash: auth.FormatHashValue(auth.Hash(pepper, k)), Enabled: true, Models: []string{"*"}, Limits: lim},
	}}, pepper)
	if err != nil {
		t.Fatal(err)
	}
	log := testsupport.DiscardLogger()
	client, err := backend.New(backend.Options{Name: "b1", Cfg: cfg.Backends["b1"], Network: backend.Policy{Mode: "loopback-only"}, MaxResponseBytes: 1 << 20, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	router, err := routing.New(cfg, func(string) int { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	prox, err := proxy.New(cfg, router, map[string]*backend.Client{"b1": client}, log, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := stats.New(nil)
	s := New(cfg, log, store, router, prox)
	s.SetStats(st)
	s.SetReady(true)
	return s, st, k
}

func post(s *Server, key string) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-a"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Code
}

func keySeries(e stats.Export, name string) *stats.SeriesExport {
	for i := range e.Series {
		if e.Series[i].Dim == "key" && e.Series[i].Name == name {
			return &e.Series[i]
		}
	}
	return nil
}

func TestKeyRateRefusalRecorded(t *testing.T) {
	s, st, key := refusalEnv(t, auth.KeyLimits{RequestsPerSecond: 0.001, Burst: 1})
	post(s, key) // spends the burst; fails at the unreachable backend
	if code := post(s, key); code != http.StatusTooManyRequests {
		t.Fatalf("second request: %d, want 429", code)
	}
	k := keySeries(st.Since(0, 1<<20), "alice")
	if k == nil || k.Refusals["key_rate"] != 1 {
		t.Fatalf("key series %+v", k)
	}
}

func TestAuthRefusalRecordedUnattributed(t *testing.T) {
	s, st, _ := refusalEnv(t, auth.KeyLimits{})
	if code := post(s, "sk-wrong"); code != http.StatusUnauthorized {
		t.Fatalf("bad key: %d", code)
	}
	e := st.Since(0, 1<<20)
	if e.Global.Refusals["auth"] != 1 {
		t.Fatalf("global refusals %v", e.Global.Refusals)
	}
	for _, se := range e.Series {
		if se.Dim == "key" {
			t.Fatalf("an unauthenticated refusal was attributed to key %q", se.ID)
		}
	}
}

// The screenshot case: a key at its concurrent_requests bound is refused,
// and the refusal names the limit and the key.
func TestKeyConcurrencyRefusalRecorded(t *testing.T) {
	s, st, key := refusalEnv(t, auth.KeyLimits{ConcurrentRequests: 1})
	k := s.Store().Keys()[0]
	if _, ok := s.Limits().For(&k).AcquireConcurrency(); !ok {
		t.Fatal("could not hold the key's only slot")
	}
	if code := post(s, key); code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", code)
	}
	if ks := keySeries(st.Since(0, 1<<20), "alice"); ks == nil || ks.Refusals["key_concurrency"] != 1 {
		t.Fatalf("key series %+v", ks)
	}
}
