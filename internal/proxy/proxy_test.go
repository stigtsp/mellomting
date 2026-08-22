package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// bigSSEEvent writes many short data lines whose aggregate exceeds
// maxSSEEvent (each line stays well under maxSSELine). Only the
// aggregated event bound (ErrSSEEventTooLarge) can trip, not the
// per-line bound — so TestHugeSSEEventBounded exercises the event-size
// cap, not the line cap (T-T7).
func bigSSEEvent(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	const line = 64 * 1024
	for i := 0; i < 20; i++ {
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(make([]byte, line))
		_, _ = w.Write([]byte("\n"))
	}
	_, _ = w.Write([]byte("\n"))
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
	p, err := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), nil, nil, false)
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

// TestModelLoggedTruncated verifies a hostile client cannot journal an
// unbounded model string (T-L3): the logged public_model is bounded even
// when the client sends a 16 KiB model name.
func TestModelLoggedTruncated(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, okJSON)
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	cfg := testConfig(f.server.URL)
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
	p, err := New(cfg, router, map[string]*backend.Client{"b1": client}, log, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	longModel := strings.Repeat("m", 16*1024)
	w := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"`+longModel+`"}`, testKey(longModel))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d (want 404)", w.Code)
	}
	var rec struct {
		PublicModel string `json:"public_model"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log is not a single JSON record: %v (%q)", err, buf.String())
	}
	if len(rec.PublicModel) > maxLoggedModelLen {
		t.Fatalf("logged model length = %d, exceeds bound %d", len(rec.PublicModel), maxLoggedModelLen)
	}
	if rec.PublicModel != longModel[:maxLoggedModelLen] {
		t.Fatalf("logged model = %q", rec.PublicModel)
	}
}

