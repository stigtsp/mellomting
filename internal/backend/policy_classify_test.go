package backend

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mellomting/internal/config"
)

// TestPolicyAllow pins the egress policy decision (PLAN §16) for every
// mode: loopback-only accepts loopback and rejects everything else; any
// accepts everything; allowed-cidrs accepts only addresses inside the
// listed CIDRs (including IPv4-mapped IPv6 forms of the same address).
func TestPolicyAllow(t *testing.T) {
	t.Parallel()
	_, v4loop, _ := net.ParseCIDR("127.0.0.0/8")
	_, v6loop, _ := net.ParseCIDR("::1/128")
	_, private, _ := net.ParseCIDR("10.0.0.0/8")

	cases := []struct {
		name string
		p    Policy
		ip   net.IP
		want bool
	}{
		{"loopback-only accepts loopback v4", Policy{Mode: "loopback-only"}, net.ParseIP("127.0.0.1"), true},
		{"loopback-only accepts loopback v6", Policy{Mode: "loopback-only"}, net.ParseIP("::1"), true},
		{"loopback-only rejects private v4", Policy{Mode: "loopback-only"}, net.ParseIP("10.0.0.5"), false},
		{"loopback-only rejects CGNAT v4", Policy{Mode: "loopback-only"}, net.ParseIP("100.64.0.1"), false},
		{"any accepts private v4", Policy{Mode: "any"}, net.ParseIP("10.0.0.5"), true},
		{"any accepts loopback", Policy{Mode: "any"}, net.ParseIP("127.0.0.1"), true},
		{"allowed-cidrs v4 in range", Policy{Mode: "allowed-cidrs", CIDRs: []*net.IPNet{private}}, net.ParseIP("10.1.2.3"), true},
		{"allowed-cidrs v4 out of range", Policy{Mode: "allowed-cidrs", CIDRs: []*net.IPNet{private}}, net.ParseIP("192.168.1.1"), false},
		{"allowed-cidrs matches any cidr", Policy{Mode: "allowed-cidrs", CIDRs: []*net.IPNet{v4loop, v6loop}}, net.ParseIP("::1"), true},
		{"allowed-cidrs v4-mapped matches v4 cidr", Policy{Mode: "allowed-cidrs", CIDRs: []*net.IPNet{private}}, net.ParseIP("::ffff:10.1.2.3"), true},
		{"loopback-only unknown mode falls back to loopback", Policy{Mode: "bogus"}, net.ParseIP("10.0.0.5"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.p.allow(tc.ip); got != tc.want {
				t.Fatalf("Policy{Mode:%q}.allow(%v) = %v, want %v", tc.p.Mode, tc.ip, got, tc.want)
			}
		})
	}
}

