// Package backend is the bounded OpenAI-compatible upstream client
// (PLAN §15, §16, §17, §18, §22, §24).
//
// Guarantees:
//   - destination is checked against the backend network policy
//     (PLAN §16); every new connection re-validates the selected IP;
//   - MPTCP is explicitly disabled on the dialer (PLAN §61, §83);
//   - client-controlled auth/identity headers are never forwarded; the
//     backend credential is injected only from trusted configuration
//     (PLAN §17, §18);
//   - admission is bounded by max_concurrency + queue_size with a
//     queue_timeout (PLAN §22);
//   - responses are bounded: non-stream bodies by max_response_bytes,
//     headers by header_timeout; stream idleness is enforced by the
//     caller (PLAN §24).
//
// Note: v1 dials backends by IP literal (PLAN §16.1 recommends IP
// literals); HTTPS backends with certificate names other than the IP
// are not supported until a concrete need appears.
package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"mellomting/internal/config"
	"mellomting/internal/landlock"
	"mellomting/internal/securefile"
)

// Sanitized error classes (PLAN §43). These never carry backend
// hostnames, bodies, or secrets into logs or client responses.
//
// Retry classification (PLAN §23): ErrConnect and ErrDialTimeout denote
// a connection failure before any response byte was observed; they are
// the client-side connection failures the proxy may retry or fall back,
// and they poison passive health. ErrHeaderTimeout is a distinct class —
// the connection was established but the first response byte did not
// arrive within header_timeout — and is deliberately NOT retried: it is
// a latency/capacity signal, and retrying would re-issue a full
// generation that cannot succeed within the header bound (X6). It also
// does not poison passive health. ErrQueueFull (admission failure) is a
// fallback candidate but not health-poisoning. ErrTimeout covers bounded
// total/body timeouts, which are not on the PLAN §23 retry list.
var (
	ErrConnect       = errors.New("backend_connect")
	ErrDialTimeout   = errors.New("backend_dial_timeout")
	ErrHeaderTimeout = errors.New("backend_header_timeout")
	ErrTimeout       = errors.New("backend_timeout")
	ErrQueueFull     = errors.New("backend_queue_full")
	ErrPolicy        = errors.New("backend_network_policy")
	ErrTooLarge      = errors.New("backend_response_too_large")
)

// Upstream is a non-2xx response from the backend. The body is buffered
// by the client and is never forwarded to the client verbatim.
type Upstream struct{ Status int }

func (e *Upstream) Error() string { return fmt.Sprintf("upstream status %d", e.Status) }

// Policy is the backend egress policy (PLAN §16).
type Policy struct {
	Mode  string // "loopback-only", "allowed-cidrs", or "any"
	CIDRs []*net.IPNet
}

func (p Policy) allow(ip net.IP) bool {
	switch p.Mode {
	case "any":
		return true
	case "allowed-cidrs":
		v4 := ip.To4()
		for _, c := range p.CIDRs {
			if c.Contains(ip) || (v4 != nil && c.Contains(v4)) {
				return true
			}
		}
		return false
	default: // "loopback-only"
		return ip.IsLoopback()
	}
}

// Resolver resolves hostnames to IP addresses. net.DefaultResolver
// satisfies it; tests inject a stub to exercise a hanging or failing
// resolver without touching the process-global default (T-M14).
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Options configures one backend client.
type Options struct {
	Name string
	Cfg  config.Backend
	// Network is the egress policy (PLAN §16).
	Network Policy
	// MaxResponseBytes bounds buffered (non-stream and error) upstream
	// responses (PLAN §9.1). Live streams are bounded by the same cap on
	// emitted bytes in the proxy pump (FIX-12).
	MaxResponseBytes int
	Log              *slog.Logger
	// Resolver overrides the DNS resolver used for outbound dials.
	// Defaults to net.DefaultResolver. Testing hook (T-M14).
	Resolver Resolver
}

// Client owns one backend's connection and admission state.
type Client struct {
	name             string
	upstream         string
	base             *url.URL
	apiKey           string
	policy           Policy
	log              *slog.Logger
	requestTimeout   time.Duration
	streamIdle       time.Duration
	maxResponseBytes int
	// http is used for streaming requests: it applies
	// ResponseHeaderTimeout = header_timeout, because the first stream
	// byte is expected promptly.
	http *http.Client
	// httpPlain is used for non-streaming requests: its header bound is
	// the completion bound (request_timeout), because for non-streaming
	// work headers arrive only when the whole completion is ready
	// (X6: a 30s+ generation must not be truncated by header_timeout).
	httpPlain    *http.Client
	queue        chan struct{}
	conc         chan struct{}
	queueTimeout time.Duration
}

