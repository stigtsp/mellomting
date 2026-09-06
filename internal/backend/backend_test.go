package backend

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"mellomting/internal/testsupport"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mellomting/internal/config"
	"mellomting/internal/sandbox"
)

// TestDialerPortAgreesWithLandlock (T-Q6): for an omitted-port base
// URL, the port the dialer derives for a scheme must be exactly the
// port the Landlock sandbox grants for that scheme. Both now read
// sandbox.DefaultPortForScheme, so a drift here is a regression in the
// single source of truth.
func TestDialerPortAgreesWithLandlock(t *testing.T) {
	for _, tc := range []struct{ scheme, want string }{
		{"http", "80"},
		{"https", "443"},
	} {
		u, err := url.Parse(tc.scheme + "://example.test")
		if err != nil {
			t.Fatal(err)
		}
		_, port := splitHostPort(u)
		if port != tc.want {
			t.Fatalf("%s: dialer default port = %q, want %q", tc.scheme, port, tc.want)
		}
		granted, err := sandbox.BackendPorts(tc.scheme + "://example.test")
		if err != nil {
			t.Fatal(err)
		}
		if len(granted) != 1 || strconv.Itoa(int(granted[0])) != tc.want {
			t.Fatalf("%s: landlock granted ports %v, want [%s]", tc.scheme, granted, tc.want)
		}
	}
}

func testOptions(t *testing.T, base string) Options {
	t.Helper()
	return Options{
		Name:             "test",
		Cfg:              config.Backend{BaseURL: base, MaxConcurrency: 2, QueueSize: 2, QueueTimeout: config.Duration(200 * time.Millisecond), ConnectTimeout: config.Duration(time.Second), HeaderTimeout: config.Duration(2 * time.Second), RequestTimeout: config.Duration(5 * time.Second), StreamIdleTimeout: config.Duration(time.Second)},
		Network:          Policy{Mode: "loopback-only"},
		MaxResponseBytes: 1 << 20,
		Log:              slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
}

func newTestClient(t *testing.T, ts *httptest.Server) *Client {
	t.Helper()
	c, err := New(testOptions(t, ts.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestAcquireQueueFullClassifiesDisconnect proves that a client
// disconnect while the backend admission queue is full is reported as
// the context error (disconnect), not ErrQueueFull (T-L14): the two
// have different operational meanings for capacity planning. The second
// acquire select races a ready timer (queue_full) against a ready
// client-cancellation, and must report the disconnect.
func TestAcquireQueueFullClassifiesDisconnect(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the handler open so the concurrency slot stays taken.
		select {}
	}))
	defer ts.Close()

	opts := testOptions(t, ts.URL)
	opts.Cfg.MaxConcurrency = 1
	opts.Cfg.QueueSize = 1
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Hold the single concurrency slot so every later acquire waits on
	// the queue timeout.
	h, err := c.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire holder: %v", err)
	}
	defer h.release()

	// A zero queue timeout makes the timer ready immediately, so each
	// acquire's second select races the fired timer against the
	// cancelled client context.
	c.queueTimeout = 0

	for i := range 200 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := c.acquire(ctx); err == nil {
			t.Fatalf("iteration %d: acquire succeeded with full queue, want error", i)
		} else if errors.Is(err, context.Canceled) {
			// correct: disconnect wins over queue_full
		} else if errors.Is(err, ErrQueueFull) {
			t.Fatalf("iteration %d: disconnect while queue full classified as queue_full (T-L14)", i)
		} else {
			t.Fatalf("iteration %d: unexpected error %v", i, err)
		}
	}
}