// TestClientDisconnectClassified verifies that a client that disconnects
// while the backend is still processing is logged as client cancellation
// (status 499, error_class client_canceled), not a server-side internal
// error. The class must not read as a 500 in operational logs (PLAN §43).
func TestClientDisconnectClassified(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	f := newFakeVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	var buf bytes.Buffer
	pLog := slog.New(slog.NewJSONHandler(&buf, nil))
	cfg := testConfig(f.server.URL)
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
	p, err := New(cfg, router, map[string]*backend.Client{"b1": client}, pLog, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gen-1","messages":[{"role":"user","content":"hi"}]}`))
	r = r.WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	q := &Req{W: w, R: r, Key: testKey(), RequestID: "req_test", Remote: "127.0.0.1"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ChatCompletions(q)
	}()
	<-started
	cancel() // the client goes away mid-request
	<-done

	var rec struct {
		Status int    `json:"status"`
		Class  string `json:"error_class"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log is not a single JSON record: %v (%q)", err, buf.String())
	}
	if rec.Status != 499 {
		t.Fatalf("status = %d, want 499", rec.Status)
	}
	if rec.Class != "client_canceled" {
		t.Fatalf("error_class = %q, want client_canceled", rec.Class)
	}
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

// FIX-03/N3: "stream": true on /v1/embeddings must not bypass token
// accounting. The backend answers with a plain application/json body that
// reports 1000 total tokens. A stream-flagged embeddings request is
// rejected 400 (OpenAI never streams embeddings), and the non-stream
// request is buffered (never fed to the SSE pump) and its usage is
// accounted.
func TestEmbeddingsStreamDoesNotBypassAccounting(t *testing.T) {
	t.Parallel()
	embJSON := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":5,"total_tokens":1000}}`))
	}
	f := newFakeVLLM(t, embJSON)

	accPath := filepath.Join(t.TempDir(), "usage.jsonl")
	acc, err := accounting.NewWriter(accounting.WriterConfig{Path: accPath, FSync: "never", Log: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = acc.Close() })

	cfg := testConfig(f.server.URL)
	router, err := routing.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := backend.New(backend.Options{
		Name: "b1", Cfg: cfg.Backends["b1"],
		Network:          backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes,
		Log:              discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), nil, acc, false)
	if err != nil {
		t.Fatal(err)
	}

	// stream=true is rejected outright: streaming is only valid for
	// generative endpoints, and accepting it here would let an
	// SSE-misrouted response charge zero tokens.
	rec := run(t, p, http.MethodPost, "/v1/embeddings",
		`{"model":"gen-1","input":"hi","stream":true}`, testKey())
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "stream_not_supported") {
		t.Fatalf("stream embeddings: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// stream=false is buffered and accounted: application/json (never
	// text/event-stream), and the backend's reported usage is charged.
	rec = run(t, p, http.MethodPost, "/v1/embeddings",
		`{"model":"gen-1","input":"hi"}`, testKey())
	if rec.Code != 200 {
		t.Fatalf("embeddings: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("embeddings content-type = %q, want application/json (never text/event-stream)", ct)
	}
	if err := acc.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(accPath)
	if err != nil {
		t.Fatal(err)
	}
	var recs []accounting.Record
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r accounting.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad accounting line: %v: %s", err, line)
		}
		recs = append(recs, r)
	}
	if len(recs) != 1 {
		t.Fatalf("accounting records = %d, want 1: %s", len(recs), string(data))
	}
	if recs[0].TotalTokens != 1000 || recs[0].ChargedTokens != 1000 {
		t.Fatalf("usage = %+v, want total/charged 1000", recs[0])
	}
	if recs[0].Endpoint != "embeddings" {
		t.Fatalf("endpoint = %q", recs[0].Endpoint)
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
	// Every proxy 429 carries Retry-After (T-Q12); this upstream sent
	// none, so the conservative default applies.
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 Retry-After header missing")
	}

	// An upstream that sends its own Retry-After has it forwarded on the
	// proxy 429 (T-Q12).
	f4 := newFakeVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	})
	p4 := newProxy(t, f4)
	rec = run(t, p4, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())
	if rec.Code != 429 {
		t.Fatalf("429 forwarding: status = %d", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "17" {
		t.Fatalf("Retry-After = %q (want upstream value 17)", ra)
	}

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

// TestUpstream502And503Sanitized (PLAN §84): upstream 502 and 503 both
// map to a sanitized proxy 502, never relaying the backend body. Both
// are on the PLAN §23 retry list, so they exercise the retry/fallback
// decision path as well as the sanitization path.
func TestUpstream502And503Sanitized(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		fn   http.HandlerFunc
	}{
		{"502", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(502)
			_, _ = w.Write([]byte(`{"error":{"message":"secret 502 detail"}}`))
		}},
		{"503", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"message":"secret 503 detail"}}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeVLLM(t, tc.fn)
			p := newProxy(t, f)
			rec := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())
			if rec.Code != 502 {
				t.Fatalf("status = %d (want 502)", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "secret") {
				t.Fatalf("backend body leaked: %s", rec.Body.String())
			}
			decodeErr(t, rec.Body.String(), 502)
		})
	}
}

// TestMalformedJSONRelayed (PLAN §84): a backend 200 whose body is not
// valid JSON is relayed verbatim — the proxy is a shallow passthrough —
// and the usage parser tolerates the malformed payload without erroring
// or panicking.
func TestMalformedJSONRelayed(t *testing.T) {
	t.Parallel()
	const malformed = `{"id":`
	f := newFakeVLLM(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(malformed))
	})
	p := newProxy(t, f)
	rec := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1"}`, testKey())
	if rec.Code != 200 {
		t.Fatalf("status = %d (want 200)", rec.Code)
	}
	if rec.Body.String() != malformed {
		t.Fatalf("body = %q, want verbatim %q", rec.Body.String(), malformed)
	}
}

// TestMalformedSSERelayed (PLAN §84): a backend stream whose bytes are
// not valid SSE framing must not crash the pump; the client still
// receives the raw bytes and the stream classifies as ok.
func TestMalformedSSERelayed(t *testing.T) {
	t.Parallel()
	const garbage = "this is not sse at all\nneither is this\n"
	f := newFakeVLLM(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(garbage))
	})
	p := newProxy(t, f)
	rec := run(t, p, http.MethodPost, "/v1/chat/completions", `{"model":"gen-1","stream":true}`, testKey())
	if rec.Code != 200 {
		t.Fatalf("status = %d (want 200)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "this is not sse") {
		t.Fatalf("malformed SSE bytes not relayed: %q", rec.Body.String())
	}
}