// maxBackendResponseHeaderBytes bounds the backend response header size
// so a misbehaving backend cannot grow its headers unboundedly (T-L7).
const maxBackendResponseHeaderBytes = 64 << 10

// New builds a validated backend client (PLAN §15.1, §16, §17).
func New(o Options) (*Client, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	policy, err := parsePolicy(o.Network)
	if err != nil {
		return nil, fmt.Errorf("backend %s: %w", o.Name, err)
	}
	resolver := o.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	u, err := parseBaseURL(o.Cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("backend %s: %w", o.Name, err)
	}
	host, _ := splitHostPort(u)
	if ip := net.ParseIP(host); ip != nil {
		if !policy.allow(ip) {
			return nil, fmt.Errorf("backend %s: %w", o.Name, ErrPolicy)
		}
	} else if policy.Mode != "any" {
		o.Log.Info("backend uses a DNS name; the resolved IP is validated per connection",
			"backend", o.Name, "host", host, "mode", policy.Mode)
	}

	var apiKey string
	if o.Cfg.APIKeyFile != "" {
		data, err := securefile.Read(o.Cfg.APIKeyFile, 4096)
		if err != nil {
			return nil, fmt.Errorf("backend %s: read api_key_file: %w", o.Name, err)
		}
		apiKey = strings.TrimSpace(string(data))
		if apiKey == "" {
			return nil, fmt.Errorf("backend %s: api_key_file is empty", o.Name)
		}
	}

	maxConc := o.Cfg.MaxConcurrency
	if maxConc < 1 {
		maxConc = 1
	}
	queueSize := int(o.Cfg.QueueSize)
	if queueSize < 0 {
		queueSize = 0
	}

	t := &http.Transport{
		DisableKeepAlives: false,
		// Bound physical connections to the backend host by the
		// admission concurrency cap: at most maxConc requests are
		// in-flight to a backend, so more connections can never be
		// needed (T-L7, §9.1 fd bound). Match the idle pool to the same
		// cap: Go's default MaxIdleConnsPerHost (2) makes MaxIdleConns
		// alone inert, tearing down and re-handshaking connections
		// under bursty load (M18).
		MaxIdleConns:        maxConc,
		MaxIdleConnsPerHost: maxConc,
		MaxConnsPerHost:     maxConc,
		IdleConnTimeout:     90 * time.Second,
		DialContext:         policyDial(policy, o.Cfg.ConnectTimeout.Duration(), resolver),
		// PLAN §9.2 SHOULD: the proxy passes response bodies through
		// byte-identical; transparent gzip decoding would corrupt the
		// accounting byte count and SSE framing (T-L7).
		DisableCompression: true,
		// Bound response headers so a hostile backend cannot grow them
		// unboundedly (T-L7).
		MaxResponseHeaderBytes: maxBackendResponseHeaderBytes,
	}
	// Streaming: the first stream byte is expected promptly, so the
	// header wait is bounded by header_timeout (PLAN §15).
	tStream := t.Clone()
	tStream.ResponseHeaderTimeout = o.Cfg.HeaderTimeout.Duration()
	// Non-streaming: headers arrive only when the completion is done
	// (X6). Bounding that wait by header_timeout truncates legitimate
	// long generations; the completion-appropriate bound is the total
	// request_timeout (the context deadline applies the same bound).
	tPlain := t.Clone()
	tPlain.ResponseHeaderTimeout = o.Cfg.RequestTimeout.Duration()

	return &Client{
		name:             o.Name,
		upstream:         o.Cfg.UpstreamModel,
		base:             u,
		apiKey:           apiKey,
		policy:           policy,
		log:              o.Log,
		requestTimeout:   o.Cfg.RequestTimeout.Duration(),
		streamIdle:       o.Cfg.StreamIdleTimeout.Duration(),
		maxResponseBytes: o.MaxResponseBytes,
		// Redirects are never followed (X3): the backend must not be
		// able to steer the connection to another host/path, where the
		// Authorization header and body would be re-sent. A 3xx is
		// returned to Forward and classified as an upstream error.
		http: &http.Client{
			Transport: tStream,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		httpPlain: &http.Client{
			Transport: tPlain,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		queue:        make(chan struct{}, queueSize),
		conc:         make(chan struct{}, maxConc),
		queueTimeout: o.Cfg.QueueTimeout.Duration(),
	}, nil
}

// parsePolicy validates a policy at startup (fail closed).
func parsePolicy(p Policy) (Policy, error) {
	switch p.Mode {
	case "loopback-only", "any":
		return p, nil
	case "allowed-cidrs":
		if len(p.CIDRs) == 0 {
			return Policy{}, errors.New("network policy allowed-cidrs needs cidrs")
		}
		return p, nil
	default:
		return Policy{}, fmt.Errorf("unsupported backend network mode %q", p.Mode)
	}
}

// parseBaseURL applies PLAN §15.1 URL validation.
func parseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("malformed base_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("base_url scheme must be http or https")
	}
	if u.Fragment != "" {
		return nil, errors.New("base_url must not contain a fragment")
	}
	if u.User != nil {
		return nil, errors.New("base_url must not contain userinfo")
	}
	if u.RawQuery != "" {
		return nil, errors.New("base_url must not contain a query string")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("base_url path must be empty or \"/\" (endpoint construction must stay unambiguous)")
	}
	if u.Hostname() == "" {
		return nil, errors.New("base_url must have a host")
	}
	return u, nil
}

func splitHostPort(u *url.URL) (string, string) {
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		// Scheme default, shared with the Landlock sandbox so the two
		// can never disagree about the granted port (T-Q6).
		port, _ = landlock.DefaultPortForScheme(u.Scheme)
		return u.Hostname(), port
	}
	return host, port
}

