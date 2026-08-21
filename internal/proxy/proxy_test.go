package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// fakeVLLM is a minimal OpenAI-shaped backend (PLAN §84 patterns).
type fakeVLLM struct {
	server    *httptest.Server
	lastModel string
	lastPath  string
	lastBody  []byte
	behaviour http.HandlerFunc
}

func newFakeVLLM(t *testing.T, behaviour http.HandlerFunc) *fakeVLLM {
	t.Helper()
	f := &fakeVLLM{behaviour: behaviour}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.lastBody = body
		f.lastPath = r.URL.Path
		var env struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &env)
		f.lastModel = env.Model
		behaviour(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func okJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion"}`))
}

func chatSSE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(
		`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"Hel"}}]}` + "\n\n" +
			`event: custom.field` + "\n" +
			`data: {"weird":"passthru"}` + "\n\n" +
			`data: [DONE]` + "\n\n",
	))
}

func responsesJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"resp_123","object":"response","status":"completed"}`))
}

func responsesSSE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(
		`event: response.created` + "\n" +
			`data: {"type":"response.created","response":{"id":"resp_abc","status":"in_progress"}}` + "\n\n" +
			`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"id":"resp_abc","status":"completed"}}` + "\n\n",
	))
}

func upstream500(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(500)
	_, _ = w.Write([]byte(`{"error":{"message":"secret backend detail"}}`))
}

func upstream429(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(429)
	_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
}

func bigSSEEvent(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(make([]byte, maxSSEEvent))
	_, _ = w.Write([]byte("\n\n"))
}

func testConfig(serverURL string) *config.Config {
	return &config.Config{
		Server: config.Server{
			MaxBodyBytes:            1 << 20,
			MaxResponseBytes:        1 << 20,
			MaxInflightRequests:     8,
			MaxBufferedRequestBytes: 1 << 20,
			StreamIdleTimeout:       config.Duration(2 * time.Second),
			StreamWriteTimeout:      config.Duration(2 * time.Second),
		},
		Responses: config.Responses{
			AffinityTTL:        config.Duration(time.Hour),
			MaxAffinityEntries: 100,
		},
		Retry: config.Retry{
			MaxAttempts:    1,
			InitialBackoff: config.Duration(5 * time.Millisecond),
			MaxBackoff:     config.Duration(10 * time.Millisecond),
			Jitter:         func() *bool { f := false; return &f }(),
		},
		Backends: map[string]config.Backend{
			"b1": {
				BaseURL:           serverURL,
				UpstreamModel:     "Upstream/Model",
				MaxConcurrency:    4,
				QueueSize:         4,
				QueueTimeout:      config.Duration(500 * time.Millisecond),
				ConnectTimeout:    config.Duration(time.Second),
				HeaderTimeout:     config.Duration(2 * time.Second),
				RequestTimeout:    config.Duration(5 * time.Second),
				StreamIdleTimeout: config.Duration(2 * time.Second),
			},
		},
		Models: map[string]config.Model{
			"gen-1": {
				Type:     "generation",
				Strategy: "single",
				Backends: []config.BackendRef{{Name: "b1", Weight: 1}},
			},
		},
	}
}

func newProxy(t *testing.T, f *fakeVLLM) *Proxy {
	t.Helper()
	cfg := testConfig(f.server.URL)
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
	p, err := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 10}))
}

func testKey(models ...string) *auth.Key {
	if len(models) == 0 {
		models = []string{"gen-1"}
	}
	return &auth.Key{ID: "K1", Name: "t", Enabled: true, Models: models}
}

// run dispatches to the proxy method for an allow-listed path.
func run(t *testing.T, p *Proxy, method, path, body string, key *auth.Key) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	q := &Req{W: w, R: r, Key: key, RequestID: "req_test", Remote: "127.0.0.1"}
	switch {
	case method == http.MethodPost && path == "/v1/chat/completions":
		p.ChatCompletions(q)
	case method == http.MethodPost && path == "/v1/completions":
		p.Completions(q)
	case method == http.MethodPost && path == "/v1/embeddings":
		p.Embeddings(q)
	case method == http.MethodPost && path == "/v1/responses":
		p.ResponsesCreate(q)
	case path == "/v1/responses": // GET
		p.ResponsesRetrieve(q, "")
	default:
		id := strings.TrimPrefix(path, "/v1/responses/")
		id = strings.TrimSuffix(id, "/cancel")
		if strings.HasSuffix(path, "/cancel") {
			p.ResponsesCancel(q, id)
		} else {
			p.ResponsesRetrieve(q, id)
		}
	}
	return w
}

// --- SSE parser --------------------------------------------------------

func TestSSEParserEventsAndBounds(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		``,
		`data: line-one`,
		`data: line-two`,
		``,
		`data: [DONE]`,
		``,
	}, "\n") + "\n"

	p := newSSEParser(strings.NewReader(stream))
	var got []string
	for {
		ev, err := p.nextEvent()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("nextEvent: %v", err)
		}
		got = append(got, string(ev))
	}
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3: %#v", len(got), got)
	}
	if !strings.Contains(got[0], "response.created") {
		t.Fatalf("event 0 = %q", got[0])
	}
	if !strings.Contains(got[0], `data: {"type":"response.created"`) {
		t.Fatalf("raw data line lost: %q", got[0])
	}
	data, ok := dataField([]byte(got[1]))
	if !ok || data != "line-one\nline-two" {
		t.Fatalf("data field = %q ok=%v", data, ok)
	}
	data3, ok := dataField([]byte(got[2]))
	if !ok || data3 != "[DONE]" {
		t.Fatalf("done field = %q", data3)
	}

	// Line bound.
	p2 := newSSEParser(strings.NewReader(strings.Repeat("a", maxSSELine+1) + "\n"))
	if _, err := p2.nextEvent(); err != ErrSSELineTooLarge {
		t.Fatalf("line bound: err = %v", err)
	}

	// Unknown fields must be re-emitted verbatim.
	p3 := newSSEParser(strings.NewReader("ext: mystery\n\ncustom: x\n\n"))
	ev, _ := p3.nextEvent()
	if string(ev) != "ext: mystery\n\n" {
		t.Fatalf("unknown field not verbatim: %q", ev)
	}
}

// --- pipeline ----------------------------------------------------------

func TestChatCompletionNonStreamAndRewrite(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, okJSON)
	p := newProxy(t, f)
	rec := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","messages":[{"role":"user","content":"hi"}],"custom_ext_field":{"a":[1,2]}}`,
		testKey())
	if rec.Code != 200 {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "chatcmpl-1") {
		t.Fatalf("body = %q", rec.Body.String())
	}
	// Model rewritten, unknown fields preserved (PLAN §12, §13).
	if !strings.Contains(string(f.lastBody), "Upstream/Model") {
		t.Fatalf("outbound model not rewritten: %s", f.lastBody)
	}
	if !strings.Contains(string(f.lastBody), "custom_ext_field") {
		t.Fatalf("unknown field dropped: %s", f.lastBody)
	}
	if strings.Contains(string(f.lastBody), "gen-1") {
		t.Fatalf("public model leaked outbound: %s", f.lastBody)
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, chatSSE)
	p := newProxy(t, f)
	rec := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true}`, testKey())
	if rec.Code != 200 {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	ctype := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ctype, "text/event-stream") {
		t.Fatalf("content-type = %q", ctype)
	}
	body := rec.Body.String()
	for _, want := range []string{`"content":"Hel"`, "event: custom.field", `"weird":"passthru"`, "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q: %s", want, body)
		}
	}
}

func TestModelACL404(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, okJSON)
	p := newProxy(t, f)

	// Not allowed -> 404 model_not_found_or_not_allowed.
	rec := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1"}`, testKey("other-model"))
	if rec.Code != 404 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "model_not_found_or_not_allowed") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	// Unknown model -> identical response (PLAN §31).
	rec = run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"nope"}`, testKey())
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "model_not_found_or_not_allowed") {
		t.Fatalf("unknown model: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpstreamErrorsAreSanitized(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, upstream500)
	p := newProxy(t, f)
	rec := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())
	if rec.Code != 502 {
		t.Fatalf("status = %d (want 502)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret backend detail") {
		t.Fatalf("backend body leaked: %s", rec.Body.String())
	}
	// The body must be exactly ONE valid JSON error object (X7: two
	// concatenated objects was invalid JSON that OpenAI-compatible SDKs
	// fail to parse).
	decodeErr(t, rec.Body.String(), 502)

	f2 := newFakeVLLM(t, upstream429)
	p2 := newProxy(t, f2)
	rec = run(t, p2, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())
	if rec.Code != 429 {
		t.Fatalf("429 mapping: status = %d", rec.Code)
	}
	decodeErr(t, rec.Body.String(), 429)

	// A backend 400 (not 429, not >=500) maps to a client 400.
	f3 := newFakeVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"bad"}}`))
	})
	p3 := newProxy(t, f3)
	rec = run(t, p3, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())
	if rec.Code != 400 {
		t.Fatalf("400 mapping: status = %d", rec.Code)
	}
	decodeErr(t, rec.Body.String(), 400)
}

