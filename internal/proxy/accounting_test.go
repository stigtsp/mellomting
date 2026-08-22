package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"mellomting/internal/accounting"
	"mellomting/internal/auth"
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
	p, err := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), quota, writer, false)
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

// streamWithTrailer serves a content event followed by a usage-only chunk
// whose terminator is exactly trailer: the T-X13 shape where the final
// event lacks a terminating blank line.
func streamWithTrailer(trailer string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		content := `data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"Hi"}}]}` + "\n\n"
		usage := `data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
		_, _ = w.Write([]byte(content + usage + trailer))
	}
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
	p, _ := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), nil, nil, false)

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
	p, _ := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), nil, nil, false)

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

// FIX-04/N10: with accounting.enabled: false but a per-key token quota in
// effect, streams must settle real usage, not the whole output cap. The
// daemon drives ensureUsage from "a quota is configured" rather than from
// accounting.enabled, so include_usage is injected even when the JSONL is
// disabled and the quota is charged the exact captured usage.
func TestAccountingOffWithQuotaSettlesExactStreamUsage(t *testing.T) {
	f := newFakeVLLM(t, usageOnlyJSON)
	quota := accounting.NewQuota()
	cfg := testConfig(f.server.URL)
	cfg.Accounting.Enabled = false
	cfg.Accounting.EnsureStreamUsage = func() *bool { b := true; return &b }()
	// A large cap mirrors the review scenario: tiny streams must not
	// exhaust a large hourly budget by charging the whole cap each.
	cfg.Models["gen-1"] = config.Model{
		Type: "generation", Strategy: "single",
		Policy:   config.ModelPolicy{MaxOutputTokens: 100000},
		Backends: []config.BackendRef{{Name: "b1", Weight: 1}},
	}
	router, err := routing.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := backend.New(backend.Options{
		Name: "b1", Cfg: cfg.Backends["b1"], Network: backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// quotaConfigured=true: accounting is off but quotas are active.
	p, err := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), quota, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	w := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true,"messages":[{"role":"user","content":"hi"}]}`, testKey())
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	// include_usage must be injected even though accounting is disabled.
	var body struct {
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(f.lastBody, &body); err != nil {
		t.Fatalf("backend body: %v", err)
	}
	if !body.StreamOptions.IncludeUsage {
		t.Fatalf("include_usage not injected with accounting disabled but quota active: %s", f.lastBody)
	}
	// The quota must be charged the exact 15 captured tokens, not the
	// 100000 output cap: a 15-token window still admits, a 14-token one
	// does not.
	now := time.Now()
	if ok, _ := quota.Admit("K1", accounting.WindowLimit{TokensPerHour: 15, TokensPerDay: 100}, 0, now); !ok {
		t.Fatal("expected admit: exactly 15 tokens settled, not the 100000 output cap")
	}
	if ok, _ := quota.Admit("K1", accounting.WindowLimit{TokensPerHour: 14, TokensPerDay: 100}, 0, now); ok {
		t.Fatal("expected reject: 15 tokens settled already")
	}
}