// policyDial returns a DialContext that enforces the egress policy on
// every new connection (PLAN §16.2: revalidate each new connection) and
// disables MPTCP (PLAN §61). The whole dial — DNS resolution included —
// is bounded by the connect timeout (T-M14): a silent resolver must not
// pin an admission slot indefinitely, and a slow resolver must surface
// as a dial timeout (never a plain ErrConnect that would misclassify the
// failure and feed passive-health poisoning).
func policyDial(p Policy, connectTimeout time.Duration, resolver Resolver) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, fmt.Errorf("unexpected network %q", network)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, ErrConnect
		}
		timeout := connectTimeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		// The deadline covers name resolution and the TCP connect
		// (T-M14); the net.Dialer.Timeout below remains as a second,
		// shorter-or-equal bound on the connect itself.
		dialCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		var target string
		if ip := net.ParseIP(host); ip != nil {
			if !p.allow(ip) {
				return nil, ErrPolicy
			}
			target = net.JoinHostPort(ip.String(), port)
		} else {
			ips, err := resolver.LookupIPAddr(dialCtx, host)
			if err != nil {
				if isTimeout(err) {
					return nil, ErrDialTimeout
				}
				return nil, ErrConnect
			}
			for _, ip := range ips {
				if p.allow(ip.IP) {
					target = net.JoinHostPort(ip.IP.String(), port)
					break
				}
			}
			if target == "" {
				return nil, ErrPolicy
			}
		}
		var d net.Dialer
		d.Timeout = timeout
		d.SetMultipathTCP(false) // PLAN §61, §83
		conn, err := d.DialContext(dialCtx, "tcp", target)
		if err != nil {
			return nil, dialError(err)
		}
		return conn, nil
	}
}

// isTimeout reports whether an error is a resolution/dial timeout or
// deadline expiry (T-M14). DNS errors carry their own timeout flag;
// context deadlines and net.Error timeouts are also covered.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var de *net.DNSError
	if errors.As(err, &de) {
		return de.IsTimeout || de.IsTemporary
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func dialError(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrDialTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrDialTimeout
	}
	return ErrConnect
}

// Request is one sanitized outbound request (PLAN §18). Headers must be
// pass-through only (User-Agent, Accept, Content-Type); Mellomting-owned
// headers (Host, Authorization, X-Api-Key, *Forwarded*) are never part of
// a Request.
type Request struct {
	Method  string
	Path    string // e.g. "/v1/chat/completions"
	Body    []byte
	Headers map[string][]string
	Stream  bool
}

// Result is the upstream response. Body is live only for 2xx stream
// responses; everything else is fully buffered in BodyBytes and the
// original stream is closed. For a live stream Body the admission slot
// is held for the lifetime of the response: Close must be called to
// release it (PLAN §22: per-backend concurrency bounds streams too).
type Result struct {
	Status    int
	Header    http.Header
	Body      io.ReadCloser
	BodyBytes []byte
	hold      *hold // admission slot; transferred to the Result for streams
}