// TestRedirectNotRelayed is the X3 proxy-level check: a backend 3xx
// must surface as a sanitized OpenAI error (never the redirect body),
// and the redirect must not be followed toward /metrics or elsewhere.
func TestRedirectNotRelayed(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/metrics")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte(`<html><body>redirect target content</body></html>`))
	})
	p := newProxy(t, f)
	rec := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())
	// 3xx maps to a sanitized client error; the redirect body must not
	// reach the client and no second hop may be attempted.
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400 (3xx surfaced as sanitized error)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "redirect target content") {
		t.Fatalf("redirect body relayed to client: %s", rec.Body.String())
	}
	decodeErr(t, rec.Body.String(), 400)
	if f.lastPath == "/metrics" {
		t.Fatalf("redirect was followed to %q", f.lastPath)
	}
}

// TestRetryAndHealthClassification is the X6 amplifier check: a header
// timeout is a latency/capacity signal, so it must neither poison the
// passive-health state (which is keyed by backend name and would
// cascade a cooldown across every model/key on that backend) nor be
// blindly retried (re-issuing a full generation that cannot succeed
// within the header bound). Connection failures remain both retryable
// and health-poisoning.
func TestRetryAndHealthClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err    error
		retry  bool
		poison bool
	}{
		{backend.ErrConnect, true, true},
		{backend.ErrDialTimeout, true, true},
		{backend.ErrHeaderTimeout, false, false},
		{backend.ErrTimeout, false, false},
		{backend.ErrQueueFull, true, false},
	}
	for _, tc := range cases {
		if got := retryableBackendError(tc.err); got != tc.retry {
			t.Errorf("retryableBackendError(%v) = %v, want %v", tc.err, got, tc.retry)
		}
		if got := connectionLevelError(tc.err); got != tc.poison {
			t.Errorf("connectionLevelError(%v) = %v, want %v", tc.err, got, tc.poison)
		}
	}
}

