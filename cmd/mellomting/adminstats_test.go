package main

import (
	"testing"
	"time"

	"mellomting/internal/accounting"
	"mellomting/internal/auth"
	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/httpapi"
	"mellomting/internal/inflight"
	"mellomting/internal/proxy"
	"mellomting/internal/routing"
	"mellomting/internal/stats"
	"mellomting/internal/testsupport"
)

// statsFixture is one server ("gpu1") serving two models, and one key
// limited to two requests in flight. No listener is involved.
func statsFixture(t *testing.T) (*statsSource, *auth.Key) {
	t.Helper()
	be := config.Backend{BaseURL: "http://127.0.0.1:9", UpstreamModel: "m", MaxConcurrency: 4, QueueSize: 8,
		ConnectTimeout: config.Duration(time.Second), HeaderTimeout: config.Duration(time.Second),
		RequestTimeout: config.Duration(time.Second), StreamIdleTimeout: config.Duration(time.Second)}
	a, b := be, be
	a.Server, b.Server = "gpu1", "gpu1"
	cfg := &config.Config{
		Server: config.Server{MaxInflightRequests: 64, MaxBodyBytes: 1 << 20, MaxResponseBytes: 1 << 20,
			MaxBufferedRequestBytes: 1 << 20, StreamIdleTimeout: config.Duration(time.Second), StreamWriteTimeout: config.Duration(time.Second)},
		Responses: config.Responses{AffinityTTL: config.Duration(time.Hour), MaxAffinityEntries: 10},
		Backends:  map[string]config.Backend{"auto-gpu1-a": a, "auto-gpu1-b": b},
		Models: map[string]config.Model{
			"chat":  {Type: "generation", Strategy: "single", Backends: []config.BackendRef{{Name: "auto-gpu1-a", Weight: 1}}},
			"embed": {Type: "embedding", Strategy: "single", Backends: []config.BackendRef{{Name: "auto-gpu1-b", Weight: 1}}},
		},
	}
	log := testsupport.DiscardLogger()
	clients, err := newBackendClients(cfg, backend.Policy{Mode: "loopback-only"}, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	router, err := routing.New(cfg, func(string) int { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	pepper := []byte("0123456789abcdef")
	raw, id, err := auth.Generate("alice")
	if err != nil {
		t.Fatal(err)
	}
	key := auth.Key{ID: id, Name: "alice", SecretHash: auth.FormatHashValue(auth.Hash(pepper, raw)), Enabled: true,
		Models: []string{"*"}, Limits: auth.KeyLimits{ConcurrentRequests: 2, TokensPerHour: 1000}}
	store, err := auth.NewStore(&auth.UsersFile{Version: 1, Keys: []auth.Key{key}}, pepper)
	if err != nil {
		t.Fatal(err)
	}
	quota := accounting.NewQuota()
	live := inflight.New()
	prox, err := proxy.New(cfg, router, clients, log, quota, nil, live)
	if err != nil {
		t.Fatal(err)
	}
	api := httpapi.New(cfg, log, store, router, prox)
	return &statsSource{cfg: cfg, started: time.Now(), store: stats.New(nil), api: api, clients: clients,
		router: router, quota: quota, live: live}, &key
}

func TestStatsSourceAssembles(t *testing.T) {
	src, key := statsFixture(t)
	src.quota.Settle(key.ID, 100, time.Now())
	ks := src.api.Limits().For(key)
	for range 2 {
		if _, ok := ks.AcquireConcurrency(); !ok {
			t.Fatal("acquire")
		}
	}
	src.router.RecordFailure("auto-gpu1-a")

	st := src.Stats(0)
	if len(st.Keys) != 1 || st.Keys[0].Inflight != 2 || st.Keys[0].ConcurrentRequests != 2 ||
		st.Keys[0].HourUsed != 100 || st.Keys[0].TokensPerHour != 1000 || st.Keys[0].HourResetsMillis == 0 {
		t.Fatalf("keys %+v", st.Keys)
	}
	if len(st.Servers) != 1 {
		t.Fatalf("servers %+v", st.Servers)
	}
	s := st.Servers[0]
	if s.Name != "gpu1" || s.Kind != "configured" || len(s.Models) != 2 || s.MaxConcurrency != 4 || s.QueueSize != 8 {
		t.Fatalf("server %+v", s)
	}
	if len(s.Health) != 1 || s.Health[0].Model != "chat" || s.Health[0].CooldownUntilMillis == 0 {
		t.Fatalf("health %+v", s.Health)
	}
	if st.Global.MaxInflight != 64 || st.Agents != nil {
		t.Fatalf("global %+v agents %v", st.Global, st.Agents)
	}

	g := src.gauges()
	if kg := g.Keys[key.ID]; kg.Inflight != 2 || !kg.AtLimit || kg.Name != "alice" {
		t.Fatalf("key gauge %+v", kg)
	}
	if _, ok := g.Servers["gpu1"]; !ok {
		t.Fatalf("server gauges %+v", g.Servers)
	}
}