// TestPassthroughHeadersStripsSensitiveHeaders is the T-T1 regression
// test for the PLAN §18 allow-list: only User-Agent and Accept are
// end-to-end headers. Client auth/identity headers (Authorization,
// X-Api-Key, Proxy-Authorization, Forwarded/X-Forwarded-*, X-Real-IP)
// must be stripped at the proxy layer — backend.Forward is a pass-through
// by contract, so the proxy is the only enforcement point.
func TestPassthroughHeadersStripsSensitiveHeaders(t *testing.T) {
	t.Parallel()
	var got http.Header
	f := newFakeVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1"}`))
	})
	p := newProxy(t, f)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gen-1","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mellomting-client")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("X-Api-Key", "client-key")
	req.Header.Set("Proxy-Authorization", "Basic dXNlcjpwYXNz")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("Forwarded", "for=1.2.3.4")
	req.Header.Set("X-Real-IP", "1.2.3.4")
	w := httptest.NewRecorder()
	p.ChatCompletions(&Req{W: w, R: req, Key: testKey(), RequestID: "req_test", Remote: "127.0.0.1"})

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	// Both allow-list arms pass through.
	if got.Get("User-Agent") != "mellomting-client" {
		t.Errorf("User-Agent = %q, want pass-through", got.Get("User-Agent"))
	}
	if got.Get("Accept") != "application/json" {
		t.Errorf("Accept = %q, want pass-through", got.Get("Accept"))
	}
	// Sensitive headers must never reach the backend.
	for _, h := range []string{"Authorization", "X-Api-Key", "Proxy-Authorization", "X-Forwarded-For", "Forwarded", "X-Real-IP"} {
		if v := got.Get(h); v != "" {
			t.Errorf("sensitive header %s leaked to backend: %q", h, v)
		}
	}
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

// TestResponsesAffinityOversizedIDSkipped verifies a backend-supplied
// response id that is unbounded is not stored in the affinity table and
// cannot evict a legitimate record (T-L5).
func TestResponsesAffinityOversizedIDSkipped(t *testing.T) {
	t.Parallel()
	bigID := "resp_" + strings.Repeat("x", 900*1024)
	creates := 0
	f := newFakeVLLM(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			responsesJSON(w, r)
			return
		}
		creates++
		if creates == 1 {
			_, _ = w.Write([]byte(`{"id":"` + bigID + `","object":"response","status":"completed"}`))
			return
		}
		responsesJSON(w, r)
	})
	p := newProxy(t, f)
	key := testKey()

	// First create returns an oversized id: it must not be stored.
	rec := run(t, p, http.MethodPost, "/v1/responses", `{"model":"gen-1","input":"hi"}`, key)
	if rec.Code != 200 {
		t.Fatalf("create status = %d", rec.Code)
	}
	if _, ok := p.affinity.Get(key.ID, bigID); ok {
		t.Fatalf("oversized id was stored in the affinity table")
	}

	// A legitimate subsequent create still records normally (the
	// oversized id did not evict or corrupt anything).
	rec = run(t, p, http.MethodPost, "/v1/responses", `{"model":"gen-1","input":"hi"}`, key)
	if rec.Code != 200 {
		t.Fatalf("second create status = %d", rec.Code)
	}
	if b, ok := p.affinity.Get(key.ID, "resp_123"); !ok || b != "b1" {
		t.Fatalf("legitimate affinity lost after oversized id: ok=%v b=%q", ok, b)
	}
}

