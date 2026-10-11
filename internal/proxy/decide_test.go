package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mellomting/internal/accounting"
	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/routing"
	"mellomting/internal/testsupport"
)

// fakeSystemOne is a fake of the Clef-Flash wrapper the probe
// described (SYSTEMONE_PLAN, "What the probe established"): the request
// is exactly model, state and questions — any other top-level key,
// including stream and max_tokens, is a 400 — and model must equal the
// server's own id. A valid call answers with the Responses usage shape,
// input tokens only. failWith, when non-empty, answers every request
// with a 400 {"error": ...} instead.
type fakeSystemOne struct {
	id       string // the server's own model id
	server   *httptest.Server
	failWith string

	mu       sync.Mutex
	lastBody []byte
	requests int
}

func newFakeSystemOne(t *testing.T, id string) *fakeSystemOne {
	t.Helper()
	f := &fakeSystemOne{id: id}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.lastBody = body
		f.requests++
		f.mu.Unlock()
		if f.failWith != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"` + f.failWith + `"}`))
			return
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"body is not a JSON object"}`))
			return
		}
		for key := range fields {
			switch key {
			case "model", "state", "questions":
			default:
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"unknown key ` + key + `"}`))
				return
			}
		}
		var env struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &env)
		if env.Model != f.id {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"model must be ` + f.id + `"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"` + f.id + `","answers":{"q1":"yes"},"usage":{"input_tokens":7,"output_tokens":0}}`))
	}))
	t.Cleanup(f.server.Close)
	return f
}

// lastRequest returns the most recent forwarded body and how many
// requests the fake has received.
func (f *fakeSystemOne) lastRequest() ([]byte, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastBody, f.requests
}

// decideConfig is testConfig with one decision model, clef-flash, whose
// backend's upstream model is upstream.
func decideConfig(serverURL, upstream string) *config.Config {
	cfg := testConfig(serverURL)
	b := cfg.Backends["b1"]
	b.UpstreamModel = upstream
	cfg.Backends["b1"] = b
	cfg.Models = map[string]config.Model{
		"clef-flash": {
			Type:     "decision",
			Strategy: "single",
			Backends: []config.BackendRef{{Name: "b1", Weight: 1}},
		},
	}
	return cfg
}

// newDecideProxy builds a proxy over the fake SystemOne wrapper, with
// the upstream model defaulting to the server's own id (§D11).
func newDecideProxy(t *testing.T, f *fakeSystemOne) *Proxy {
	t.Helper()
	return newProxyCfg(t, decideConfig(f.server.URL, f.id))
}