// TestProxyQuota429RetryAfter is the T-Q12 check on the token-quota 429:
// it must carry a Retry-After just like the httpapi rate-limit 429s.
func TestProxyQuota429RetryAfter(t *testing.T) {
	f := newFakeVLLM(t, okJSON)
	quota := accounting.NewQuota()
	writer, _ := tmpWriter(t)
	p := newAccountingProxy(t, f, quota, writer)

	key := &auth.Key{
		ID: "K1", Name: "t", Enabled: true, Models: []string{"gen-1"},
		Limits: auth.KeyLimits{TokensPerHour: 10, TokensPerDay: 100},
	}
	quota.Settle("K1", 11, time.Now())

	w := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","messages":[{"role":"user","content":"hi"}]}`, key)
	if w.Code != 429 {
		t.Fatalf("status = %d (want 429)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "token_quota_exceeded") {
		t.Fatalf("body = %s", w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Fatal("quota 429 Retry-After header missing")
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

// FIX-08: an empty-choices frame carrying "usage": null plus provider
// metadata is not a usage-only chunk and must be re-emitted verbatim
// (PLAN §24) even when the proxy injected include_usage; only a real
// injected usage object (no choices) is swallowed.
func TestUsageNullFrameReEmitted(t *testing.T) {
	f := newFakeVLLM(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"id":"c","choices":[{"delta":{"content":"Hi"}}]}` + "\n\n" +
				`data: {"id":"c","choices":[],"usage":null,"metadata":{"provider":"upstream"}}` + "\n\n" +
				`data: {"id":"c","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n" +
				`data: [DONE]` + "\n\n",
		))
	})
	quota := accounting.NewQuota()
	writer, _ := tmpWriter(t)
	p := newAccountingProxy(t, f, quota, writer)

	w := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true,"messages":[{"role":"user","content":"hi"}]}`, testKey())
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	got := w.Body.String()
	if !strings.Contains(got, `"usage":null`) || !strings.Contains(got, `"provider":"upstream"`) {
		t.Fatalf("usage:null frame must be re-emitted verbatim:\n%s", got)
	}
	if strings.Contains(got, `"total_tokens":15`) {
		t.Fatalf("injected usage chunk must be swallowed:\n%s", got)
	}
	if !strings.Contains(got, "Hi") || !strings.Contains(got, "[DONE]") {
		t.Fatalf("stream content missing:\n%s", got)
	}
}

// FIX-09: injectedUsage must be true only when the proxy actually wrote
// include_usage. An explicit client value (true or false) is preserved
// upstream and never counts as injected, so the pump does not swallow a
// real backend usage-only chunk the client did not ask the proxy to add.
func TestInjectedUsageOnlyWhenAbsent(t *testing.T) {
	prep := func(body string) bool {
		o := operation{endpoint: "chat.completions", generative: true}
		_, _, inj, err := prepareOutbound([]byte(body), o, 100000, true, true, 0)
		if err != nil {
			t.Fatalf("prepareOutbound: %v", err)
		}
		return inj
	}
	if !prep(`{"model":"m","stream":true,"messages":[]}`) {
		t.Fatal("omit stream_options: injectedUsage should be true")
	}
	if prep(`{"model":"m","stream":true,"stream_options":{"include_usage":false},"messages":[]}`) {
		t.Fatal("explicit include_usage:false: injectedUsage should be false")
	}
	if prep(`{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[]}`) {
		t.Fatal("explicit include_usage:true: injectedUsage should be false")
	}

	f := newFakeVLLM(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"id":"c","choices":[{"delta":{"content":"Hi"}}]}` + "\n\n" +
				`data: {"id":"c","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n" +
				`data: [DONE]` + "\n\n",
		))
	})
	quota := accounting.NewQuota()
	writer, _ := tmpWriter(t)
	p := newAccountingProxy(t, f, quota, writer)

	w := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true,"stream_options":{"include_usage":false},"messages":[{"role":"user","content":"hi"}]}`, testKey())
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var upstream struct {
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(f.lastBody, &upstream); err != nil {
		t.Fatalf("backend body: %v", err)
	}
	if upstream.StreamOptions.IncludeUsage {
		t.Fatalf("client's include_usage:false must be preserved upstream: %s", f.lastBody)
	}
	got := w.Body.String()
	if !strings.Contains(got, `"total_tokens":15`) {
		t.Fatalf("usage-only chunk must be relayed when the client set include_usage:false:\n%s", got)
	}
	if !strings.Contains(got, "Hi") || !strings.Contains(got, "[DONE]") {
		t.Fatalf("stream content missing:\n%s", got)
	}
}