// T-T9: the affinity table's bounded-evil eviction (PLAN §21.4) must
// actually evict: a full table evicts the entry with the oldest expiry,
// expired records drop on access, and expired records are swept before a
// Put adds to an at-capacity table.
func TestAffinityEviction(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	a := newAffinity(time.Second, 2)
	a.now = func() time.Time { return now }

	// A third Put past the bound evicts the oldest (resp_1).
	a.Put("K1", "resp_1", "b1")
	now = now.Add(100 * time.Millisecond)
	a.Put("K1", "resp_2", "b2")
	now = now.Add(100 * time.Millisecond)
	a.Put("K1", "resp_3", "b1")
	if _, ok := a.Get("K1", "resp_1"); ok {
		t.Fatal("resp_1 was not evicted when the table exceeded its bound")
	}
	if _, ok := a.Get("K1", "resp_2"); !ok {
		t.Fatal("resp_2 was wrongly evicted")
	}

	// TTL expiry on access: after the TTL passes, a record is not found.
	now = now.Add(2 * time.Second)
	if _, ok := a.Get("K1", "resp_2"); ok {
		t.Fatal("resp_2 still present after TTL expiry")
	}
	if _, ok := a.Get("K1", "resp_3"); ok {
		t.Fatal("resp_3 still present after TTL expiry")
	}

	// Sweep-on-Put: fill the table again, let both records expire, then
	// Put — the expired entries are swept before the new record lands.
	a.Put("K1", "resp_4", "b1")
	now = now.Add(100 * time.Millisecond)
	a.Put("K1", "resp_5", "b2")
	now = now.Add(2 * time.Second)
	a.Put("K1", "resp_6", "b1")
	if _, ok := a.Get("K1", "resp_4"); ok {
		t.Fatal("expired resp_4 not swept on Put")
	}
	if _, ok := a.Get("K1", "resp_5"); ok {
		t.Fatal("expired resp_5 not swept on Put")
	}
	if b, ok := a.Get("K1", "resp_6"); !ok || b != "b1" {
		t.Fatalf("fresh Put after sweep: ok=%v b=%q", ok, b)
	}
}