func TestForwardNonStream(t *testing.T) {
	t.Parallel()

	var gotAuth, gotXAPIKey, gotUA, gotHost string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXAPIKey = r.Header.Get("X-Api-Key")
		gotUA = r.Header.Get("User-Agent")
		gotHost = r.Host
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1"}`))
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	res, err := c.Forward(context.Background(), Request{
		Method: "POST",
		Path:   "/v1/chat/completions",
		Body:   []byte(`{"model":"m"}`),
		Headers: map[string][]string{
			"User-Agent":          {"mellomting-test"},
			"Accept":              {"application/json"},
			"Authorization":       {"Bearer client-secret"},
			"X-Api-Key":           {"client-key"},
			"Proxy-Authorization": {"Basic dXNlcjpwYXNz"},
			"X-Forwarded-For":     {"1.2.3.4"},
		},
		Stream: false,
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if res.Status != 200 || string(res.BodyBytes) != `{"id":"chatcmpl-1"}` {
		t.Fatalf("res = %+v", res)
	}
	// Forward is a pass-through by contract (PLAN §18): the caller's
	// header map reaches the backend verbatim and Host is rewritten to
	// the backend. Stripping of client auth/identity headers is the
	// proxy layer's job (proxy.passthroughHeaders); they must never be
	// placed in a Request there (T-T1). These assertions document the
	// pass-through contract so the earlier vacuous "not forwarded"
	// checks (which never sent these headers) cannot regress.
	if gotUA != "mellomting-test" {
		t.Errorf("User-Agent = %q", gotUA)
	}
	if gotXAPIKey != "client-key" {
		t.Errorf("X-Api-Key = %q, want pass-through", gotXAPIKey)
	}
	// No api_key_file is configured, so the Authorization that arrived
	// is the caller's pass-through value, not a backend injection.
	if gotAuth != "Bearer client-secret" {
		t.Errorf("Authorization = %q, want pass-through (no injection)", gotAuth)
	}
	if gotHost != ts.Listener.Addr().String() {
		t.Errorf("Host = %q, want backend host", gotHost)
	}
}

func TestForwardInjectsBackendAuth(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "backend.key")
	if err := os.WriteFile(keyFile, []byte("  backend-secret-123 \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	o := testOptions(t, ts.URL)
	o.Cfg.APIKeyFile = keyFile
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/embeddings", Body: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer backend-secret-123" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}

func TestForwardRejectsWorldReadableKeyFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "backend.key")
	if err := os.WriteFile(keyFile, []byte("backend-secret-123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := testOptions(t, "http://127.0.0.1:8001")
	o.Cfg.APIKeyFile = keyFile
	if _, err := New(o); err == nil {
		t.Fatal("world-readable api_key_file accepted (T-M8)")
	}
	if err := os.Chmod(keyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(o); err != nil {
		t.Fatalf("0600 api_key_file rejected: %v", err)
	}
}

// Redirects are never followed (X3): the backend must not steer the
// connection elsewhere, and the Authorization header must never be
// re-sent to a redirect target.
func TestForwardDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "backend.key")
	if err := os.WriteFile(keyFile, []byte("backend-secret-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The redirect target: if it is ever contacted, it records the
	// fact and any credential it received.
	var targetHits, targetAuth atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		if a := r.Header.Get("Authorization"); a != "" {
			targetAuth.Add(1)
		}
		_, _ = w.Write([]byte(`{"target":"reachable"}`))
	}))
	defer target.Close()

	// The backend that answers with a redirect away from itself.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL+"/metrics")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte(`<a href="/metrics">moved</a>`))
	}))
	defer origin.Close()

	o := testOptions(t, origin.URL)
	o.Cfg.APIKeyFile = keyFile
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`)})
	if err == nil {
		t.Fatal("redirect: expected an upstream error, got nil")
	}
	var up *Upstream
	if !errors.As(err, &up) || up.Status != http.StatusFound {
		t.Fatalf("err = %v (want *Upstream{302})", err)
	}
	if res == nil || res.Status != http.StatusFound {
		t.Fatalf("result = %+v (want status 302)", res)
	}
	// The redirect target must never have been contacted, so no
	// credential could leak to it.
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("redirect target was contacted %d times", n)
	}
	if n := targetAuth.Load(); n != 0 {
		t.Fatalf("Authorization leaked to redirect target %d times", n)
	}
}

func TestForwardUpstreamErrors(t *testing.T) {
	t.Parallel()

	status := 500
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("internal secret stuff"))
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	res, err := c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`)})
	var up *Upstream
	if !errors.As(err, &up) || up.Status != 500 {
		t.Fatalf("err = %v (want upstream 500)", err)
	}
	if res == nil || res.Status != 500 {
		t.Fatalf("res = %+v", res)
	}
	// The buffered body is available internally but the class is sanitized:
	if strings.Contains(up.Error(), "secret") {
		t.Fatalf("upstream error leaks body: %q", up.Error())
	}
}