// decodeErr asserts the response body is exactly one valid OpenAI-shaped
// JSON error object carrying the given HTTP status.
func decodeErr(t *testing.T, body string, status int) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), &struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{}); err != nil {
		t.Fatalf("error body is not a single valid JSON object (status %d): %q: %v", status, body, err)
	}
	// Assert the whole body was consumed by one object (no trailing
	// concatenated object) via a strict decode.
	dec := json.NewDecoder(strings.NewReader(body))
	var first struct {
		Error json.RawMessage `json:"error"`
	}
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, body)
	}
	if first.Error == nil {
		t.Fatalf("body carries no error field: %q", body)
	}
	if dec.More() {
		t.Fatalf("body carries more than one JSON object: %q", body)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("trailing content after error object: %v (body=%q)", err, body)
	}
}

func TestResponsesAffinityLifecycle(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			responsesJSON(w, r)
			return
		}
		responsesJSON(w, r)
	})
	p := newProxy(t, f)
	key := testKey()

	// Non-stream create: ID recorded.
	rec := run(t, p, http.MethodPost, "/v1/responses", `{"model":"gen-1","input":"hi"}`, key)
	if rec.Code != 200 {
		t.Fatalf("create status = %d", rec.Code)
	}
	if b, ok := p.affinity.Get(key.ID, "resp_123"); !ok || b != "b1" {
		t.Fatalf("affinity not recorded: ok=%v b=%q", ok, b)
	}

	// Retrieve routes to the owning backend.
	rec = run(t, p, http.MethodGet, "/v1/responses/resp_123", "", key)
	if rec.Code != 200 || f.lastPath != "/v1/responses/resp_123" {
		t.Fatalf("retrieve: status=%d path=%q", rec.Code, f.lastPath)
	}

	// A different key with no affinity must NOT reach the backend: a
	// response is bound to the key that created it, so a foreign key
	// gets 404 even when it knows the ID and only one backend is
	// possible (PLAN §21.1).
	other := &auth.Key{ID: "K2", Name: "o", Enabled: true, Models: []string{"gen-1"}}
	rec = run(t, p, http.MethodGet, "/v1/responses/resp_123", "", other)
	if rec.Code != 404 {
		t.Fatalf("foreign-key retrieve: status = %d, want 404", rec.Code)
	}
	// And it must not be able to cancel key K1's response either.
	rec = run(t, p, http.MethodPost, "/v1/responses/resp_123/cancel", "", other)
	if rec.Code != 404 {
		t.Fatalf("foreign-key cancel: status = %d, want 404", rec.Code)
	}

	// Cancel routes the same way.
	rec = run(t, p, http.MethodPost, "/v1/responses/resp_123/cancel", "", key)
	if rec.Code != 200 || !strings.HasSuffix(f.lastPath, "/cancel") {
		t.Fatalf("cancel: status=%d path=%q", rec.Code, f.lastPath)
	}
}

