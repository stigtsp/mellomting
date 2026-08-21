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
	"mellomting/internal/securefile"
)

// Sanitized error classes (PLAN §43). These never carry backend
// hostnames, bodies, or secrets into logs or client responses.
//
// Retry classification (PLAN §23): ErrConnect, ErrDialTimeout,
// ErrHeaderTimeout, and ErrQueueFull denote a connection-level or
// admission failure before any response byte was observed; they are the
// client-side connection failures the proxy may retry or fall back.
// ErrTimeout covers bounded total/body timeouts, which are not on the
// PLAN §23 retry list.
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

// AsUpstream reports whether err is a *Upstream.
func AsUpstream(err error) (*Upstream, bool) {
	var u *Upstream
	if errors.As(err, &u) {
		return u, true
	}
	return nil, false
}

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

// Options configures one backend client.
type Options struct {
	Name string
	Cfg  config.Backend
	// Network is the egress policy (PLAN §16).
	Network Policy
	// MaxResponseBytes bounds buffered (non-stream and error) upstream
	// responses (PLAN §9.1).
	MaxResponseBytes int
	Log              *slog.Logger
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
	http             *http.Client
	queue            chan struct{}
	conc             chan struct{}
	queueTimeout     time.Duration
}

// New builds a validated backend client (PLAN §15.1, §16, §17).
func New(o Options) (*Client, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	policy, err := parsePolicy(o.Network)
	if err != nil {
		return nil, fmt.Errorf("backend %s: %w", o.Name, err)
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
		MaxIdleConns:      16,
		IdleConnTimeout:   90 * time.Second,
		DialContext:       policyDial(policy, o.Cfg.ConnectTimeout.Duration()),
	}
	// Header wait (first response byte) is bounded per PLAN §15.
	t.ResponseHeaderTimeout = o.Cfg.HeaderTimeout.Duration()

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
		http:             &http.Client{Transport: t},
		queue:            make(chan struct{}, queueSize),
		conc:             make(chan struct{}, maxConc),
		queueTimeout:     o.Cfg.QueueTimeout.Duration(),
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
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
		return u.Hostname(), port
	}
	return host, port
}

// policyDial returns a DialContext that enforces the egress policy on
// every new connection (PLAN §16.2: revalidate each new connection) and
// disables MPTCP (PLAN §61).
func policyDial(p Policy, connectTimeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, fmt.Errorf("unexpected network %q", network)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, ErrConnect
		}
		var target string
		if ip := net.ParseIP(host); ip != nil {
			if !p.allow(ip) {
				return nil, ErrPolicy
			}
			target = net.JoinHostPort(ip.String(), port)
		} else {
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
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
		d.Timeout = connectTimeout
		if d.Timeout == 0 {
			d.Timeout = 30 * time.Second
		}
		d.SetMultipathTCP(false) // PLAN §61, §83
		conn, err := d.DialContext(ctx, "tcp", target)
		if err != nil {
			return nil, dialError(err)
		}
		return conn, nil
	}
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

// hold is an admission slot (PLAN §22): a queue token while waiting,
// then a concurrency token once admitted. The queue token is released
// at admission so Inflight counts each request exactly once.
type hold struct{ c *Client }

func (h *hold) release() {
	<-h.c.conc
}

// acquire implements PLAN §22 admission: a queue slot is taken
// non-blockingly, then a concurrency slot is waited on for up to
// queue_timeout. Failures are bounded and never wait indefinitely.
func (c *Client) acquire(ctx context.Context) (*hold, error) {
	select {
	case c.queue <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, ErrQueueFull
	}

	timer := time.NewTimer(c.queueTimeout)
	defer timer.Stop()
	select {
	case c.conc <- struct{}{}:
		// Admitted: free the queue slot for other waiters so an
		// active request holds exactly one token (Inflight counts
		// queued + active, each once).
		<-c.queue
		return &hold{c: c}, nil
	case <-ctx.Done():
		<-c.queue
		return nil, ctx.Err()
	case <-timer.C:
		<-c.queue
		return nil, ErrQueueFull
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
	cancel := func() {}
	if !req.Stream && c.requestTimeout > 0 {
		fctx, cancel = context.WithTimeout(ctx, c.requestTimeout)
	}
	defer cancel()

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

	resp, err := c.http.Do(outReq)
	if err != nil {
		h.release()
		return nil, requestError(err)
	}

	streamBody := req.Stream && resp.StatusCode == http.StatusOK
	if !streamBody {
		defer resp.Body.Close()
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, int64(c.maxResponseBytes)+1))
		if len(data) > c.maxResponseBytes {
			h.release()
			return nil, ErrTooLarge
		}
		if rerr != nil {
			// Partial-body read failure: the buffered body cannot be
			// trusted, so only the sanitized class comes back.
			h.release()
			return nil, bodyReadError(rerr)
		}
		if resp.StatusCode >= 400 {
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

// requestError classifies an http.Client.Do error (no response headers
// observed) into a sanitized class (PLAN §43, §23). Mellomting's own
// dialer sentinels pass through unchanged so their retry semantics
// survive the http.Client's url.Error wrapping.
func requestError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, ErrConnect) || errors.Is(err, ErrPolicy) || errors.Is(err, ErrDialTimeout) {
		return err
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			// No headers within the ResponseHeaderTimeout bound (the
			// request-total deadline surfaces without a net.Error).
			return ErrHeaderTimeout
		}
		return ErrConnect
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrTimeout
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

// Inflight reports the current admission load: queued waiters plus
// active requests, each counted once (PLAN §19, §22). It is a snapshot
// for least-inflight routing and is always >= 0. For live streams the
// slot is held until Result.Close, so Inflight stays accurate for the
// full generation, not just the header exchange.
func (c *Client) Inflight() int {
	return len(c.queue) + len(c.conc)
}

// Name returns the configured backend name (safe to log).
func (c *Client) Name() string { return c.name }

// UpstreamModel returns the upstream model name (safe to log).
func (c *Client) UpstreamModel() string { return c.upstream }

// StreamIdleTimeout is the backend stream-idle bound (PLAN §24).
func (c *Client) StreamIdleTimeout() time.Duration { return c.streamIdle }