// T-T9: a non-streaming upstream body that exceeds the configured
// MaxResponseBytes bound must fail closed with ErrTooLarge (PLAN §22):
// the oversized payload is never trusted or relayed.
func TestForwardMaxResponseBytesTooLarge(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"big":"` + strings.Repeat("x", 8192) + `"}`))
	}))
	defer ts.Close()

	opts := testOptions(t, ts.URL)
	opts.MaxResponseBytes = 1024
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`)})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v (want ErrTooLarge), res = %+v", err, res)
	}
	if res != nil {
		t.Fatalf("res must be nil on ErrTooLarge, got %+v", res)
	}

	// An in-bounds body still succeeds on the same client.
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer ts2.Close()
	opts2 := testOptions(t, ts2.URL)
	opts2.MaxResponseBytes = 1024
	c2, err := New(opts2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res2, err := c2.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`)})
	if err != nil || res2.Status != 200 || string(res2.BodyBytes) != `{"id":"ok"}` {
		t.Fatalf("in-bounds body: err=%v res=%+v", err, res2)
	}
}

// PLAN §23: a dead port is a connection failure (retryable class), and
// a silent server is a header timeout, kept distinct from total/body
// timeouts.
func TestForwardTimeoutClassification(t *testing.T) {
	t.Parallel()

	// Refused connection.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()
	c1, err := New(testOptions(t, dead))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c1.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`)}); !errors.Is(err, ErrConnect) {
		t.Fatalf("dead port: err = %v (want ErrConnect)", err)
	}

	// Accepted, but no headers within header_timeout. header_timeout
	// is the streaming header bound (X6: non-streaming headers arrive
	// at completion and are bounded by request_timeout instead), so
	// this must be a streaming request.
	never := make(chan struct{})
	defer close(never)
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-never:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(silent.Close)
	o := testOptions(t, silent.URL)
	o.Cfg.HeaderTimeout = config.Duration(200 * time.Millisecond)
	c2, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c2.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`), Stream: true})
	if !errors.Is(err, ErrHeaderTimeout) {
		t.Fatalf("silent server (stream): err = %v (want ErrHeaderTimeout)", err)
	}
	// T-T5: ErrHeaderTimeout and ErrTimeout are distinct bare sentinels,
	// so a negation of errors.Is(err, ErrTimeout) right after the
	// ErrHeaderTimeout assertion above is a tautology and proved
	// nothing. The meaningful classification is asserted directly in
	// TestRequestErrorClassification below.
}

// timeoutErr implements net.Error with Timeout() true, standing in for
// the net/http client's response-header deadline so requestError can be
// tested without a live silent server.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// TestRequestErrorClassification pins the requestError/bodyReadError
// taxonomy (T-T5): a streaming request's header bound is its own class
// (ErrHeaderTimeout), distinct from the non-streaming total deadline
// (ErrTimeout), so the proxy can apply different retry rules (PLAN
// §23). Testing the classification functions directly with
// representative errors makes the assertion meaningful — it fails when
// a classification is wrong, unlike the old tautological errors.Is
// negation between two unrelated bare sentinels.
func TestRequestErrorClassification(t *testing.T) {
	t.Parallel()
	timedOut := &net.OpError{Err: timeoutErr{}}
	cases := []struct {
		name   string
		fn     func(error, bool) error
		stream bool
		err    error
		want   error
	}{
		{"streaming header bound", requestError, true, timedOut, ErrHeaderTimeout},
		{"streaming deadline is still a header bound", requestError, true, context.DeadlineExceeded, ErrHeaderTimeout},
		{"non-streaming total deadline", requestError, false, context.DeadlineExceeded, ErrTimeout},
		{"non-streaming net timeout", requestError, false, timedOut, ErrTimeout},
		{"canceled", requestError, false, context.Canceled, context.Canceled},
		{"plain connect", requestError, false, errors.New("boom"), ErrConnect},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.fn(tc.err, tc.stream); got != tc.want {
				t.Fatalf("%s(%v, stream=%v) = %v, want %v", tc.name, tc.err, tc.stream, got, tc.want)
			}
		})
	}
}

// TestBodyReadErrorClassification pins bodyReadError: any timeout while
// draining a buffered body is the total/body timeout class (ErrTimeout),
// never the header bound, and a dropped connection stays ErrConnect
// (PLAN §23).
func TestBodyReadErrorClassification(t *testing.T) {
	t.Parallel()
	timedOut := &net.OpError{Err: timeoutErr{}}
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"deadline", context.DeadlineExceeded, ErrTimeout},
		{"net timeout", timedOut, ErrTimeout},
		{"dropped connection", errors.New("boom"), ErrConnect},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := bodyReadError(tc.err); got != tc.want {
				t.Fatalf("bodyReadError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// X6: a non-streaming generation that takes longer than header_timeout
// but within request_timeout must SUCCEED (headers arrive only at
// completion, so header_timeout must not truncate it), while the same
// backend as a streaming request still respects header_timeout.
func TestNonStreamingLongerThanHeaderTimeoutSucceeds(t *testing.T) {
	t.Parallel()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-slow","object":"chat.completion"}`))
	}))
	defer slow.Close()

	o := testOptions(t, slow.URL)
	o.Cfg.HeaderTimeout = config.Duration(200 * time.Millisecond) // 200ms header bound
	o.Cfg.RequestTimeout = config.Duration(5 * time.Second)       // 5s completion bound
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}

	res, err := c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`)})
	if err != nil {
		t.Fatalf("non-streaming > header_timeout: err = %v (want success within request_timeout)", err)
	}
	if res == nil || res.Status != 200 || !strings.Contains(string(res.BodyBytes), "chatcmpl-slow") {
		t.Fatalf("res = %+v", res)
	}

	// The same backend as a streaming request must still hit the
	// header bound (a silent stream is a header timeout). The handler
	// is released via defer before the server is closed (cleanup), so
	// Server.Close never waits on a parked handler.
	silent := make(chan struct{})
	defer close(silent)
	never := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-silent:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(never.Close)
	o2 := testOptions(t, never.URL)
	o2.Cfg.HeaderTimeout = config.Duration(200 * time.Millisecond)
	c2, err := New(o2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`), Stream: true}); !errors.Is(err, ErrHeaderTimeout) {
		t.Fatalf("streaming header bound: err = %v (want ErrHeaderTimeout)", err)
	}
}