func TestResponsesStreamCapture(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, responsesSSE)
	p := newProxy(t, f)
	rec := run(t, p, http.MethodPost, "/v1/responses",
		`{"model":"gen-1","stream":true,"input":"hi"}`, testKey())
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if b, ok := p.affinity.Get("K1", "resp_abc"); !ok || b != "b1" {
		t.Fatalf("stream capture failed: ok=%v b=%q", ok, b)
	}
}

func TestHugeSSEEventBounded(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, bigSSEEvent)
	p := newProxy(t, f)
	rec := run(t, p, http.MethodPost, "/v1/responses",
		`{"model":"gen-1","stream":true}`, testKey())
	// 200 is committed before the oversized event is seen; the stream is
	// truncated rather than buffered unboundedly.
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.Len() > maxSSEEvent+64 {
		t.Fatalf("oversized event passed through (%d bytes)", rec.Body.Len())
	}
}

// TestStreamRequestNon200StreamResponse is the X4 regression test: a
// stream:true request answered with a 2xx other than 200 must not be
// read as a stream (the body was buffered and res.Body is nil). Before
// the fix the proxy read a nil body in a spawned goroutine and killed
// the process.
func TestStreamRequestNon200StreamResponse(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-created","object":"chat.completion"}`))
	})
	p := newProxy(t, f)
	rec := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true,"messages":[]}`, testKey())
	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "chatcmpl-created") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// panickingReader is an io.Reader whose Read always panics, used to
// prove the pump's spawned-goroutine recover containment.
type panickingReader struct{}

func (panickingReader) Read([]byte) (int, error) { panic("synthetic parser panic") }

// TestStreamIdleUsesPerBackendBound is the T-M3 regression test: the
// pump's upstream read-idle bound must come from the per-backend
// stream_idle_timeout, not the server-level default. A backend that
// sends one chunk then stalls must be terminated at the per-backend
// bound (well below the server-level value).
func TestStreamIdleUsesPerBackendBound(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl-1","choices":[{"delta":{"content":"H"}}]}` + "\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
	})
	cfg := testConfig(f.server.URL)
	// Server-level bound is large; the per-backend value must govern.
	cfg.Server.StreamIdleTimeout = config.Duration(5 * time.Second)
	b := cfg.Backends["b1"]
	b.StreamIdleTimeout = config.Duration(200 * time.Millisecond)
	cfg.Backends["b1"] = b
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
	p, err := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	rec := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","stream":true,"messages":[]}`, testKey())
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("stalled stream not terminated at per-backend stream_idle_timeout: elapsed = %v (server default was 5s)", elapsed)
	}
}

