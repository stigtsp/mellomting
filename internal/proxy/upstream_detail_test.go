package proxy

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/routing"
)

// The body the reported SGLang failure produced: an OpenAI-shaped 400
// whose message is the only thing that distinguishes a chat-template
// mismatch from an output-limit, tool-schema, or context-window error.
const (
	upstreamRejectMessage = "invalid tool schema"
	upstreamRejectSecret  = "sk-live-deadbeef"
)

func upstreamRejectsWithDetail(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(400)
	_, _ = w.Write([]byte(`{"error":{"type":"BadRequestError","code":400,"message":"` +
		upstreamRejectMessage + `"},"api_key":"` + upstreamRejectSecret + `"}`))
}

// newProxyLogged builds a single-backend proxy whose logger writes JSON
// into buf at the given level, so a test can assert both what the client
// received and what the operator's log did or did not record.
func newProxyLogged(t *testing.T, cfg *config.Config, buf *bytes.Buffer, level slog.Level) *Proxy {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level}))
	router, err := routing.New(cfg, nil)
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
	p, err := New(cfg, router, map[string]*backend.Client{"b1": client}, log, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// detailLine returns the "backend error detail" record from a JSON log
// buffer, or nil when none was written.
func detailLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		if rec["msg"] == "backend attempt failed" {
			return rec
		}
	}
	return nil
}

// TestBackendRejectionIsDiagnosableAtDebug is the report's regression
// test: a backend 400 still produces the configured sanitized client
// error, while the diagnostic sink receives the bounded backend message
// with this request's ID and the backend name.
func TestBackendRejectionIsDiagnosableAtDebug(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, upstreamRejectsWithDetail)
	var buf bytes.Buffer
	p := newProxyLogged(t, testConfig(f.server.URL), &buf, slog.LevelDebug)

	rec := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())

	if rec.Code != 400 {
		t.Fatalf("status = %d (want 400)", rec.Code)
	}
	decodeErr(t, rec.Body.String(), rec.Code)
	if strings.Contains(rec.Body.String(), upstreamRejectMessage) {
		t.Fatalf("backend message reached a client that did not opt in: %s", rec.Body.String())
	}

	line := detailLine(t, &buf)
	if line == nil {
		t.Fatalf("no diagnostic line was logged: %s", buf.String())
	}
	if got := line["upstream_message"]; got != upstreamRejectMessage {
		t.Fatalf("upstream_message = %v, want %q", got, upstreamRejectMessage)
	}
	if got := line["upstream_type"]; got != "BadRequestError" {
		t.Fatalf("upstream_type = %v, want BadRequestError", got)
	}
	if got := line["upstream_code"]; got != "400" {
		t.Fatalf("upstream_code = %v, want \"400\"", got)
	}
	if got := line["request_id"]; got != "req_test" {
		t.Fatalf("request_id = %v, want req_test", got)
	}
	if got := line["backend"]; got != "b1" {
		t.Fatalf("backend = %v, want b1", got)
	}
	// Only the three named members are read, so a credential the backend
	// echoed elsewhere in the body cannot ride along.
	if strings.Contains(buf.String(), upstreamRejectSecret) {
		t.Fatalf("an unrelated backend field reached the log: %s", buf.String())
	}
}

// TestBackendRejectionSilentAtInfo holds the default posture: with the
// shipped log level, a backend's own words appear nowhere. The class in
// the request line is all an operator gets until they ask for more.
func TestBackendRejectionSilentAtInfo(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, upstreamRejectsWithDetail)
	var buf bytes.Buffer
	p := newProxyLogged(t, testConfig(f.server.URL), &buf, slog.LevelInfo)

	run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())

	if strings.Contains(buf.String(), upstreamRejectMessage) {
		t.Fatalf("backend message logged at info level: %s", buf.String())
	}
	if detailLine(t, &buf) != nil {
		t.Fatalf("diagnostic line emitted at info level: %s", buf.String())
	}
	// The request line still classifies the failure.
	if !strings.Contains(buf.String(), `"error_class":"backend_4xx"`) {
		t.Fatalf("request line lost its error class: %s", buf.String())
	}
}