// R1 (FIX-03 eval): a stream-flagged request answered with a plain 200
// is buffered, and that buffered read must be bounded by request_timeout
// (keyed off liveness, not the client's stream flag). Before the fix the
// streaming branch set no deadline and the SSE pump never ran, so a
// stalled backend held the request — and its admission slots — until the
// client hung up.
func TestStreamFlaggedBufferedResponseBoundedByRequestTimeout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion"}`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	t.Cleanup(stall.Close)

	o := testOptions(t, stall.URL)
	o.Cfg.HeaderTimeout = config.Duration(200 * time.Millisecond)
	o.Cfg.RequestTimeout = config.Duration(300 * time.Millisecond)
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = c.Forward(context.Background(), Request{
		Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`), Stream: true,
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout (buffered read of a stream-flagged request)", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("bounded buffered read took %v, want ~request_timeout (300ms)", d)
	}
}

// T-T5: a non-streaming request that exceeds request_timeout is the
// total-deadline class (ErrTimeout), NOT the streaming header bound
// (ErrHeaderTimeout). The http client surfaces both bounds as a net.Error
// timeout (context.DeadlineExceeded implements net.Error), so the
// classification depends on the request being non-streaming; this is the
// end-to-end check that the corrected classification is observable.
func TestNonStreamingTotalTimeoutClassified(t *testing.T) {
	t.Parallel()

	hold := make(chan struct{})
	defer close(hold)
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(silent.Close)

	o := testOptions(t, silent.URL)
	o.Cfg.RequestTimeout = config.Duration(200 * time.Millisecond)
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`)})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("non-streaming total timeout: err = %v (want ErrTimeout)", err)
	}
}