// TestNullBodyNoNilMapPanic covers the T-L6 hardening: a JSON `null`
// body must never reach a nil-map write, and the dispatch must return a
// clean 400 rather than crash the process.
func TestNullBodyNoNilMapPanic(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, okJSON)
	p := newProxy(t, f)

	// Dispatch-level: `null` is not a JSON object; shallow-parse rejects
	// it with missing_model (400), never a nil-map panic.
	rec := run(t, p, http.MethodPost, "/v1/chat/completions", `null`, testKey())
	if rec.Code != 400 {
		t.Fatalf("null body: status = %d (want 400)", rec.Code)
	}

	// Unit-level: the normalize helpers must fail closed on `null`
	// rather than write to a nil map (T-L6).
	if _, _, _, err := prepareOutbound([]byte("null"), opChat, 0, false, false, 0); !errors.Is(err, errNotJSONObject) {
		t.Fatalf("prepareOutbound(null): err = %v (want errNotJSONObject)", err)
	}
	if _, err := rewriteModel([]byte("null"), "Up/Model"); err == nil {
		t.Fatalf("rewriteModel(null) returned a nil error")
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
	// truncated rather than buffered unboundedly. The fixture's many short
	// lines aggregate past maxSSEEvent, so the event bound (not the line
	// bound) is what truncates the stream (T-T7).
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
	p, err := New(cfg, router, map[string]*backend.Client{"b1": client}, discardLogger(), nil, nil, false)
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

// FIX-12: a live SSE stream is bounded by a cumulative emitted-byte cap
// (server.max_response_bytes, the same bound as the buffered path), so a
// backend emitting small events forever cannot stream unbounded data. On
// breach the stream terminates with backend_stream_error and only a
// bounded prefix reaches the client.
func TestStreamCumulativeBound(t *testing.T) {
	event := `data: {"id":"c","choices":[{"delta":{"content":"x"}}]}` + "\n\n"

	// Over the cap: the stream is cut off; the reader is never drained.
	pr, pw := io.Pipe()
	var sent atomic.Int64
	go func() {
		defer pw.Close()
		for i := 0; i < 10000; i++ {
			sent.Add(1)
			if _, err := io.WriteString(pw, event); err != nil {
				return
			}
		}
	}()
	p := newProxy(t, newFakeVLLM(t, okJSON))
	p.cfg.Server.MaxResponseBytes = 2000
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gen-1","stream":true}`))
	w := httptest.NewRecorder()
	q := &Req{W: w, R: r, Key: testKey(), RequestID: "req_budget", Remote: "127.0.0.1"}
	res := &backend.Result{Status: http.StatusOK, Body: pr}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	status, bytesOut, cls := p.pump(q, res, opChat, cancel, "b1", &accounting.Usage{}, false)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if cls != "backend_stream_error" {
		t.Fatalf("class = %q, want backend_stream_error", cls)
	}
	if bytesOut > 2000 {
		t.Fatalf("emitted %d bytes, want <= 2000", bytesOut)
	}
	if sent.Load() >= 10000 {
		t.Fatalf("stream drained to completion (%d events); budget never applied", sent.Load())
	}

	// Under the cap: an ordinary stream is unaffected.
	pr2, pw2 := io.Pipe()
	go func() {
		defer pw2.Close()
		for i := 0; i < 3; i++ {
			_, _ = io.WriteString(pw2, event)
		}
		_, _ = io.WriteString(pw2, `data: [DONE]`+"\n\n")
	}()
	p2 := newProxy(t, newFakeVLLM(t, okJSON))
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gen-1","stream":true}`))
	w2 := httptest.NewRecorder()
	q2 := &Req{W: w2, R: r2, Key: testKey(), RequestID: "req_budget2", Remote: "127.0.0.1"}
	res2 := &backend.Result{Status: http.StatusOK, Body: pr2}
	_, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	status2, _, cls2 := p2.pump(q2, res2, opChat, cancel2, "b1", &accounting.Usage{}, false)
	if status2 != http.StatusOK || cls2 != "ok" {
		t.Fatalf("under-cap stream: status=%d class=%q, want 200/ok", status2, cls2)
	}
}

// errWriteTimeout is the synthetic write error a stalled client socket
// yields once the conn write deadline fires.
var errWriteTimeout = errors.New("write tcp: i/o timeout")

// stallWriter simulates a client whose socket accepts `limit` writes and
// then fills: further writes block until the armed conn write deadline
// (stream_write_timeout) fires, then fail. When no deadline is armed the
// write blocks unboundedly (the bug the deadline exists to prevent).
type stallWriter struct {
	hdr      http.Header
	code     int
	mu       sync.Mutex
	deadline time.Time
	writes   int
	limit    int
}

func (s *stallWriter) Header() http.Header { return s.hdr }
func (s *stallWriter) WriteHeader(c int)   { s.code = c }

func (s *stallWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.writes < s.limit {
		s.writes++
		s.mu.Unlock()
		return len(p), nil
	}
	dl := s.deadline
	s.mu.Unlock()
	if dl.IsZero() {
		time.Sleep(10 * time.Second) // no deadline armed: the write is unbounded
		return 0, errWriteTimeout
	}
	time.Sleep(time.Until(dl) + 10*time.Millisecond)
	return 0, errWriteTimeout
}

func (s *stallWriter) Flush() {}

// SetWriteDeadline is found by http.ResponseController via Unwrap and
// records the conn write deadline the pump arms before each event write.
func (s *stallWriter) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	s.deadline = t
	s.mu.Unlock()
	return nil
}

func (s *stallWriter) Unwrap() http.ResponseWriter { return s }