// TestBackendRejectionRelayOptIn covers the explicit opt-in: the message
// reaches the client, and the envelope it arrives in does not change.
func TestBackendRejectionRelayOptIn(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, upstreamRejectsWithDetail)
	cfg := testConfig(f.server.URL)
	b := cfg.Backends["b1"]
	b.RelayUpstreamErrors = true
	cfg.Backends["b1"] = b
	var buf bytes.Buffer
	p := newProxyLogged(t, cfg, &buf, slog.LevelInfo)

	rec := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())

	if rec.Code != 400 {
		t.Fatalf("status = %d (want 400)", rec.Code)
	}
	decodeErr(t, rec.Body.String(), rec.Code)
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Error.Message, upstreamRejectMessage) {
		t.Fatalf("message = %q, want it to carry %q", body.Error.Message, upstreamRejectMessage)
	}
	if body.Error.Type != errUpstreamRejected.typ || body.Error.Code != errUpstreamRejected.code {
		t.Fatalf("relay changed the envelope: type=%q code=%q", body.Error.Type, body.Error.Code)
	}
	if strings.Contains(rec.Body.String(), upstreamRejectSecret) {
		t.Fatalf("an unrelated backend field reached the client: %s", rec.Body.String())
	}
}

// TestBackendRejectionWithoutParsableBody proves the diagnostic path
// fails closed on bodies it cannot make sense of: no line is logged, no
// message is relayed, and the client error is unchanged.
func TestBackendRejectionWithoutParsableBody(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
	}{
		{"malformed json", `{"error":{"message":"trunc`},
		{"html error page", `<html><body>400 Bad Request</body></html>`},
		{"no error member", `{"detail":"validation failed"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeVLLM(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(tc.body))
			})
			cfg := testConfig(f.server.URL)
			b := cfg.Backends["b1"]
			b.RelayUpstreamErrors = true
			cfg.Backends["b1"] = b
			var buf bytes.Buffer
			p := newProxyLogged(t, cfg, &buf, slog.LevelDebug)

			rec := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())

			if rec.Code != 400 {
				t.Fatalf("status = %d (want 400)", rec.Code)
			}
			decodeErr(t, rec.Body.String(), rec.Code)
			var body struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Message != errUpstreamRejected.msg {
				t.Fatalf("message = %q, want the unchanged %q", body.Error.Message, errUpstreamRejected.msg)
			}
			// The line is still emitted: it reports the rejection with
			// empty fields, which is how an operator tells "the
			// backend said nothing parsable" from "the diagnostic is
			// not reaching me".
			line := detailLine(t, &buf)
			if line == nil {
				t.Fatalf("no line was logged for an unparsable body: %s", buf.String())
			}
			for _, f := range []string{"upstream_type", "upstream_code", "upstream_message"} {
				if got := line[f]; got != "" {
					t.Fatalf("%s = %v, want empty for an unparsable body", f, got)
				}
			}
		})
	}
}

// TestBackendRejectionRelayIsBounded keeps a hostile backend from using
// the relay as an amplifier: the appended message stays within the
// diagnostic cap however long the backend's own is.
func TestBackendRejectionRelayIsBounded(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("E", 8192)
	f := newFakeVLLM(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"` + long + `"}}`))
	})
	cfg := testConfig(f.server.URL)
	b := cfg.Backends["b1"]
	b.RelayUpstreamErrors = true
	cfg.Backends["b1"] = b
	var buf bytes.Buffer
	p := newProxyLogged(t, cfg, &buf, slog.LevelInfo)

	rec := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())

	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	grown := len(body.Error.Message) - len(errUpstreamRejected.msg)
	// Without this the test passes when nothing is relayed at all, so
	// it would stay green if the opt-in silently broke.
	if grown <= 0 {
		t.Fatalf("nothing was relayed, so no bound was exercised: %q", body.Error.Message)
	}
	// The relay appends one message plus its ": " separator. Naming the
	// whole three-field budget here would not catch the message cap
	// alone being raised.
	if want := backend.MaxMessageBytes + len(": "); grown > want {
		t.Fatalf("relayed message grew by %d bytes, exceeds the %d-byte budget", grown, want)
	}
}