// PLAN §19: Inflight reports admission load so least-inflight routing
// can see queued and active requests.
// T-L7: the backend client bounds per-host connections, bounds response
// headers, and disables transparent compression (PLAN §9.2).
func TestBackendClientTransportBounds(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	c := newTestClient(t, ts)

	for name, hc := range map[string]*http.Client{"stream": c.http, "plain": c.httpPlain} {
		tr, ok := hc.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s: transport is %T", name, hc.Transport)
		}
		if tr.MaxConnsPerHost != 2 {
			t.Fatalf("%s: MaxConnsPerHost = %d (want 2 = MaxConcurrency)", name, tr.MaxConnsPerHost)
		}
		if tr.MaxIdleConnsPerHost != 2 {
			t.Fatalf("%s: MaxIdleConnsPerHost = %d (want 2 = MaxConcurrency, M18)", name, tr.MaxIdleConnsPerHost)
		}
		if tr.MaxResponseHeaderBytes != maxBackendResponseHeaderBytes {
			t.Fatalf("%s: MaxResponseHeaderBytes = %d", name, tr.MaxResponseHeaderBytes)
		}
		if !tr.DisableCompression {
			t.Fatalf("%s: DisableCompression = false (want true, PLAN §9.2)", name)
		}
	}
}

// M18: the idle connection pool matches the per-host concurrency cap, so
// bursty load reuses connections instead of tearing down and
// re-handshaking (PLAN §9.1 fd bound). With 4 workers and an idle pool
// of 4, 64 sequential-ish requests must reuse the 4 connections rather
// than open one per request.
func TestBackendConnectionReuse(t *testing.T) {
	var conns atomic.Int64
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	ts.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	ts.Start()
	defer ts.Close()

	opts := testOptions(t, ts.URL)
	opts.Cfg.MaxConcurrency = 4
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const workers = 4
	const perWorker = 16
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range perWorker {
				res, err := c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`)})
				if err != nil {
					t.Errorf("Forward: %v", err)
					return
				}
				res.Close()
			}
		})
	}
	wg.Wait()

	total := int(conns.Load())
	if total > workers {
		t.Fatalf("connections established = %d (want <= %d = concurrency cap: keep-alive reuse)", total, workers)
	}
}

// PLAN §22: the admission queue must never gate idle concurrency. A burst
// of arrivals is admitted up to max_concurrency even when queue_size is
// smaller, because a free concurrency slot is taken directly without a
// queue token. Regression for the bug where the queue was the first gate,
// so a big-box config (large max_concurrency, small queue_size) rejected
// arrivals while capacity sat idle.
func TestAcquireIdleConcurrencyIgnoresQueue(t *testing.T) {
	release := make(chan struct{})
	admitted := make(chan struct{}, 16)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admitted <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	// max_concurrency=8 with queue_size=1: the queue is much smaller than
	// the concurrency ceiling, so the old gate (queue first) would reject
	// everything beyond a single arrival.
	o := testOptions(t, ts.URL)
	o.Cfg.MaxConcurrency = 8
	o.Cfg.QueueSize = 1
	o.Cfg.QueueTimeout = config.Duration(200 * time.Millisecond)
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}

	const arrivals = 8
	start := make(chan struct{})
	errs := make(chan error, arrivals)
	for range arrivals {
		go func() {
			<-start
			res, err := c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/embeddings", Body: []byte(`{}`)})
			if res != nil {
				res.Close()
			}
			errs <- err
		}()
	}
	close(start)

	// Every arrival must be admitted (reach the handler): reaching the
	// handler proves it passed admission, so waiting for all `arrivals`
	// proves idle concurrency is never gated by queue_size.
	for i := range arrivals {
		select {
		case <-admitted:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d/%d arrivals admitted while concurrency idle", i, arrivals)
		}
	}
	close(release)

	// None of the admitted requests may report an admission error.
	for range arrivals {
		if err := <-errs; err != nil {
			t.Fatalf("admitted arrival errored: %v", err)
		}
	}
}

func TestInflightSnapshot(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	if c.Inflight() != 0 {
		t.Fatalf("fresh Inflight = %d, want 0", c.Inflight())
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`)})
		if err == nil && res != nil {
			res.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request never reached the backend")
	}
	if n := c.Inflight(); n < 1 {
		t.Fatalf("Inflight = %d while held, want >= 1", n)
	}
	close(release)
	<-done
}