// TestPumpClientWriteDeadlineBounded is the streaming half of T-X9 at the
// pump level: once a stalled client's socket fills, the next event write
// must block at most stream_write_timeout and then fail, which the pump
// must treat as terminal (client_write_error) while cancelling the
// upstream (PLAN §9.1, §24). It is deterministic: the stalled socket is
// a controllable writer, not kernel buffer timing.
func TestPumpClientWriteDeadlineBounded(t *testing.T) {
	t.Parallel()
	p := newProxy(t, newFakeVLLM(t, okJSON))
	p.cfg.Server.StreamWriteTimeout = config.Duration(100 * time.Millisecond)

	pr, pw := io.Pipe()
	defer pr.Close()
	go func() {
		ev := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"c\"}}]}\n\n"
		for i := 0; i < 5; i++ {
			if _, err := pw.Write([]byte(ev)); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()

	sw := &stallWriter{hdr: http.Header{}, limit: 1} // first event fits, then the socket fills
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gen-1","stream":true}`))
	q := &Req{W: sw, R: r, Key: testKey(), RequestID: "req_test", Remote: "127.0.0.1"}
	res := &backend.Result{Status: http.StatusOK, Body: pr}
	cancelled := false
	cancel := func() { cancelled = true }

	start := time.Now()
	status, bytesOut, cls := p.pump(q, res, opChat, cancel, "b1", &accounting.Usage{}, false)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if cls != "client_write_error" {
		t.Fatalf("class = %q, want client_write_error", cls)
	}
	if !cancelled {
		t.Fatal("upstream was not cancelled after the stalled client write")
	}
	if bytesOut == 0 {
		t.Fatal("expected at least the first event to reach the client")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("stalled client write was not bounded by stream_write_timeout: elapsed = %v", elapsed)
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
	p3, _ := New(cfg3, router3, map[string]*backend.Client{"b1": cl3}, discardLogger(), nil, nil, false)
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

// --- T-X12: byte budget reserved before the body is read ----------------

// TestBudgetWeightsAndOverload pins the weighted byte-budget semantics
// (T-X12): per-request cap, aggregate cap, and Release restoring capacity.
func TestBudgetWeightsAndOverload(t *testing.T) {
	t.Parallel()
	b := newBudget(64 * 1024 * budgetWeight)
	for i := 0; i < 16; i++ {
		if !b.Acquire(4 * 1024) {
			t.Fatalf("acquire %d: unexpected overload", i)
		}
	}
	if b.Acquire(4 * 1024) {
		t.Fatal("aggregate overload not detected")
	}
	b.Release(4 * 1024)
	if !b.Acquire(4 * 1024) {
		t.Fatal("release did not restore budget capacity")
	}
	if b.Acquire(128 * 1024) {
		t.Fatal("per-request cap not enforced (bodyBytes > total/budgetWeight)")
	}
}

// panicReader fails the test if the body is ever read.
type panicReader struct{}

func (panicReader) Read([]byte) (int, error) {
	panic("body was read despite the budget rejecting the request")
}

// TestBodyBudgetRejectsBeforeRead is the T-X12 regression test: a request
// whose known body size cannot fit the budget must receive a clean 503
// before a single body byte is read or decoded, and must never reach the
// backend.
func TestBodyBudgetRejectsBeforeRead(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, okJSON)
	p := newProxy(t, f) // MaxBufferedRequestBytes=1MiB × 8 inflight → per-request cap 512KiB

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.ContentLength = 1 << 20 // 1 MiB: over the 512 KiB per-request budget cap
	r.Body = io.NopCloser(panicReader{})
	w := httptest.NewRecorder()
	p.ChatCompletions(&Req{W: w, R: r, Key: testKey(), RequestID: "req_t", Remote: "x"})

	if w.Code != 503 {
		t.Fatalf("status = %d (want 503)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "server_overloaded") {
		t.Fatalf("body = %q", w.Body.String())
	}
	if f.lastPath != "" {
		t.Fatalf("backend saw request (path %q) despite budget rejection", f.lastPath)
	}
}

// blockingReader holds its first Read until release is closed, then
// serves data. ContentLength is unknown for an arbitrary io.Reader, so a
// request built on it is chunked (FIX-15).
type blockingReader struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	data    []byte
}

func (b *blockingReader) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	if len(b.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

// FIX-15 / PLAN §12.1: an unknown-length (chunked) body must reserve the
// worst case (max_body_bytes) up front, so a body that cannot fit the
// budget is rejected before the read and the concurrent budget is
// unavailable to other requests while a chunked body is being read.
func TestChunkedBodyBudgetReservedBeforeRead(t *testing.T) {
	t.Parallel()

	t.Run("rejected before the full read", func(t *testing.T) {
		f := newFakeVLLM(t, okJSON)
		p := newProxy(t, f) // total=8MiB → per-request cap 512KiB; maxBody=1MiB
		// A chunked body reserves max_body_bytes = 1MiB (16 MiB weighted)
		// up front, which exceeds the 8 MiB budget: rejected with a clean
		// 503 before a single byte is read (the reader would panic).
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", panicReader{})
		w := httptest.NewRecorder()
		p.ChatCompletions(&Req{W: w, R: r, Key: testKey(), RequestID: "req_t", Remote: "x"})
		if w.Code != 503 {
			t.Fatalf("status = %d (want 503)", w.Code)
		}
		if !strings.Contains(w.Body.String(), "server_overloaded") {
			t.Fatalf("body = %q", w.Body.String())
		}
		if f.lastPath != "" {
			t.Fatalf("backend saw request (path %q) despite budget rejection", f.lastPath)
		}
	})

	t.Run("budget unavailable during the read", func(t *testing.T) {
		f := newFakeVLLM(t, okJSON)
		p := newProxy(t, f)
		// 16 MiB weighted total; maxBody=1MiB reserves the whole budget, so
		// while a chunked body is being read no other request can acquire.
		p.budget = newBudget(16 << 20)
		p.cfg.Server.MaxBodyBytes = 1 << 20

		br := &blockingReader{
			entered: make(chan struct{}),
			release: make(chan struct{}),
			data:    []byte(`{"model":"gen-1","messages":[{"role":"user","content":"hi"}]}`),
		}
		r1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", br)
		w1 := httptest.NewRecorder()
		done1 := make(chan struct{})
		go func() {
			defer close(done1)
			p.ChatCompletions(&Req{W: w1, R: r1, Key: testKey(), RequestID: "req_a", Remote: "x"})
		}()
		select {
		case <-br.entered:
			// Now inside the body read with the full budget held.
		case <-time.After(3 * time.Second):
			t.Fatal("first request never entered the body read")
		}

		// A known-size request that would fit an idle budget must 503 while
		// the chunked read holds the reservation.
		w2 := run(t, p, http.MethodPost, "/v1/chat/completions",
			`{"model":"gen-1","messages":[{"role":"user","content":"hi"}]}`, testKey())
		if w2.Code != 503 {
			t.Fatalf("second request during chunked read: %d (want 503)", w2.Code)
		}

		close(br.release)
		select {
		case <-done1:
		case <-time.After(3 * time.Second):
			t.Fatal("first request never completed after the read was released")
		}
		if w1.Code != 200 {
			t.Fatalf("first request: %d (want 200)", w1.Code)
		}
	})
}

// TestManyShortKeysBodyRejected reproduces the T-X12 finding: a body of
// many short keys (whose decode+re-encode peaks at ~13× body) is rejected
// by the pre-read budget with a clean 503 before the costly decode, and
// never reaches the backend.
func TestManyShortKeysBodyRejected(t *testing.T) {
	t.Parallel()
	f := newFakeVLLM(t, okJSON)
	p := newProxy(t, f)

	var b strings.Builder
	b.WriteString(`{"model":"gen-1","messages":[{"role":"user","content":"x"}]`)
	for i := 0; i < 60000; i++ {
		fmt.Fprintf(&b, ",\"k%d\":%d", i, i)
	}
	b.WriteString("}")
	body := b.String()
	if len(body) < 512*1024 {
		t.Fatalf("test body too small (%d); budget cap is 512 KiB", len(body))
	}
	if len(body) > p.cfg.Server.MaxBodyBytes {
		t.Fatalf("test body (%d) exceeds MaxBodyBytes (%d)", len(body), p.cfg.Server.MaxBodyBytes)
	}

	rec := run(t, p, http.MethodPost, "/v1/chat/completions", body, testKey())
	if rec.Code != 503 {
		t.Fatalf("status = %d (want 503); backend path=%q", rec.Code, f.lastPath)
	}
	if !strings.Contains(rec.Body.String(), "server_overloaded") {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if f.lastPath != "" {
		t.Fatal("many-short-keys body reached the backend")
	}
}

// Ensure the context-cancellation path compiles and is referenced.
var _ = context.Canceled