// TestPumpPanicContained proves a panic inside the stream-parser
// goroutine is converted into a bounded stream error and never takes
// the process down (the goroutine is spawned by the handler, so
// net/http's per-connection recover cannot reach it).
func TestPumpPanicContained(t *testing.T) {
	p := newProxy(t, newFakeVLLM(t, okJSON))
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gen-1","stream":true}`))
	w := httptest.NewRecorder()
	q := &Req{W: w, R: r, Key: testKey(), RequestID: "req_test", Remote: "127.0.0.1"}
	res := &backend.Result{Status: http.StatusOK, Body: io.NopCloser(panickingReader{})}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	status, _, cls := p.pump(q, res, opChat, cancel, "b1", &accounting.Usage{}, false)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if cls != "backend_stream_error" {
		t.Fatalf("class = %q, want backend_stream_error", cls)
	}
}

func TestBodyLimitsAndEncoding(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, okJSON)
	p := newProxy(t, f)

	// A normal-size body succeeds on the default-limit proxy.
	big := `{"model":"gen-1","x":"` + strings.Repeat("a", 512) + `"}`
	rec := run(t, p, http.MethodPost, "/v1/chat/completions", big, testKey())
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}

	// A tiny body limit forces 413.
	f3 := newFakeVLLM(t, okJSON)
	cfg3 := testConfig(f3.server.URL)
	cfg3.Server.MaxBodyBytes = 64
	router3, _ := routing.New(cfg3, nil)
	cl3, err := backend.New(backend.Options{
		Name: "b1", Cfg: cfg3.Backends["b1"], Network: backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg3.Server.MaxResponseBytes, Log: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p3, _ := New(cfg3, router3, map[string]*backend.Client{"b1": cl3}, discardLogger(), nil, nil)
	rec = run(t, p3, http.MethodPost, "/v1/chat/completions", big, testKey())
	if rec.Code != 413 {
		t.Fatalf("oversized body: status = %d", rec.Code)
	}

	// Non-identity encoding: 415.
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gen-1"}`))
	r.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()
	p.ChatCompletions(&Req{W: w, R: r, Key: testKey(), RequestID: "req_t", Remote: "x"})
	if w.Code != 415 {
		t.Fatalf("encoding: status = %d", w.Code)
	}

	// Missing model / invalid JSON: 400.
	rec = run(t, p, http.MethodPost, "/v1/chat/completions", `{}`, testKey())
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "missing_model") {
		t.Fatalf("missing model: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = run(t, p, http.MethodPost, "/v1/chat/completions", `not-json`, testKey())
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_json") {
		t.Fatalf("bad json: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDrainingRejectsNewInference(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, okJSON)
	p := newProxy(t, f)
	p.BeginDraining()
	rec := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())
	if rec.Code != 503 {
		t.Fatalf("draining: status = %d (want 503)", rec.Code)
	}
}

// Ensure the context-cancellation path compiles and is referenced.
var _ = context.Canceled
