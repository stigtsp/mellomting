package backend

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mellomting/internal/config"
)

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
		Method:  "POST",
		Path:    "/v1/chat/completions",
		Body:    []byte(`{"model":"m"}`),
		Headers: map[string][]string{"User-Agent": {"mellomting-test"}},
		Stream:  false,
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if res.Status != 200 || string(res.BodyBytes) != `{"id":"chatcmpl-1"}` {
		t.Fatalf("res = %+v", res)
	}
	if gotXAPIKey != "" {
		t.Errorf("X-Api-Key forwarded: %q", gotXAPIKey)
	}
	if gotUA != "mellomting-test" {
		t.Errorf("User-Agent = %q", gotUA)
	}
	if gotHost != ts.Listener.Addr().String() {
		t.Errorf("Host = %q, want backend host", gotHost)
	}
	// No credential configured → no Authorization injected.
	if gotAuth != "" {
		t.Errorf("Authorization injected without api_key_file: %q", gotAuth)
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
	if errors.Is(err, ErrTimeout) {
		t.Fatal("header timeout misclassified as total timeout")
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

// PLAN §19: Inflight reports admission load so least-inflight routing
// can see queued and active requests.
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
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("no flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f.Flush()
		close(streamStarted)
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
		Log:     discardLogger(),
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
// backend connection must explicitly disable MPTCP. A source-level
// assertion around the constructor is permitted by the plan; the runtime
// check exercises the dial path against a live listener.
func TestMPTCPDisabledOnDialer(t *testing.T) {
	data, err := os.ReadFile("backend.go")
	if err != nil {
		t.Fatal(err)
	}
	src := `d.SetMultipathTCP(false)`
	if !strings.Contains(string(data), src) {
		t.Fatalf("backend.go does not contain %q (PLAN §61/§83)", src)
	}

	// Also confirm the dial path works end to end on this host.
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
		tc := tc
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

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 10}))
}