// TestStreamFinalEventNoBlankLineDelivered is the T-X13 regression test:
// a stream whose final event lacks a terminating blank line (trailer="",
// "\n" or "\r") must still deliver the final content event to the client
// and settle accounting with the exact usage from the final usage-only
// chunk, not the fallback reservation.
func TestStreamFinalEventNoBlankLineDelivered(t *testing.T) {
	for _, trailer := range []string{"", "\n", "\r"} {
		trailer := trailer
		t.Run(fmt.Sprintf("trailer=%q", trailer), func(t *testing.T) {
			f := newFakeVLLM(t, streamWithTrailer(trailer))
			quota := accounting.NewQuota()
			writer, path := tmpWriter(t)
			p := newAccountingProxy(t, f, quota, writer)

			w := run(t, p, http.MethodPost, "/v1/chat/completions",
				`{"model":"gen-1","stream":true,"messages":[{"role":"user","content":"hi"}]}`, testKey())
			if w.Code != 200 {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			// The final content event must reach the client.
			if !strings.Contains(w.Body.String(), "Hi") {
				t.Fatalf("final content event missing: %s", w.Body.String())
			}
			// The usage-only chunk was injected on the client's behalf, so
			// it must not be relayed, but its usage must settle exactly.
			if strings.Contains(w.Body.String(), `"total_tokens":10`) {
				t.Fatalf("usage-only chunk leaked to client: %s", w.Body.String())
			}
			lim := accounting.WindowLimit{TokensPerHour: 9, TokensPerDay: 100}
			if ok, _ := quota.Admit("K1", lim, 0, time.Now()); ok {
				t.Fatal("expected reject: 10 settled tokens exceed hour limit 9 (usage lost)")
			}
			_ = writer.Close()
			recs := readRecords(t, path)
			if len(recs) != 1 || recs[0].TotalTokens != 10 || recs[0].UsageStatus != accounting.UsageExact {
				t.Fatalf("records = %+v", recs)
			}
		})
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
	p, _ := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), quota, writer, false)

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
	p, _ := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), quota, writer, false)

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

// T-M1: the generative output cap must not be bypassable. Negative,
// malformed (float/exponent/overflow), and over-cap limits are rejected
// fail-closed on both endpoints, an over-cap value in the alternate field
// is caught even when the primary field is present, and an explicit 0 is
// forwarded unchanged rather than silently raised to the cap.
func TestOutputCapCannotBeBypassed(t *testing.T) {
	const cap = 100
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/completions"} {
		f := newFakeVLLM(t, okJSON)
		cfg := testConfig(f.server.URL)
		cfg.Models["gen-1"] = config.Model{
			Type: "generation", Strategy: "single",
			Policy:   config.ModelPolicy{MaxOutputTokens: cap},
			Backends: []config.BackendRef{{Name: "b1", Weight: 1}},
		}
		router, err := routing.New(cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		client, err := backend.New(backend.Options{
			Name: "b1", Cfg: cfg.Backends["b1"], Network: backend.Policy{Mode: "loopback-only"},
			MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: discardLogger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		p, err := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), nil, nil, false)
		if err != nil {
			t.Fatal(err)
		}

		t.Run(endpoint, func(t *testing.T) {
			cases := []struct {
				name string
				body string
				want int
			}{
				{"negative", `{"model":"gen-1","max_tokens":-5}`, 400},
				{"exponent", `{"model":"gen-1","max_tokens":1e9}`, 400},
				{"float", `{"model":"gen-1","max_tokens":1000000.0}`, 400},
				{"over-cap", `{"model":"gen-1","max_tokens":5000}`, 400},
				{"over-cap-alt-with-primary", `{"model":"gen-1","max_completion_tokens":10,"max_tokens":5000}`, 400},
				{"zero-not-raised", `{"model":"gen-1","max_tokens":0}`, 200},
				{"in-cap-forwarded", `{"model":"gen-1","max_tokens":50}`, 200},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					f.lastBody = nil
					w := run(t, p, http.MethodPost, endpoint, tc.body, testKey())
					if w.Code != tc.want {
						t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
					}
					if tc.want == 400 {
						if len(f.lastBody) != 0 {
							t.Fatalf("backend must not be reached on rejection, got %s", f.lastBody)
						}
						return
					}
					var body map[string]json.RawMessage
					if err := json.Unmarshal(f.lastBody, &body); err != nil {
						t.Fatalf("backend body: %v", err)
					}
					assertFieldAtOrBelow(t, body, "max_tokens", cap)
					assertFieldAtOrBelow(t, body, "max_completion_tokens", cap)
				})
			}
		})
	}
}

// assertFieldAtOrBelow fails if body[name] is present and exceeds cap.
func assertFieldAtOrBelow(t *testing.T, body map[string]json.RawMessage, name string, cap int) {
	t.Helper()
	raw, ok := body[name]
	if !ok {
		return
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatalf("%s present but not a number: %s", name, raw)
	}
	v, err := n.Int64()
	if err != nil {
		t.Fatalf("%s not an integer: %s", name, raw)
	}
	if v > int64(cap) {
		t.Fatalf("%s = %d exceeds cap %d", name, v, cap)
	}
}