// Close releases the stream, if any, and the admission slot that is
// held for a live stream response. It is idempotent.
func (r *Result) Close() {
	if r.Body != nil {
		r.Body.Close()
	}
	if r.hold != nil {
		r.hold.release()
		r.hold = nil
	}
}

// hold is the admission handle (PLAN §22) of a request admitted by acquire:
// it carries the concurrency token the request keeps for its lifetime.
// Inflight counts it as one active request once acquire has returned;
// until then a queue-admitted request still holds its queue token too
// (see the Inflight note on the transient double count).
type hold struct{ c *Client }

func (h *hold) release() {
	<-h.c.conc
}

// acquire implements PLAN §22 admission. A concurrency slot is taken
// immediately when one is free, so idle capacity is never gated by the
// queue (a request is admitted whenever a concurrency slot OR a queue
// slot is available, not only when a queue slot is). Only when
// concurrency is saturated does the request take a queue slot to wait,
// bounded by queue_size and queue_timeout. Failures are bounded and never
// wait indefinitely.
func (c *Client) acquire(ctx context.Context) (*hold, error) {
	// Fast path: a concurrency slot is free right now — admit without
	// touching the queue.
	select {
	case c.conc <- struct{}{}:
		return &hold{c: c}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	// Concurrency is saturated: take a queue slot to wait for one. The
	// queue token is released when acquire returns (admitted or not), so
	// it gates only waiters, never in-flight requests.
	select {
	case c.queue <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		// Queue is full: still honour a cancelled context so a client
		// disconnect while the queue is full is classified as a
		// disconnect, not as queue exhaustion (T-L14) — the two have
		// different operational meanings (capacity planning vs. client
		// churn).
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			return nil, ErrQueueFull
		}
	}
	defer func() { <-c.queue }()

	timer := time.NewTimer(c.queueTimeout)
	defer timer.Stop()
	select {
	case c.conc <- struct{}{}:
		return &hold{c: c}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		// The timer (queue_full) and a client disconnect can fire in
		// the same instant; Go's select picks randomly among ready
		// cases, so re-check the context before reporting queue
		// exhaustion (T-L14).
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			return nil, ErrQueueFull
		}
	}
}

// Forward issues one bounded request to the backend (PLAN §15, §18, §24).
func (c *Client) Forward(ctx context.Context, req Request) (*Result, error) {
	h, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}

	path := req.Path
	if c.base.Path != "" {
		path = c.base.Path + strings.TrimLeft(req.Path, "/")
	}
	u := &url.URL{Scheme: c.base.Scheme, Host: c.base.Host, Path: path}

	fctx := ctx
	var cancel context.CancelFunc
	var boundTimer *time.Timer
	var timeoutFired <-chan struct{}
	if c.requestTimeout > 0 {
		if !req.Stream {
			fctx, cancel = context.WithTimeout(ctx, c.requestTimeout)
			defer cancel()
		} else {
			// R1 (FIX-03 eval): a stream-flagged request may still be
			// answered with plain content, taking the buffered path,
			// where the streaming branch sets no request deadline. Keep a
			// cancelable request context so that read can be bounded by
			// request_timeout once the headers have arrived; a genuine
			// stream keeps it live. The timer is created here (so cancel
			// escapes the lostcancel check) but disarmed until the
			// buffered path re-arms it.
			fired := make(chan struct{})
			timeoutFired = fired
			fctx, cancel = context.WithCancel(ctx)
			boundTimer = time.AfterFunc(c.requestTimeout, func() { close(fired); cancel() })
			boundTimer.Stop()
			defer boundTimer.Stop()
		}
	}

	outReq, err := http.NewRequestWithContext(fctx, req.Method, u.String(), bytes.NewReader(req.Body))
	if err != nil {
		h.release()
		return nil, ErrConnect
	}
	for k, vs := range req.Headers {
		for _, v := range vs {
			outReq.Header.Add(k, v)
		}
	}
	if c.apiKey != "" {
		outReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	// Streaming requests expect the first byte promptly (header bound =
	// header_timeout); non-streaming work's headers arrive at
	// completion, so it uses the plain client whose header bound is the
	// completion request_timeout (X6: a long non-streaming generation
	// must not be truncated by header_timeout).
	httpClient := c.http
	if !req.Stream {
		httpClient = c.httpPlain
	}
	resp, err := httpClient.Do(outReq)
	if err != nil {
		h.release()
		return nil, requestError(err, req.Stream)
	}
	// Response liveness is decided from the response's Content-Type, not
	// from the client's stream flag (FIX-03/N3 of FIX_REVIEW_2026-08-22):
	// a stream-flagged request answered with a plain application/json
	// 200 (e.g. /v1/embeddings, which OpenAI never streams) is buffered
	// and accounted like any non-streaming response, never fed to the
	// SSE pump where it would charge zero tokens.
	streamBody := resp.StatusCode == http.StatusOK && isEventStream(resp)
	if !streamBody {
		defer resp.Body.Close()
		if boundTimer != nil {
			// R1 (FIX-03 eval): the streaming branch sets no deadline, so
			// a stream-flagged response buffered here would otherwise hold
			// its admission slots until the client hung up. Arm the bound
			// now that liveness is known; cancelling the request context
			// is the only reliable way to interrupt a blocked body read.
			boundTimer.Reset(c.requestTimeout)
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, int64(c.maxResponseBytes)+1))
		if rerr != nil {
			// Partial-body read failure: the buffered body cannot be
			// trusted, so only the sanitized class comes back. When our
			// request_timeout bound fired, report it directly — the
			// ErrTimeout sentinel is not a net.Error, so passing it
			// through bodyReadError would misclassify it as ErrConnect.
			if timeoutFired != nil {
				select {
				case <-timeoutFired:
					h.release()
					return nil, ErrTimeout
				default:
				}
			}
			h.release()
			return nil, bodyReadError(rerr)
		}
		if len(data) > c.maxResponseBytes {
			h.release()
			return nil, ErrTooLarge
		}
		// Any 3xx is an upstream error: with CheckRedirect set to
		// ErrUseLastResponse the redirect was not followed, so the
		// Location response (and never the redirect target) is what we
		// hold; it must be surfaced as a sanitized error, never relayed
		// as a success body (X3).
		if resp.StatusCode >= 300 {
			h.release()
			return &Result{Status: resp.StatusCode, Header: resp.Header, BodyBytes: data}, &Upstream{Status: resp.StatusCode}
		}
		h.release()
		return &Result{Status: resp.StatusCode, Header: resp.Header, BodyBytes: data}, nil
	}

	// 2xx stream: keep live, and hold the admission slot for the
	// lifetime of the response (PLAN §22 bounds streaming work per
	// backend). The caller must Close the Result once the body has
	// been fully drained.
	return &Result{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body, hold: h}, nil
}