// TestRelayDoesNotMutateTheSharedError pins an aliasing hazard in the
// relay. The apiError values are package-level templates shared by every
// request, so appending to one must produce a copy; a relay that wrote
// through would leak one client's backend message into every later error
// the proxy returned, on every backend.
func TestRelayDoesNotMutateTheSharedError(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, upstreamRejectsWithDetail)
	cfg := testConfig(f.server.URL)
	b := cfg.Backends["b1"]
	b.RelayUpstreamErrors = true
	cfg.Backends["b1"] = b
	p := newProxyCfg(t, cfg)

	before := errUpstreamRejected
	err := &backend.Upstream{Status: 400, Detail: backend.Detail{Message: "first"}}

	got := p.relayUpstreamDetail(errUpstreamRejected, "req_test", "b1", err)
	if got.msg == before.msg {
		t.Fatal("relay did not append the backend message")
	}
	if errUpstreamRejected != before {
		t.Fatalf("relay mutated the shared error template: %+v", errUpstreamRejected)
	}
	// The next request must start from the same base, not from the
	// previous result.
	if again := p.relayUpstreamDetail(errUpstreamRejected, "req_test", "b1", err); again != got {
		t.Fatalf("message accumulated across requests: %q then %q", got.msg, again.msg)
	}
}

// TestRelayIgnoresFailuresWithoutABody: relay speaks only for a backend
// that answered. A connection failure or a timeout has no error body, so
// the client gets the sanitized class and nothing invented around it.
func TestRelayIgnoresFailuresWithoutABody(t *testing.T) {
	t.Parallel()

	// Wait on the request context, not the clock: the proxy abandons
	// this at header_timeout, and a fixed sleep would then block
	// httptest's Close for the rest of it.
	f := newFakeVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	cfg := testConfig(f.server.URL)
	b := cfg.Backends["b1"]
	b.RelayUpstreamErrors = true
	b.HeaderTimeout = config.Duration(100 * time.Millisecond)
	cfg.Backends["b1"] = b
	p := newProxyCfg(t, cfg)

	rec := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true}`, testKey())

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Message != errUpstreamTimeout.msg {
		t.Fatalf("message = %q, want the unchanged %q", body.Error.Message, errUpstreamTimeout.msg)
	}
}

// TestRelayKeepsUpstreamRetryAfter pins the ordering on the terminal
// path: the 429 branch recognizes the rate-limit response by value, so
// it has to run before relay rewrites the message. Reversing the two
// would silently drop the backend's own Retry-After.
func TestRelayKeepsUpstreamRetryAfter(t *testing.T) {
	t.Parallel()

	f := newFakeVLLM(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"KV cache is full"}}`))
	})
	cfg := testConfig(f.server.URL)
	b := cfg.Backends["b1"]
	b.RelayUpstreamErrors = true
	cfg.Backends["b1"] = b
	p := newProxyCfg(t, cfg)

	rec := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "42" {
		t.Fatalf("Retry-After = %q, want the upstream's 42", got)
	}
	if !strings.Contains(rec.Body.String(), "KV cache is full") {
		t.Fatalf("relayed message missing: %s", rec.Body.String())
	}
}