// newDecideAccountingProxy is newDecideProxy with token accounting
// enabled, for the usage test (§D5).
func newDecideAccountingProxy(t *testing.T, f *fakeSystemOne, quota *accounting.Quota, writer *accounting.Writer) *Proxy {
	t.Helper()
	cfg := decideConfig(f.server.URL, f.id)
	cfg.Accounting.Enabled = true
	cfg.Accounting.EnsureStreamUsage = new(true)
	router, err := routing.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := backend.New(backend.Options{
		Name: "b1", Cfg: cfg.Backends["b1"], Network: backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: testsupport.DiscardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(cfg, router, map[string]*backend.Client{"b1": client}, testsupport.DiscardLogger(), quota, writer, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestDecideForwardsBodyWithNothingAdded pins §D3: the server rejects
// any key it does not know, so Mellomting forwards exactly what the
// client sent, with only model rewritten to the upstream name. The
// forwarded body's key set equals the client's.
func TestDecideForwardsBodyWithNothingAdded(t *testing.T) {
	t.Parallel()
	// The server's own id differs from the public name, so the model
	// rewrite is visible in what the backend received.
	f := newFakeSystemOne(t, "clef-internal")
	p := newProxyCfg(t, decideConfig(f.server.URL, "clef-internal"))

	body := `{"model":"clef-flash","state":{"pkg":"Acme"},"questions":{"q1":"Is this safe?"}}`
	rec := run(t, p, http.MethodPost, "/v1/systemone", body, testKey("clef-flash"))
	if rec.Code != 200 {
		t.Fatalf("status = %d (want 200) body = %s", rec.Code, rec.Body.String())
	}
	got, _ := f.lastRequest()
	var clientFields, gotFields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &clientFields); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &gotFields); err != nil {
		t.Fatalf("forwarded body is not JSON: %q", got)
	}
	if len(gotFields) != len(clientFields) {
		t.Fatalf("forwarded body has %d keys, client sent %d: %s", len(gotFields), len(clientFields), got)
	}
	for key := range clientFields {
		if _, ok := gotFields[key]; !ok {
			t.Fatalf("forwarded body lost the client's key %q: %s", key, got)
		}
	}
	var env struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(got, &env)
	if env.Model != "clef-internal" {
		t.Fatalf("forwarded model = %q, want the upstream name clef-internal", env.Model)
	}
}

// TestDecideRefusesStreamBeforeForwarding pins §D1: opDecide is
// non-generative, so "stream": true is refused locally with a 400 and
// never forwarded.
func TestDecideRefusesStreamBeforeForwarding(t *testing.T) {
	t.Parallel()
	f := newFakeSystemOne(t, "clef-flash")
	p := newDecideProxy(t, f)

	rec := run(t, p, http.MethodPost, "/v1/systemone",
		`{"model":"clef-flash","stream":true,"state":{},"questions":{}}`, testKey("clef-flash"))
	if rec.Code != 400 {
		t.Fatalf("status = %d (want 400) body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stream_not_supported") {
		t.Fatalf("body = %s (want stream_not_supported)", rec.Body.String())
	}
	if _, n := f.lastRequest(); n != 0 {
		t.Fatalf("backend received %d requests, want none", n)
	}
}

// TestDecideKeyACLMatchesUnknownModel pins §31/§D10: a key without the
// decision model gets the same 404 as an unknown model — the response
// does not reveal whether the model exists.
func TestDecideKeyACLMatchesUnknownModel(t *testing.T) {
	t.Parallel()
	f := newFakeSystemOne(t, "clef-flash")
	p := newDecideProxy(t, f)

	denied := run(t, p, http.MethodPost, "/v1/systemone",
		`{"model":"clef-flash","state":{},"questions":{}}`, testKey("other-model"))
	unknown := run(t, p, http.MethodPost, "/v1/systemone",
		`{"model":"no-such-model","state":{},"questions":{}}`, testKey("*"))
	if denied.Code != 404 || unknown.Code != 404 {
		t.Fatalf("statuses = %d/%d, want 404/404", denied.Code, unknown.Code)
	}
	if denied.Body.String() != unknown.Body.String() {
		t.Fatalf("ACL denial and unknown model differ:\n%s\n%s", denied.Body.String(), unknown.Body.String())
	}
	if _, n := f.lastRequest(); n != 0 {
		t.Fatalf("backend received %d requests, want none", n)
	}
}

// TestDecideAccountsUsageAsSystemone pins §D5: the wrapper's usage
// shape (input_tokens, output_tokens 0 — the Responses shape) is
// already parsed; the record carries endpoint "systemone", counts the
// input tokens, and computes the total as input plus output.
func TestDecideAccountsUsageAsSystemone(t *testing.T) {
	t.Parallel()
	f := newFakeSystemOne(t, "clef-flash")
	quota := accounting.NewQuota()
	writer, path := tmpWriter(t)
	p := newDecideAccountingProxy(t, f, quota, writer)

	rec := run(t, p, http.MethodPost, "/v1/systemone",
		`{"model":"clef-flash","state":{"pkg":"Acme"},"questions":{"q1":"Is this safe?"}}`, testKey("clef-flash"))
	if rec.Code != 200 {
		t.Fatalf("status = %d (want 200) body = %s", rec.Code, rec.Body.String())
	}
	// 7 input tokens settled against the key's quota.
	lim := accounting.WindowLimit{TokensPerHour: 5, TokensPerDay: 100}
	if ok, _ := quota.Admit("K1", lim, 0, time.Now()); ok {
		t.Fatal("expected reject: 7 input tokens settled exceeds hour limit 5")
	}
	_ = writer.Close()
	recs := readRecords(t, path)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	r := recs[0]
	if r.Endpoint != "systemone" || r.InputTokens != 7 || r.OutputTokens != 0 || r.TotalTokens != 7 {
		t.Fatalf("record = %+v, want endpoint systemone, 7/0/7 tokens", r)
	}
	if r.UsageStatus != accounting.UsageExact {
		t.Fatalf("usage_status = %s, want exact", r.UsageStatus)
	}
}

// TestDecideForwardsServerRejections pins §D3/§D6: a key the server
// does not know (here "stream": false, which it rejects) is forwarded
// — that rejection is the server's rule, not Mellomting's — and its
// reason reaches the client through the relay.
func TestDecideForwardsServerRejections(t *testing.T) {
	t.Parallel()
	f := newFakeSystemOne(t, "clef-flash")
	cfg := decideConfig(f.server.URL, f.id)
	b := cfg.Backends["b1"]
	b.RelayUpstreamErrors = true
	cfg.Backends["b1"] = b
	p := newProxyCfg(t, cfg)

	rec := run(t, p, http.MethodPost, "/v1/systemone",
		`{"model":"clef-flash","stream":false,"state":{},"questions":{}}`, testKey("clef-flash"))
	if rec.Code != 400 {
		t.Fatalf("status = %d (want 400) body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown key stream") {
		t.Fatalf("body = %s, want the server's own rejection reason", rec.Body.String())
	}
	got, _ := f.lastRequest()
	if !strings.Contains(string(got), `"stream"`) {
		t.Fatalf("forwarded body = %s, want the client's stream key forwarded", got)
	}
}

// TestDecideRelaysBackendStringError pins §D6: the wrapper's errors are
// a bare {"error": "<string>"}. With relay on — the default — the
// client sees the reason; with it off, the client error stays generic;
// the debug line carries the message either way.
func TestDecideRelaysBackendStringError(t *testing.T) {
	t.Parallel()
	const reason = "Complete input exceeds 4096 tokens; shorten state/schema"
	withRelay := func(cfg *config.Config, on bool) *config.Config {
		b := cfg.Backends["b1"]
		b.RelayUpstreamErrors = on
		cfg.Backends["b1"] = b
		return cfg
	}

	t.Run("relayed by default", func(t *testing.T) {
		t.Parallel()
		f := newFakeSystemOne(t, "clef-flash")
		f.failWith = reason
		var buf bytes.Buffer
		p := newProxyLogged(t, withRelay(decideConfig(f.server.URL, f.id), true), &buf, slog.LevelInfo)

		rec := run(t, p, http.MethodPost, "/v1/systemone",
			`{"model":"clef-flash","state":{},"questions":{}}`, testKey("clef-flash"))
		if rec.Code != 400 {
			t.Fatalf("status = %d (want 400) body = %s", rec.Code, rec.Body.String())
		}
		decodeErr(t, rec.Body.String(), rec.Code)
		if !strings.Contains(rec.Body.String(), reason) {
			t.Fatalf("body = %s, want it to carry the backend's reason", rec.Body.String())
		}
		// The envelope is Mellomting's own.
		if !strings.Contains(rec.Body.String(), "upstream_rejected") {
			t.Fatalf("body = %s, want the upstream_rejected code", rec.Body.String())
		}
		// The Info line records the relay by size, not its text.
		if strings.Contains(buf.String(), reason) {
			t.Fatalf("backend message logged at info level: %s", buf.String())
		}
		if !strings.Contains(buf.String(), "relayed backend error detail") {
			t.Fatalf("no relay record at info level: %s", buf.String())
		}
	})

	t.Run("generic with relay off, debug line carries the message", func(t *testing.T) {
		t.Parallel()
		f := newFakeSystemOne(t, "clef-flash")
		f.failWith = reason
		var buf bytes.Buffer
		p := newProxyLogged(t, withRelay(decideConfig(f.server.URL, f.id), false), &buf, slog.LevelDebug)

		rec := run(t, p, http.MethodPost, "/v1/systemone",
			`{"model":"clef-flash","state":{},"questions":{}}`, testKey("clef-flash"))
		if rec.Code != 400 {
			t.Fatalf("status = %d (want 400) body = %s", rec.Code, rec.Body.String())
		}
		decodeErr(t, rec.Body.String(), rec.Code)
		if strings.Contains(rec.Body.String(), reason) {
			t.Fatalf("backend message reached the client with relay off: %s", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "upstream rejected the request") {
			t.Fatalf("body = %s, want the generic message", rec.Body.String())
		}
		line := detailLine(t, &buf)
		if line == nil {
			t.Fatalf("no diagnostic line was logged: %s", buf.String())
		}
		if got := line["upstream_message"]; got != reason {
			t.Fatalf("upstream_message = %v, want %q", got, reason)
		}
	})

	t.Run("debug line carries the message with relay on", func(t *testing.T) {
		t.Parallel()
		f := newFakeSystemOne(t, "clef-flash")
		f.failWith = reason
		var buf bytes.Buffer
		p := newProxyLogged(t, withRelay(decideConfig(f.server.URL, f.id), true), &buf, slog.LevelDebug)

		rec := run(t, p, http.MethodPost, "/v1/systemone",
			`{"model":"clef-flash","state":{},"questions":{}}`, testKey("clef-flash"))
		if rec.Code != 400 {
			t.Fatalf("status = %d (want 400) body = %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), reason) {
			t.Fatalf("body = %s, want the relayed reason", rec.Body.String())
		}
		line := detailLine(t, &buf)
		if line == nil {
			t.Fatalf("no diagnostic line was logged: %s", buf.String())
		}
		if got := line["upstream_message"]; got != reason {
			t.Fatalf("upstream_message = %v, want %q", got, reason)
		}
	})
}