// PLAN §22: per-backend concurrency must bound live streams, not just
// header exchange. The admission slot is held until the stream body is
// closed, and Inflight stays accurate for the whole generation (X5).
func TestStreamHoldsAdmissionUntilClosed(t *testing.T) {
	t.Parallel()

	streamStarted := make(chan struct{})
	closeBody := make(chan struct{})
	var onceStreamStarted sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("no flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f.Flush()
		onceStreamStarted.Do(func() { close(streamStarted) })
		<-closeBody // hold the stream open until the test drains it
		_, _ = w.Write([]byte("data: done\n\n"))
		f.Flush()
	}))
	defer ts.Close()

	o := testOptions(t, ts.URL)
	o.Cfg.MaxConcurrency = 1
	o.Cfg.QueueSize = 1
	o.Cfg.QueueTimeout = config.Duration(30 * time.Millisecond)
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}

	// Stream request 1: admitted, then the slot must stay held while
	// the stream body is live.
	stream1Done := make(chan struct{})
	var res1 *Result
	go func() {
		defer close(stream1Done)
		var err error
		res1, err = c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{"stream":true}`), Stream: true})
		if err != nil {
			t.Errorf("stream 1 Forward: %v", err)
			return
		}
	}()

	select {
	case <-streamStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("stream 1 never started")
	}

	// While the stream body is open, the single concurrency slot is
	// still held: Inflight must reflect the active stream, and a second
	// request must be refused (queue full), not admitted.
	if c.Inflight() != 1 {
		t.Fatalf("Inflight = %d during live stream, want 1", c.Inflight())
	}
	_, err = c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/embeddings", Body: []byte(`{}`)})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("second request while stream live: err = %v, want ErrQueueFull", err)
	}

	// Draining and closing the stream body must release the slot.
	close(closeBody)
	select {
	case <-stream1Done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream 1 did not return")
	}
	res1.Close()

	// The slot is now free: a fresh request is admitted.
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/embeddings", Body: []byte(`{}`)})
		if err == nil && res != nil {
			res.Close()
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("post-close request never admitted")
	}
}

func TestForwardQueueFull(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	admitted := make(chan struct{})
	var once sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := r.Header.Get("X-Test-First"); n == "1" {
			once.Do(func() { close(admitted) })
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	defer close(release) // declared last, runs first: unblock the handler so Close can return.

	// Concurrency 1 + queue 2: req1 holds the concurrency slot; its
	// queue slot is freed at admission, so later probes take the free
	// queue slot and fail with ErrQueueFull when the (short)
	// queue_timeout elapses, or with context.Canceled if the caller
	// gives up first.
	o := testOptions(t, ts.URL)
	o.Cfg.MaxConcurrency = 1
	o.Cfg.QueueSize = 2
	o.Cfg.QueueTimeout = config.Duration(30 * time.Millisecond)
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	go func() {
		res, err := c.Forward(ctx, Request{
			Method:  "POST",
			Path:    "/v1/embeddings",
			Body:    []byte(`{}`),
			Headers: map[string][]string{"X-Test-First": {"1"}},
		})
		if err == nil && res != nil {
			res.Close()
		}
	}()

	// Wait until req1 is admitted (the handler only runs once the
	// request is on the wire, i.e. after admission).
	select {
	case <-admitted:
	case <-time.After(3 * time.Second):
		t.Fatal("first request was not admitted")
	}

	// Now the concurrency slot is held; a probe must get ErrQueueFull.
	_, err = c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/embeddings", Body: []byte(`{}`)})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}

	// Client cancellation of a queued request also surfaces promptly.
	pctx, pcancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		pcancel()
	}()
	if _, err := c.Forward(pctx, Request{Method: "POST", Path: "/v1/embeddings", Body: []byte(`{}`)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancel: err = %v, want context.Canceled", err)
	}
}

func TestForwardPolicyRejectsNonLoopback(t *testing.T) {
	t.Parallel()

	// 100.64.0.1 is not in 127.0.0.0/8. The literal IP must fail at
	// construction, before any connection is made.
	o := Options{
		Name:    "remote",
		Cfg:     config.Backend{BaseURL: "http://100.64.0.1:8000"},
		Network: Policy{Mode: "loopback-only"},
		Log:     testsupport.DiscardLogger(),
	}
	if _, err := New(o); !errors.Is(err, ErrPolicy) {
		t.Fatalf("err = %v, want ErrPolicy", err)
	}
	// mode "any" accepts the same URL.
	o.Network = Policy{Mode: "any"}
	if _, err := New(o); err != nil {
		t.Fatalf("mode any: %v", err)
	}
}

func TestBaseURLValidation(t *testing.T) {
	t.Parallel()
	cases := []string{
		"ftp://127.0.0.1:8000",      // unsupported scheme
		"http://127.0.0.1:8000#x",   // fragment
		"http://user:pw@127.0.0.1",  // userinfo
		"http://127.0.0.1:8000?x=1", // query
		"http://127.0.0.1:8000/v1",  // ambiguous path
		"http://",                   // no host
	}
	for _, raw := range cases {
		if _, err := parseBaseURL(raw); err == nil {
			t.Fatalf("parseBaseURL(%q) succeeded", raw)
		}
	}
}

func TestStreamForwardKeepsBodyLive(t *testing.T) {
	t.Parallel()

	events := "data: {\"a\":1}\n\ndata: [DONE]\n\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = w.Write([]byte(events))
		fl.Flush()
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	res, err := c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{"stream":true}`), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	if res.Body == nil {
		t.Fatal("stream body not live")
	}
	buf := make([]byte, 4096)
	n, err := res.Body.Read(buf)
	got := string(buf[:n])
	_ = err
	if !strings.Contains(got, "data: ") {
		t.Fatalf("stream body = %q", got)
	}
}