// TestRelayRefusesUpstream5xx: the opt-in relays what a client got
// wrong, not what broke inside the backend. A 5xx message is where a
// traceback or a model path appears, so it stays operator-only however
// the backend is configured — the debug log still carries it.
func TestRelayRefusesUpstream5xx(t *testing.T) {
	t.Parallel()

	const internal = "Traceback: /opt/models/qwen/config.json missing"
	f := newFakeVLLM(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"` + internal + `"}}`))
	})
	cfg := testConfig(f.server.URL)
	b := cfg.Backends["b1"]
	b.RelayUpstreamErrors = true
	cfg.Backends["b1"] = b
	var buf bytes.Buffer
	p := newProxyLogged(t, cfg, &buf, slog.LevelDebug)

	rec := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), internal) {
		t.Fatalf("a backend 5xx message reached the client: %s", rec.Body.String())
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Message != errUpstream5xx.msg {
		t.Fatalf("message = %q, want the unchanged %q", body.Error.Message, errUpstream5xx.msg)
	}
	// The operator still gets it.
	if line := detailLine(t, &buf); line == nil || line["upstream_message"] != internal {
		t.Fatalf("the 5xx message did not reach the debug log: %v", line)
	}
}

// TestRelayRefusesNonRequestStatuses is the complement of the opt-in: a
// 4xx that is not about the client's request carries exactly the things
// PLAN §72 keeps out of a client response — the credential Mellomting
// injected, and the backend's own model path, which routing rewrites so
// a client never learns it.
func TestRelayRefusesNonRequestStatuses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		status  int
		message string
		want    apiError
	}{{
		name:    "401 quotes the injected credential",
		status:  http.StatusUnauthorized,
		message: "Incorrect API key provided: sk-proj-ab***XYZ",
		want:    errUpstreamRejected,
	}, {
		name:    "403 is about the proxy's own credential",
		status:  http.StatusForbidden,
		message: "key does not have access to this model",
		want:    errUpstreamRejected,
	}, {
		name:    "404 names the backend's model path",
		status:  http.StatusNotFound,
		message: "The model `/srv/models/qwen3-27b` does not exist",
		want:    errUpstreamRejected,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeVLLM(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"message":"` + tc.message + `"}}`))
			})
			cfg := testConfig(f.server.URL)
			b := cfg.Backends["b1"]
			b.RelayUpstreamErrors = true
			cfg.Backends["b1"] = b
			p := newProxyCfg(t, cfg)

			rec := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())

			if strings.Contains(rec.Body.String(), tc.message) {
				t.Fatalf("a %d message reached the client: %s", tc.status, rec.Body.String())
			}
			var body struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Message != tc.want.msg {
				t.Fatalf("message = %q, want the unchanged %q", body.Error.Message, tc.want.msg)
			}
		})
	}
}

// TestRelayIsRecordedAtTheLevelItIsDisclosed: the debug line is the
// operator's to enable, but text already handed to a client has to be in
// the log whatever the level — otherwise nothing says what was given out.
func TestRelayIsRecordedAtTheLevelItIsDisclosed(t *testing.T) {
	t.Parallel()

	f := newFakeVLLM(t, upstreamRejectsWithDetail)
	cfg := testConfig(f.server.URL)
	b := cfg.Backends["b1"]
	b.RelayUpstreamErrors = true
	cfg.Backends["b1"] = b
	var buf bytes.Buffer
	p := newProxyLogged(t, cfg, &buf, slog.LevelInfo)

	rec := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())

	if !strings.Contains(rec.Body.String(), upstreamRejectMessage) {
		t.Fatalf("relay did not reach the client: %s", rec.Body.String())
	}
	if !strings.Contains(buf.String(), `"msg":"relayed backend error detail"`) {
		t.Fatalf("a disclosure to the client was not recorded at info: %s", buf.String())
	}
	if !strings.Contains(buf.String(), upstreamRejectMessage) {
		t.Fatalf("the log does not say what was disclosed: %s", buf.String())
	}
}

// TestHeaderTimeoutIsDiagnosable: the failure this branch exists for
// carries no body at all. An operator who turns debug on after a wave of
// 504s must still learn which backend timed out, or the diagnostic is
// silent in exactly the case it was built for.
func TestHeaderTimeoutIsDiagnosable(t *testing.T) {
	t.Parallel()

	f := newFakeVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	cfg := testConfig(f.server.URL)
	b := cfg.Backends["b1"]
	b.HeaderTimeout = config.Duration(100 * time.Millisecond)
	cfg.Backends["b1"] = b
	var buf bytes.Buffer
	p := newProxyLogged(t, cfg, &buf, slog.LevelDebug)

	rec := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true}`, testKey())

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rec.Code)
	}
	line := detailLine(t, &buf)
	if line == nil {
		t.Fatalf("no diagnostic line for a header timeout: %s", buf.String())
	}
	if got := line["error_class"]; got != "backend_timeout" {
		t.Fatalf("error_class = %v, want backend_timeout", got)
	}
	if got := line["backend"]; got != "b1" {
		t.Fatalf("backend = %v, want b1", got)
	}
	// No response arrived, so there is nothing to report about a body.
	if _, ok := line["upstream_message"]; ok {
		t.Fatalf("a bodiless failure reported an upstream message: %v", line)
	}
}