// TestParsePolicy pins the fail-closed policy validation: allowed-cidrs
// without CIDRs is rejected, an unknown mode is rejected, and the two
// safe modes pass through unchanged.
func TestParsePolicy(t *testing.T) {
	t.Parallel()
	_, v4loop, _ := net.ParseCIDR("127.0.0.0/8")

	if p, err := parsePolicy(Policy{Mode: "loopback-only"}); err != nil || p.Mode != "loopback-only" {
		t.Fatalf("loopback-only: p = %+v, err = %v", p, err)
	}
	if p, err := parsePolicy(Policy{Mode: "any"}); err != nil || p.Mode != "any" {
		t.Fatalf("any: p = %+v, err = %v", p, err)
	}
	p, err := parsePolicy(Policy{Mode: "allowed-cidrs", CIDRs: []*net.IPNet{v4loop}})
	if err != nil || len(p.CIDRs) != 1 {
		t.Fatalf("allowed-cidrs with cidrs: p = %+v, err = %v", p, err)
	}
	if _, err := parsePolicy(Policy{Mode: "allowed-cidrs"}); err == nil {
		t.Fatal("allowed-cidrs without cidrs accepted")
	}
	if _, err := parsePolicy(Policy{Mode: "warp-drive"}); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

// TestDialError pins the dial-failure taxonomy: a net.Error timeout (or
// a context deadline) is a dial timeout (PLAN §23 retry candidate),
// anything else is a plain connect failure.
func TestDialError(t *testing.T) {
	t.Parallel()
	timedOut := &net.OpError{Err: timeoutErr{}}
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"net timeout", timedOut, ErrDialTimeout},
		{"deadline exceeded", context.DeadlineExceeded, ErrDialTimeout},
		{"connection refused", errors.New("connect: connection refused"), ErrConnect},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := dialError(tc.err); got != tc.want {
				t.Fatalf("dialError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsTimeout pins the timeout classification used by the dial path:
// context deadlines, DNS timeout/temporary flags, and net.Error timeouts
// all count as timeouts; ordinary failures do not.
func TestIsTimeout(t *testing.T) {
	t.Parallel()
	timedOut := &net.OpError{Err: timeoutErr{}}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"context deadline", context.DeadlineExceeded, true},
		{"dns timeout flag", &net.DNSError{IsTimeout: true}, true},
		{"dns temporary flag", &net.DNSError{IsTemporary: true}, true},
		{"dns not found", &net.DNSError{IsNotFound: true}, false},
		{"net timeout", timedOut, true},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isTimeout(tc.err); got != tc.want {
				t.Fatalf("isTimeout(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestBodyReadErrorUnwrapsURLAndCancel pins the remaining bodyReadError
// branches: a url.Error wrapping a timeout is the total/body timeout
// class, and a client cancel passes through unchanged so the proxy can
// classify a disconnect.
func TestBodyReadErrorUnwrapsURLAndCancel(t *testing.T) {
	t.Parallel()
	timedOut := &net.OpError{Err: timeoutErr{}}
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"url.Error wrapping timeout", &url.Error{Op: "Get", URL: "http://x", Err: timedOut}, ErrTimeout},
		{"url.Error wrapping deadline", &url.Error{Op: "Get", URL: "http://x", Err: context.DeadlineExceeded}, ErrTimeout},
		{"client cancel", context.Canceled, context.Canceled},
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

// TestStreamIdleTimeout pins the stream-idle getter (PLAN §24): it must
// return the configured bound so the proxy's SSE pump can enforce it.
// TestNewRejectsEmptyAPIKeyFile pins the fail-closed branch of New: a
// configured but empty (or blank) api_key_file is a construction error,
// never a backend that silently dials without auth.
func TestNewRejectsEmptyAPIKeyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "backend.key")
	if err := os.WriteFile(keyFile, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := testOptions(t, "http://127.0.0.1:8001")
	o.Cfg.APIKeyFile = keyFile
	if _, err := New(o); err == nil {
		t.Fatal("empty api_key_file accepted")
	}
}

func TestStreamIdleTimeout(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion"}`))
	}))
	defer ts.Close()
	c := newTestClient(t, ts)
	if want := time.Second; c.StreamIdleTimeout() != want {
		t.Fatalf("StreamIdleTimeout() = %v, want %v", c.StreamIdleTimeout(), want)
	}
}

// TestAcquireQueueTimeoutAndCancel pins the two bounded admission
// failure modes (PLAN §22): when the concurrency slot stays occupied
// past queue_timeout the acquire reports ErrQueueFull, and a client
// cancel during the wait reports the context error (never the sentinel).
func TestAcquireQueueTimeoutAndCancel(t *testing.T) {
	t.Parallel()

	o := Options{
		Name: "full",
		Cfg: config.Backend{
			BaseURL:        "http://127.0.0.1:8000",
			QueueSize:      1,
			MaxConcurrency: 1,
			QueueTimeout:   config.Duration(50 * time.Millisecond),
		},
		Network:          Policy{Mode: "loopback-only"},
		MaxResponseBytes: 1 << 20,
		Log:              discardLogger(),
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}

	// Occupy the only concurrency slot so any further acquire must wait
	// on the queue timer.
	c.conc <- struct{}{}

	if _, err := c.acquire(context.Background()); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("acquire with full concurrency = %v, want ErrQueueFull", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.acquire(ctx)
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire after client cancel = %v, want context.Canceled", err)
	}

	// The queue token must be released on both paths so Inflight
	// returns to exactly the one occupied concurrency slot.
	if n := c.Inflight(); n != 1 {
		t.Fatalf("inflight = %d, want 1", n)
	}
}

// TestPolicyDialRejectsNonTCPNetwork pins the policyDial guard: only
// tcp-family networks are dialed; anything else fails closed before any
// resolution or connection.
func TestPolicyDialRejectsNonTCPNetwork(t *testing.T) {
	t.Parallel()
	dialer := policyDial(Policy{Mode: "any"}, time.Second, net.DefaultResolver)
	if _, err := dialer(context.Background(), "udp", "127.0.0.1:8000"); err == nil {
		t.Fatal("policyDial accepted a non-tcp network")
	}
}

// TestPolicyDialAllowedCIDRs pins the per-connection revalidation (PLAN
// §16.2): an allowed-cidrs policy permits a connection only to
// addresses inside its CIDRs, and the check happens before any dial (an
// out-of-range literal must fail with ErrPolicy even for a live
// listener).
func TestPolicyDialAllowedCIDRs(t *testing.T) {
	t.Parallel()

	_, v4loop, _ := net.ParseCIDR("127.0.0.0/8")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	allowed := policyDial(Policy{Mode: "allowed-cidrs", CIDRs: []*net.IPNet{v4loop}}, time.Second, net.DefaultResolver)
	conn, err := allowed(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial in-range address: %v", err)
	}
	_ = conn.Close()

	denied := policyDial(Policy{Mode: "allowed-cidrs", CIDRs: []*net.IPNet{}}, time.Second, net.DefaultResolver)
	if _, err := denied(context.Background(), "tcp", ln.Addr().String()); !errors.Is(err, ErrPolicy) {
		t.Fatalf("dial out-of-range address = %v, want ErrPolicy", err)
	}
}

// TestPolicyDialDNSResolvesToAllowed pins the DNS path of the policy
// dial: the resolved address is validated per connection, and a
// resolution that yields only disallowed addresses is ErrPolicy.
func TestPolicyDialDNSResolvesToAllowed(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	_, port, _ := net.SplitHostPort(ln.Addr().String())

	// stubResolver returns a fixed address set for the host, standing in
	// for DNS so the test never touches the process-global resolver.
	stub := &staticResolver{ips: []net.IP{net.ParseIP("192.168.1.99"), net.ParseIP("127.0.0.1")}}
	allowed := policyDial(Policy{Mode: "loopback-only"}, time.Second, stub)
	conn, err := allowed(context.Background(), "tcp", "name.test:"+port)
	if err != nil {
		t.Fatalf("dial via DNS resolving to loopback: %v", err)
	}
	_ = conn.Close()

	stubOnlyPrivate := &staticResolver{ips: []net.IP{net.ParseIP("192.168.1.99")}}
	denied := policyDial(Policy{Mode: "loopback-only"}, time.Second, stubOnlyPrivate)
	if _, err := denied(context.Background(), "tcp", "name.test:"+port); !errors.Is(err, ErrPolicy) {
		t.Fatalf("dial via DNS resolving outside policy = %v, want ErrPolicy", err)
	}
}

// staticResolver resolves every host to a fixed IP list (T-M14-style
// stub, no process-global resolver mutation).
type staticResolver struct{ ips []net.IP }

func (r *staticResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	out := make([]net.IPAddr, 0, len(r.ips))
	for _, ip := range r.ips {
		out = append(out, net.IPAddr{IP: ip})
	}
	return out, nil
}