// MPTCP regression (PLAN §83): the dialer constructor used for every
// backend connection must explicitly disable MPTCP. The behavioural
// assertion (socket-level TCP_MPTCP check) lives in mptcp_linux_test.go;
// this smoke test just confirms the dial path works end to end on any
// host.
func TestMPTCPDisabledOnDialer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dialer := policyDial(Policy{Mode: "loopback-only"}, time.Second, net.DefaultResolver)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()
	conn, err := dialer(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial through policyDialer: %v", err)
	}
	defer conn.Close()
}

// hangingResolver blocks until its context is done, emulating a silent
// resolver whose DNS response never arrives (T-M14).
type hangingResolver struct{}

func (hangingResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// errResolver returns a fixed error from LookupIPAddr.
type errResolver struct{ err error }

func (r errResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return nil, r.err
}

// TestForwardDNSHangBoundedByConnectTimeout is the T-M14 integration
// test: a resolver that hangs must not pin an admission slot or pin the
// caller indefinitely. The DNS lookup runs under the connect-timeout
// deadline (even for streaming requests, whose request context carries
// no deadline), fails bounded, and is classified as ErrDialTimeout —
// never ErrConnect, which would misclassify the failure and poison
// passive health. The admission slot must be released.
func TestForwardDNSHangBoundedByConnectTimeout(t *testing.T) {
	t.Parallel()
	o := testOptions(t, "http://hang-resolver.invalid")
	o.Cfg.ConnectTimeout = config.Duration(200 * time.Millisecond)
	o.Resolver = hangingResolver{}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`), Stream: true})
	if errors.Is(err, ErrConnect) {
		t.Fatalf("hanging resolver misclassified as ErrConnect: %v", err)
	}
	if !errors.Is(err, ErrDialTimeout) {
		t.Fatalf("hanging resolver: err = %v (want ErrDialTimeout)", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("DNS hang not bounded by connect timeout: %v", elapsed)
	}
	if n := c.Inflight(); n != 0 {
		t.Fatalf("admission slot not released after DNS timeout: inflight = %d", n)
	}
}

// TestForwardResolverClassification pins the DNS error taxonomy (T-M14):
// only genuine timeouts map to ErrDialTimeout; a not-found name stays a
// connect failure so it can drive fallback/health as a hard error.
func TestForwardResolverClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"dns timeout flag", &net.DNSError{Err: "i/o timeout", Name: "slow.invalid", IsTimeout: true}, ErrDialTimeout},
		{"dns temporary", &net.DNSError{Err: "temporary failure", Name: "temp.invalid", IsTemporary: true}, ErrDialTimeout},
		{"not found", &net.DNSError{Err: "no such host", Name: "nx.invalid", IsNotFound: true}, ErrConnect},
		{"refused", &net.DNSError{Err: "server misbehaving", Name: "refused.invalid"}, ErrConnect},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := testOptions(t, "http://resolve.invalid")
			o.Cfg.ConnectTimeout = config.Duration(time.Second)
			o.Resolver = errResolver{err: tc.err}
			c, err := New(o)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Forward(context.Background(), Request{Method: "POST", Path: "/v1/chat/completions", Body: []byte(`{}`), Stream: true})
			if !errors.Is(err, tc.want) {
				t.Fatalf("resolver err %v: got %v (want %v)", tc.err, err, tc.want)
			}
		})
	}
}

// A caller that did not ask for a stream must never be handed a live
// body, whatever Content-Type the backend chose. The non-stream path
// holds a request-timeout context whose deferred cancel fires as soon as
// Forward returns, so a live body would die mid-read: the client would
// get a truncated 200 carrying text/event-stream it never asked for, and
// nothing would be accounted. FIX-03/N3 narrows the other way (a
// stream-flagged request answered with JSON is buffered); both
// directions are needed.
func TestNonStreamRequestNeverGetsLiveBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := range 10 {
			fmt.Fprintf(w, "data: {\"n\":%d}\n\n", i)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	c := newTestClient(t, ts)

	res, err := c.Forward(context.Background(), Request{
		Path: "/v1/chat/completions", Method: http.MethodPost, Stream: false,
		Body: []byte(`{"model":"m"}`), Headers: http.Header{},
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer res.Close()

	if res.Body != nil {
		t.Fatal("a non-stream request was handed a live body")
	}
	if len(res.BodyBytes) == 0 {
		t.Fatal("the response was not buffered")
	}
}

// An https backend is verified against roots the caller loaded before
// the sandbox was applied. Left to itself, crypto/x509 reads the system
// trust store on the first handshake — which happens after confinement,
// where the policy grants no path to it, so every request to a
// perfectly trustworthy backend would fail verification.
func TestBackendUsesSuppliedRoots(t *testing.T) {
	roots := x509.NewCertPool()
	c, err := New(Options{
		Name:             "b1",
		Cfg:              config.Backend{BaseURL: "https://127.0.0.1:8443", MaxConcurrency: 1, QueueSize: 1},
		Network:          Policy{Mode: "loopback-only"},
		MaxResponseBytes: 1 << 20,
		Log:              testsupport.DiscardLogger(),
		RootCAs:          roots,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, hc := range map[string]*http.Client{"stream": c.http, "plain": c.httpPlain} {
		tr, ok := hc.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s client has no http.Transport", name)
		}
		if tr.TLSClientConfig == nil {
			t.Fatalf("%s transport has no TLS configuration, so it would load system roots after confinement", name)
		}
		if tr.TLSClientConfig.RootCAs != roots {
			t.Fatalf("%s transport does not use the preloaded roots", name)
		}
	}
}
