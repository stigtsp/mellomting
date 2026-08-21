package proxy

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"mellomting/internal/accounting"
	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/routing"
)

// newAccountingProxy builds a proxy with token accounting enabled: a quota
// tracker and an on-disk JSONL writer.
func newAccountingProxy(t *testing.T, f *fakeVLLM, quota *accounting.Quota, writer *accounting.Writer) *Proxy {
	t.Helper()
	cfg := testConfig(f.server.URL)
	cfg.Accounting.Enabled = true
	cfg.Accounting.EnsureStreamUsage = func() *bool { b := true; return &b }()
	router, err := routing.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := backend.New(backend.Options{
		Name:             "b1",
		Cfg:              cfg.Backends["b1"],
		Network:          backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes,
		Log:              discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), quota, writer)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// tmpWriter creates a JSONL accounting writer backed by a temp file and
// returns the writer and its path so tests can read records back.
func tmpWriter(t *testing.T) (*accounting.Writer, string) {
	t.Helper()
	path := t.TempDir() + "/usage.jsonl"
	w, err := accounting.NewWriter(accounting.WriterConfig{
		Path:      path,
		QueueSize: 16,
		FSync:     "every",
		Log:       discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, path
}

func usageOnlyJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(
		`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"Hi"}}]}` + "\n\n" +
			`data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n" +
			`data: [DONE]` + "\n\n",
	))
}

func usageJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
}

func TestOutputCapRejectsExcess(t *testing.T) {
	f := newFakeVLLM(t, okJSON)
	cfg := testConfig(f.server.URL)
	cfg.Models["gen-1"] = config.Model{
		Type:     "generation",
		Strategy: "single",
		Policy:   config.ModelPolicy{MaxOutputTokens: 100},
		Backends: []config.BackendRef{{Name: "b1", Weight: 1}},
	}
	router, _ := routing.New(cfg, nil)
	client, _ := backend.New(backend.Options{
		Name: "b1", Cfg: cfg.Backends["b1"], Network: backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: discardLogger(),
	})
	p, _ := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), nil, nil)

	w := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","max_completion_tokens":200}`, testKey())
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "output_limit_exceeded") {
		t.Fatalf("body = %s", w.Body.String())
	}
	if len(f.lastBody) != 0 {
		t.Fatalf("backend must not be reached on cap rejection, got %s", f.lastBody)
	}
}

func TestOutputCapInjectWhenAbsent(t *testing.T) {
	f := newFakeVLLM(t, okJSON)
	cfg := testConfig(f.server.URL)
	cfg.Models["gen-1"] = config.Model{
		Type: "generation", Strategy: "single",
		Policy:   config.ModelPolicy{MaxOutputTokens: 64},
		Backends: []config.BackendRef{{Name: "b1", Weight: 1}},
	}
	router, _ := routing.New(cfg, nil)
	client, _ := backend.New(backend.Options{
		Name: "b1", Cfg: cfg.Backends["b1"], Network: backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: discardLogger(),
	})
	p, _ := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), nil, nil)

	w := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","messages":[{"role":"user","content":"hi"}]}`, testKey())
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body struct {
		MaxCompletionTokens int64 `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(f.lastBody, &body); err != nil {
		t.Fatalf("backend body: %v", err)
	}
	if body.MaxCompletionTokens != 64 {
		t.Fatalf("injected cap = %d, want 64", body.MaxCompletionTokens)
	}
}

func TestStreamUsageInjectedAndSwallowed(t *testing.T) {
	f := newFakeVLLM(t, usageOnlyJSON)
	quota := accounting.NewQuota()
	writer, path := tmpWriter(t)
	p := newAccountingProxy(t, f, quota, writer)

	w := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true,"messages":[{"role":"user","content":"hi"}]}`, testKey())
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// The synthetic usage-only chunk must NOT reach the client.
	got := w.Body.String()
	if strings.Contains(got, `"total_tokens":15`) {
		t.Fatalf("usage-only chunk leaked to client: %s", got)
	}
	if !strings.Contains(got, "Hi") || !strings.Contains(got, "[DONE]") {
		t.Fatalf("stream content missing: %s", got)
	}
	// The backend must have received stream_options.include_usage=true.
	var body struct {
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(f.lastBody, &body); err != nil {
		t.Fatalf("backend body: %v", err)
	}
	if !body.StreamOptions.IncludeUsage {
		t.Fatalf("include_usage not injected: %s", f.lastBody)
	}
	// The quota must have been settled with the exact captured usage.
	lim := accounting.WindowLimit{TokensPerHour: 10, TokensPerDay: 100}
	if ok, _ := quota.Admit("K1", lim, 0, time.Now()); ok {
		t.Fatal("expected reject: 15 tokens settled exceeds hour limit 10")
	}
	// A record must have been written with exact usage. Close drains and
	// syncs the async writer so the record is guaranteed on disk.
	_ = writer.Close()
	recs := readRecords(t, path)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if recs[0].TotalTokens != 15 || recs[0].UsageStatus != accounting.UsageExact {
		t.Fatalf("record = %+v", recs[0])
	}
}

func TestClientRequestedUsageNotSwallowed(t *testing.T) {
	f := newFakeVLLM(t, usageOnlyJSON)
	quota := accounting.NewQuota()
	writer, _ := tmpWriter(t)
	p := newAccountingProxy(t, f, quota, writer)

	// The client explicitly asked for usage, so the chunk must be relayed.
	w := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`, testKey())
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"total_tokens":15`) {
		t.Fatalf("client-requested usage chunk should be relayed: %s", w.Body.String())
	}
}