// isEventStream reports whether the backend is sending a Server-Sent
// Events body. Liveness is decided from the response Content-Type, not
// the client's stream flag, so a plain application/json 200 is never fed
// to the SSE pump (FIX-03/N3).
func isEventStream(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return strings.HasPrefix(strings.ToLower(ct), "text/event-stream")
}

// requestError classifies an http.Client.Do error (no response headers
// observed) into a sanitized class (PLAN §43, §23). Mellomting's own
// dialer sentinels pass through unchanged so their retry semantics
// survive the http.Client's url.Error wrapping. The http client
// surfaces both its header bound and the request context deadline as a
// net.Error with Timeout() true (context.DeadlineExceeded itself
// implements net.Error), so the stream flag selects the correct class:
// a streaming request's header bound (ResponseHeaderTimeout =
// header_timeout) is ErrHeaderTimeout, while a non-streaming request's
// total deadline (request_timeout) is ErrTimeout (T-T5).
func requestError(err error, stream bool) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, ErrConnect) || errors.Is(err, ErrPolicy) || errors.Is(err, ErrDialTimeout) {
		return err
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			if stream {
				return ErrHeaderTimeout
			}
			return ErrTimeout
		}
		return ErrConnect
	}
	return ErrConnect
}

// bodyReadError classifies a buffered body read failure (headers were
// observed). A dropped connection is a connection failure (PLAN §23
// retry candidate); a timeout is the total/body timeout that is not on
// the retry list.
func bodyReadError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrTimeout
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrTimeout
	}
	return ErrConnect
}

// Inflight reports the current admission load (PLAN §19, §22): queued
// waiters plus active requests. It is an approximate snapshot for
// least-inflight routing and is always >= 0. A waiter that has just been
// admitted holds its queue token and its concurrency token until acquire
// returns, so it is transiently double counted for a few instructions;
// the window is too brief to matter for a routing decision. For live
// streams the slot is held until Result.Close, so Inflight stays
// accurate for the full generation, not just the header exchange.
func (c *Client) Inflight() int {
	return len(c.queue) + len(c.conc)
}

// StreamIdleTimeout is the backend stream-idle bound (PLAN §24).
func (c *Client) StreamIdleTimeout() time.Duration { return c.streamIdle }