func TestQuotaAdmissionReject(t *testing.T) {
	f := newFakeVLLM(t, usageJSON)
	quota := accounting.NewQuota()
	// Pre-settle the key to exceed its hourly limit.
	quota.Settle("K1", 120, time.Now())
	writer, _ := tmpWriter(t)
	p := newAccountingProxy(t, f, quota, writer)

	key := testKey()
	key.Limits.TokensPerHour = 100
	w := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","messages":[{"role":"user","content":"hi"}]}`, key)
	if w.Code != 429 {
		t.Fatalf("status = %d, want 429: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "token_quota_exceeded") {
		t.Fatalf("body = %s", w.Body.String())
	}
	if len(f.lastBody) != 0 {
		t.Fatalf("backend must not be reached on quota rejection")
	}
}

func TestNonStreamAccountingExact(t *testing.T) {
	f := newFakeVLLM(t, usageJSON)
	quota := accounting.NewQuota()
	writer, path := tmpWriter(t)
	p := newAccountingProxy(t, f, quota, writer)

	w := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","messages":[{"role":"user","content":"hi"}]}`, testKey())
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	lim := accounting.WindowLimit{TokensPerHour: 5, TokensPerDay: 100}
	if ok, _ := quota.Admit("K1", lim, 0, time.Now()); ok {
		t.Fatal("expected reject: 10 tokens settled exceeds hour limit 5")
	}
	_ = writer.Close()
	recs := readRecords(t, path)
	if len(recs) != 1 || recs[0].TotalTokens != 10 || recs[0].UsageStatus != accounting.UsageExact {
		t.Fatalf("records = %+v", recs)
	}
}

func TestStreamUnknownUsageChargesConfiguredReservation(t *testing.T) {
	// A stream that ends without any usage chunk: quota is charged the
	// configured reservation (PLAN §39). The client's own max_completion_tokens
	// must NOT shrink the charge — here the client asks for 1 token but the
	// model cap (50) is reserved.
	f := newFakeVLLM(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"c","choices":[{"delta":{"content":"x"}}]}` + "\n\n" + `data: [DONE]` + "\n\n"))
	})
	quota := accounting.NewQuota()
	writer, path := tmpWriter(t)
	cfg := testConfig(f.server.URL)
	cfg.Models["gen-1"] = config.Model{
		Type: "generation", Strategy: "single",
		Policy:   config.ModelPolicy{MaxOutputTokens: 50},
		Backends: []config.BackendRef{{Name: "b1", Weight: 1}},
	}
	router, _ := routing.New(cfg, nil)
	client, _ := backend.New(backend.Options{
		Name: "b1", Cfg: cfg.Backends["b1"], Network: backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: discardLogger(),
	})
	p, _ := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), quota, writer)

	w := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true,"max_completion_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, testKey())
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// The configured cap (50) is reserved, not the client's 1.
	lim := accounting.WindowLimit{TokensPerHour: 40, TokensPerDay: 100}
	if ok, _ := quota.Admit("K1", lim, 0, time.Now()); ok {
		t.Fatal("expected reject: 50 reservation charged exceeds hour limit 40")
	}
	_ = writer.Close()
	recs := readRecords(t, path)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if recs[0].UsageStatus != accounting.UsageUnknown {
		t.Fatalf("usage_status = %s, want unknown", recs[0].UsageStatus)
	}
	if recs[0].ChargedTokens != 50 || recs[0].TotalTokens != 0 {
		t.Fatalf("record = %+v: want charged_tokens 50, total_tokens 0", recs[0])
	}
}

func TestStreamUnknownUsageChargesExplicitReservation(t *testing.T) {
	// accounting.unknown_usage_reservation overrides the model cap and
	// covers input and output conservatively. The charge must equal the
	// configured value, not the client's max_completion_tokens.
	f := newFakeVLLM(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"c","choices":[{"delta":{"content":"x"}}]}` + "\n\n" + `data: [DONE]` + "\n\n"))
	})
	quota := accounting.NewQuota()
	writer, path := tmpWriter(t)
	cfg := testConfig(f.server.URL)
	cfg.Accounting.UnknownUsageReservation = 40
	cfg.Models["gen-1"] = config.Model{
		Type: "generation", Strategy: "single",
		Policy:   config.ModelPolicy{MaxOutputTokens: 50},
		Backends: []config.BackendRef{{Name: "b1", Weight: 1}},
	}
	router, _ := routing.New(cfg, nil)
	client, _ := backend.New(backend.Options{
		Name: "b1", Cfg: cfg.Backends["b1"], Network: backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: discardLogger(),
	})
	p, _ := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), quota, writer)

	w := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true,"max_completion_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, testKey())
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// The configured reservation (40) is charged, not the cap (50) or the
	// client's 1.
	lim := accounting.WindowLimit{TokensPerHour: 39, TokensPerDay: 100}
	if ok, _ := quota.Admit("K1", lim, 0, time.Now()); ok {
		t.Fatal("expected reject: 40 reservation charged exceeds hour limit 39")
	}
	_ = writer.Close()
	recs := readRecords(t, path)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if recs[0].ChargedTokens != 40 || recs[0].TotalTokens != 0 {
		t.Fatalf("record = %+v: want charged_tokens 40, total_tokens 0", recs[0])
	}
}

// readRecords parses the JSONL file at path into ordered records. It is
// used after an FSync "every" writer has flushed, so all enqueued records
// are present on disk.
func readRecords(t *testing.T, path string) []accounting.Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var recs []accounting.Record
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		r, err := accounting.UnmarshalRecord(line)
		if err != nil {
			t.Fatalf("bad record line: %v: %s", err, line)
		}
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return recs
}
