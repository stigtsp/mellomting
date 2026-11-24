package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mellomting/internal/discovery"
)

// Options configures one agent connection: one edge, one local
// inference server.
type Options struct {
	// Edge is the edge's base URL (http or https). A path prefix is kept,
	// for an edge behind a reverse proxy under a sub-path.
	Edge *url.URL
	// Key is the raw agent key. It is sent only to the edge.
	Key string
	// Backend is the local inference server's base URL.
	Backend *url.URL
	// BackendKey, when set, is sent to the local server as a bearer
	// token. The edge never sees it.
	BackendKey string
	// RootCAs verifies an https edge; nil uses the system pool.
	RootCAs *x509.CertPool
	// BackendRootCAs verifies an https local server; nil uses the system
	// pool. A sandboxed caller preloads it: the pool is otherwise read on
	// the first handshake, after confinement.
	BackendRootCAs *x509.CertPool
	// UserAgent identifies the agent in the attach request.
	UserAgent string
	// Metrics lets the edge read the local server's load report
	// (attach.metrics, default on).
	Metrics bool
	// Decision, when set, makes the local server a decision server
	// (SYSTEMONE_PLAN §D13): its models are declared rather than
	// discovered, and only POST /v1/systemone is relayed to it.
	Decision *Decision
	Log      *slog.Logger
}

// Decision declares a local decision server, which has no model list.
type Decision struct {
	// Models are the ids the server accepts, as configured.
	Models []string
	// Health is the path probed for readiness and liveness; any 2xx
	// means up.
	Health string
}

// drainTimeout bounds a graceful detach: in-flight streams of a few
// minutes, normal for LLM generations, finish across a model change. On
// shutdown the daemon's grace period cuts it short.
const drainTimeout = 5 * time.Minute

var (
	handshakeTimeout = 30 * time.Second
	minBackoff       = time.Second
	maxBackoff       = 30 * time.Second
	// A connection that lasted this long resets the backoff.
	stableAfter = time.Minute
)

// Run keeps the agent attached until ctx ends, reconnecting with
// exponential backoff whenever the connection drops or is refused.
//
// The agent never cuts streams it can let finish. A model change opens
// the new attachment before draining the old one; a local server that
// went away, and ctx ending, drain the attachment: HTTP/2 GOAWAY, then
// up to drainTimeout for in-flight streams. Only a connection that is
// already gone is closed outright. Run returns when every drain is done.
func Run(ctx context.Context, o Options) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	st := &stats{}
	handler := backendHandler(o, st)
	var drains sync.WaitGroup
	defer drains.Wait()

	backoff := minBackoff
	var cur *attached
	var lastErr error
	for {
		if cur == nil {
			if lastErr != nil {
				wait := jitter(backoff)
				o.Log.Warn("agent disconnected; reconnecting", "edge", o.Edge.Redacted(), "error", lastErr, "retry_in", wait.Round(time.Millisecond).String())
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
				backoff = min(backoff*2, maxBackoff)
			}
			// Attach only once the local server answers: an inference
			// server can take many minutes to load its model, and
			// attaching before that only makes the edge's discovery fail.
			models, ok := waitForBackend(ctx, o)
			if !ok {
				return
			}
			a, err := attach(ctx, o, handler, models, st)
			if err != nil {
				lastErr = err
				continue
			}
			cur, lastErr = a, nil
		}

		ev := cur.wait(ctx)
		if ev.reason != endModelsChanged {
			cur.logEnd(ev.reason)
		}
		switch ev.reason {
		case endShutdown:
			cur.drain(drainTimeout)
			return
		case endConnLost:
			// Only here: a retried model change also wakes this loop on
			// a long-lived attachment, and resetting then would undo
			// that retry's backoff.
			if time.Since(cur.since) > stableAfter {
				backoff = minBackoff
			}
			cur.close()
			cur, lastErr = nil, errors.New("connection closed or lost")
		case endBackendDown:
			old := cur
			drains.Go(func() { old.drain(drainTimeout) })
			cur = nil
			backoff = minBackoff
		case endModelsChanged:
			old := cur
			a, err := attach(ctx, o, handler, ev.models, st)
			if err == nil {
				// The edge reads the list after the upgrade and closes
				// the connection if it refuses it; draining the old
				// attachment before that would leave nothing serving.
				if err = a.accepted(ctx); err != nil {
					a.close()
				}
			}
			switch {
			case err == nil:
				// Make before break: the new attachment takes new
				// requests while the old one's streams finish.
				old.logEnd(ev.reason)
				cur = a
				drains.Go(func() { old.drain(drainTimeout) })
				backoff = minBackoff
			case atCapacity(err):
				// The key is at its attachment cap, and the old
				// attachment holds the slot. Its GOAWAY frees it (the
				// edge does not count draining attachments), so drain it
				// and attach the new list right away: break before
				// make, with a gap of one handshake.
				o.Log.Info("attachment cap reached; replacing the attachment in place")
				old.logEnd(ev.reason)
				drains.Go(func() { old.drain(drainTimeout) })
				cur, err = attachSoon(ctx, o, handler, ev.models, st)
				if err != nil {
					lastErr = err
				}
				backoff = minBackoff
			default:
				// Keep serving the old models rather than none, and try
				// again shortly.
				wait := jitter(backoff)
				backoff = min(backoff*2, maxBackoff)
				o.Log.Warn("could not attach the changed model list; keeping the current attachment",
					"error", err, "retry_in", wait.Round(time.Millisecond).String())
				old.retryChange(ev.models, wait)
			}
		}
	}
}

// Variables rather than constants only so tests can shorten them.
var (
	// backendPollInterval is how often a not-yet-ready local server is
	// probed.
	backendPollInterval = 5 * time.Second
	// backendWatchInterval is how often an attached agent re-checks its
	// local server.
	backendWatchInterval = 15 * time.Second
)

// backendFailLimit is how many failed checks in a row make an attached
// agent detach.
const backendFailLimit = 2

// jitter spreads reconnects over [d/2, d) so agents cut off together do
// not return together.
func jitter(d time.Duration) time.Duration {
	if d < 2 {
		return d
	}
	return d/2 + rand.N(d/2)
}

// probeBackend asks the local server for its models. A decision server
// has none to list: it is asked for its health, and its models are the
// declared ones.
func probeBackend(ctx context.Context, o Options) ([]discovery.Model, error) {
	if o.Decision != nil {
		return probeDecision(ctx, o)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: backendTransport(o)}
	u := *o.Backend
	u.Path = path.Join("/", o.Backend.Path, "/v1/models")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if o.BackendKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.BackendKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/models: %s", resp.Status)
	}
	models, err := readModelList(resp.Body, probeMaxModels)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errModelList, err)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: GET /v1/models: no models", errModelList)
	}
	return models, nil
}

// probeDecision checks a decision server's health path and returns its
// declared models, sorted as a discovered list is.
func probeDecision(ctx context.Context, o Options) ([]discovery.Model, error) {
	client := &http.Client{
		Timeout:       10 * time.Second,
		Transport:     backendTransport(o),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	u := *o.Backend
	u.Path = path.Join("/", o.Backend.Path, o.Decision.Health)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if o.BackendKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.BackendKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("GET %s: %s", o.Decision.Health, resp.Status)
	}
	return decisionModels(o.Decision), nil
}

func decisionModels(d *Decision) []discovery.Model {
	ids := slices.Clone(d.Models)
	slices.Sort(ids)
	ms := make([]discovery.Model, len(ids))
	for i, id := range ids {
		ms[i] = discovery.Model{ID: id}
	}
	return ms
}

// errModelList marks a server that answered but whose model list cannot
// be used: a configuration problem to report, not a server still
// starting.
var errModelList = errors.New("model list rejected")

// probeClass groups probe failures for logging: a new class is logged,
// a repeat of the same one is not (the text varies, e.g. ports).
func probeClass(err error) string {
	switch {
	case errors.Is(err, errModelList):
		return err.Error()
	case strings.HasPrefix(err.Error(), "GET /"):
		return err.Error() // HTTP status of the model list or health path
	default:
		return "unreachable"
	}
}

// waitForBackend blocks until the local server lists at least one model,
// logging when it starts and stops waiting, and returns the ids. It
// reports false when ctx ends first.
func waitForBackend(ctx context.Context, o Options) ([]discovery.Model, bool) {
	waiting, last := false, ""
	for {
		ids, err := probeBackend(ctx, o)
		if err == nil {
			if waiting {
				o.Log.Info("backend ready", "models", strings.Join(modelIDs(ids), ","))
			}
			return ids, true
		}
		if ctx.Err() != nil {
			return nil, false
		}
		// Log each new kind of failure, so a server that comes up with
		// an unusable list is not hidden behind its earlier "connection
		// refused".
		if class := probeClass(err); class != last {
			if errors.Is(err, errModelList) {
				o.Log.Warn("backend answers but its model list is unusable; waiting", "error", err, "poll_every", backendPollInterval.String())
			} else {
				o.Log.Info("waiting for backend", "error", err, "poll_every", backendPollInterval.String())
			}
			last, waiting = class, true
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(backendPollInterval):
		}
	}
}

// endReason is why an attachment ends.
type endReason int

const (
	endConnLost      endReason = iota // the edge or the network closed it
	endShutdown                       // the agent is stopping
	endBackendDown                    // the local server stopped answering
	endModelsChanged                  // the local server's models changed
)

func (r endReason) String() string {
	return [...]string{"connection closed or lost", "agent shutting down", "local server unavailable", "local server models changed"}[r]
}

type endEvent struct {
	reason endReason
	models []discovery.Model // the new list, for endModelsChanged
}

// watchBackend re-checks the local server while attached and reports
// when it stops answering or its models change. A failed check counts
// only while nothing is in flight and nothing was served recently:
// a busy server answers the probe slowly (llama.cpp queues it behind
// its streams), and live traffic is better evidence than the probe.
func watchBackend(ctx context.Context, o Options, models []discovery.Model, st *stats, out chan<- endEvent) {
	failures := 0
	t := time.NewTicker(backendWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ids, err := probeBackend(ctx, o)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil && (st.active.Load() > 0 || st.recentlyServed(2*backendWatchInterval)):
			o.Log.Debug("backend probe failed while serving traffic; not counted", "error", err)
			failures = 0
			continue
		case err != nil:
			failures++
			if failures < backendFailLimit {
				continue
			}
			o.Log.Warn("backend unavailable; detaching", "error", err, "failed_checks", failures)
			send(ctx, out, endEvent{reason: endBackendDown})
		case !slices.Equal(ids, models):
			o.Log.Info("backend models changed; reattaching", "was", strings.Join(modelIDs(models), ","), "now", strings.Join(modelIDs(ids), ","))
			send(ctx, out, endEvent{reason: endModelsChanged, models: ids})
		default:
			failures = 0
			continue
		}
		return
	}
}

// send delivers ev unless ctx ends first: once an attachment is replaced
// nobody reads its events, and close waits for its helpers.
func send(ctx context.Context, out chan<- endEvent, ev endEvent) {
	select {
	case out <- ev:
	case <-ctx.Done():
	}
}

// attachSoon attaches, retrying a capacity refusal for a moment: the
// edge frees the old attachment's slot when it reads its GOAWAY, which
// may land just after the new handshake.
func attachSoon(ctx context.Context, o Options, handler http.Handler, models []discovery.Model, st *stats) (*attached, error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		a, err := attach(ctx, o, handler, models, st)
		if err == nil || !atCapacity(err) || time.Now().After(deadline) {
			return a, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// attached is one live attachment: the upgraded connection and the
// HTTP/2 server answering the edge on it.
type attached struct {
	o        Options
	tc       *trackedConn
	srv      *http.Server
	since    time.Time
	st       *stats
	before   int64 // st.requests at attach, for the per-attachment count
	events   chan endEvent
	hctx     context.Context    // lives as long as the attachment
	stop     context.CancelFunc // ends the helpers
	helpers  sync.WaitGroup
	closeOne sync.Once

	// watchStop ends the current backend watcher; there is one at a time.
	watchStop context.CancelFunc
	// discovered is closed once the edge has read the model list.
	discovered chan struct{}

	// A model change this attachment could not hand over yet: retry
	// fires to try again with pending.
	retry   <-chan time.Time
	pending []discovery.Model
}

// retryChange keeps the attachment serving after a failed replacement,
// tries the replacement again after d, and meanwhile keeps watching the
// local server against the new list.
func (a *attached) retryChange(models []discovery.Model, d time.Duration) {
	a.pending = models
	a.retry = time.After(d)
	a.watch(models)
}

// watch replaces the backend watcher with one checking against models.
// The previous one would otherwise run until the attachment closes: the
// server still serves the list it waits to see change.
func (a *attached) watch(models []discovery.Model) {
	if a.watchStop != nil {
		a.watchStop()
	}
	wctx, stop := context.WithCancel(a.hctx)
	a.watchStop = stop
	a.helpers.Go(func() { watchBackend(wctx, a.o, models, a.st, a.events) })
}

// Bounds on confirming that the edge accepted a replacement attachment:
// the edge reads the model list within its discoveryTimeout and closes
// the connection right after if it refuses the list.
var (
	acceptTimeout = discoveryTimeout + 5*time.Second
	acceptSettle  = 2 * time.Second
)

// accepted waits until the edge has read the attachment's model list and
// kept the connection open for acceptSettle. A connection closed first
// means the edge refused the list (over agents.max_models, or nothing
// the key may register).
func (a *attached) accepted(ctx context.Context) error {
	timeout := time.NewTimer(acceptTimeout)
	defer timeout.Stop()
	select {
	case <-a.discovered:
	case <-a.tc.Done():
		return errors.New("edge closed the new attachment before reading its models")
	case <-timeout.C:
		return errors.New("edge did not read the new attachment's models")
	case <-ctx.Done():
		return ctx.Err()
	}
	settle := time.NewTimer(acceptSettle)
	defer settle.Stop()
	select {
	case <-settle.C:
		return nil
	case <-a.tc.Done():
		return errors.New("edge refused the new model list")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// attach performs one handshake and starts serving the edge.
func attach(ctx context.Context, o Options, handler http.Handler, models []discovery.Model, st *stats) (*attached, error) {
	conn, br, err := handshake(ctx, o)
	if err != nil {
		return nil, err
	}
	a := &attached{
		o:      o,
		tc:     newTrackedConn(conn, br),
		since:  time.Now(),
		st:     st,
		before: st.requests.Load(),
		events: make(chan endEvent, 1),

		discovered: make(chan struct{}),
	}
	var discoveredOnce sync.Once
	a.srv = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handler.ServeHTTP(w, r)
			if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
				discoveredOnce.Do(func() { close(a.discovered) })
			}
		}),
		ReadHeaderTimeout: handshakeTimeout,
		HTTP2: &http.HTTP2Config{
			// Above any admission cap (max_concurrency is at most
			// 1024): the edge's transport would otherwise try to open a
			// second connection, which an agent does not have.
			MaxConcurrentStreams: 4096,
			SendPingTimeout:      pingInterval,
			PingTimeout:          pingTimeout,
		},
		ErrorLog: slog.NewLogLogger(o.Log.Handler(), slog.LevelDebug),
	}
	a.srv.Protocols = new(http.Protocols)
	a.srv.Protocols.SetUnencryptedHTTP2(true)

	hctx, stop := context.WithCancel(context.Background())
	a.hctx, a.stop = hctx, stop
	a.helpers.Go(func() { _ = a.srv.Serve(newOneConnListener(a.tc)) })
	a.watch(models)
	a.helpers.Go(func() { logStatus(hctx, o, st, a.tc.Done()) })
	o.Log.Info("agent attached", "edge", o.Edge.Redacted(), "models", strings.Join(modelIDs(models), ","))
	return a, nil
}

// wait blocks until the attachment should end and says why.
func (a *attached) wait(ctx context.Context) endEvent {
	select {
	case <-a.tc.Done():
		return endEvent{reason: endConnLost}
	case <-ctx.Done():
		return endEvent{reason: endShutdown}
	case ev := <-a.events:
		return ev
	case <-a.retry:
		a.retry = nil
		return endEvent{reason: endModelsChanged, models: a.pending}
	}
}

func (a *attached) logEnd(r endReason) {
	a.o.Log.Info("agent detaching", "reason", r.String(),
		"attached_for", time.Since(a.since).Round(time.Second).String(),
		"requests", a.st.requests.Load()-a.before, "in_flight", a.st.active.Load())
}

// drain stops taking new streams (HTTP/2 GOAWAY), lets in-flight ones
// finish for up to d, then closes the connection. The edge stops routing
// to the attachment when it sees the GOAWAY.
func (a *attached) drain(d time.Duration) {
	start := time.Now()
	sctx, cancel := context.WithTimeout(context.Background(), d)
	err := a.srv.Shutdown(sctx)
	cancel()
	if err != nil {
		a.o.Log.Warn("drain timed out; closing with streams in flight", "drain_timeout", d.String(), "in_flight", a.st.active.Load())
	} else {
		a.o.Log.Info("drained", "took", time.Since(start).Round(time.Millisecond).String())
	}
	a.close()
}

// close ends the attachment at once.
func (a *attached) close() {
	a.closeOne.Do(func() {
		_ = a.tc.Close()
		_ = a.srv.Close()
		a.stop()
		a.helpers.Wait()
	})
}

// refusedError is an attach the edge answered without upgrading.
type refusedError struct {
	status int
	text   string
}

func (e *refusedError) Error() string { return "edge refused attach: " + e.text }

// atCapacity reports an attach refused because an attachment limit was
// reached (503).
func atCapacity(err error) bool {
	re, ok := errors.AsType[*refusedError](err)
	return ok && re.status == http.StatusServiceUnavailable
}

// newDialer is the agent's TCP dialer, with multipath TCP off like every
// dialer Mellomting owns (PLAN §61, §83).
// backendTransport reaches the local server: no proxy, and no keep-alive
// (see the relay).
func backendTransport(o Options) *http.Transport {
	return &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext:       newDialer(10 * time.Second).DialContext,
		TLSClientConfig:   &tls.Config{RootCAs: o.BackendRootCAs, MinVersion: tls.VersionTLS12},
	}
}

func newDialer(timeout time.Duration) *net.Dialer {
	d := &net.Dialer{Timeout: timeout}
	d.SetMultipathTCP(false)
	return d
}

// maxHandshakeResponseBytes bounds the edge's answer to the upgrade.
const maxHandshakeResponseBytes = 64 << 10

// handshake dials the edge and upgrades the connection. On success the
// connection is ready for HTTP/2 with the agent as server.
func handshake(ctx context.Context, o Options) (net.Conn, *bufio.Reader, error) {
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	host := o.Edge.Host
	if o.Edge.Port() == "" {
		port := "80"
		if o.Edge.Scheme == "https" {
			port = "443"
		}
		host = net.JoinHostPort(o.Edge.Hostname(), port)
	}
	conn, err := newDialer(handshakeTimeout).DialContext(hctx, "tcp", host)
	if err != nil {
		return nil, nil, err
	}
	if o.Edge.Scheme == "https" {
		// HTTP/1.1 only: an Upgrade cannot happen over HTTP/2, and TLS
		// front ends (reverse proxies, tunnels) would otherwise
		// negotiate it.
		tconn := tls.Client(conn, &tls.Config{
			ServerName: o.Edge.Hostname(),
			RootCAs:    o.RootCAs,
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"http/1.1"},
		})
		if err := tconn.HandshakeContext(hctx); err != nil {
			_ = conn.Close()
			return nil, nil, err
		}
		conn = tconn
	}
	if dl, ok := hctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	u := *o.Edge
	u.Path = path.Join("/", o.Edge.Path, AttachPath)
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+o.Key)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", UpgradeProtocol)
	if o.UserAgent != "" {
		req.Header.Set("User-Agent", o.UserAgent)
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	// Bound the response head as http.Transport's MaxResponseHeaderBytes
	// does. The limit is lifted after the head: the same reader then
	// carries the HTTP/2 session.
	lr := &io.LimitedReader{R: conn, N: maxHandshakeResponseBytes}
	br := bufio.NewReader(lr)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	lr.N = math.MaxInt64
	if resp.StatusCode != http.StatusSwitchingProtocols ||
		!strings.EqualFold(resp.Header.Get("Upgrade"), UpgradeProtocol) {
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, nil, &refusedError{status: resp.StatusCode, text: resp.Status}
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, br, nil
}

// relayable reports whether a request is one the edge forwards (its
// allow-listed inference routes, PLAN §11.1) and nothing else. The edge
// never sends anything else, so this only matters if something other
// than the edge is talking to the agent; it keeps the agent from
// widening what reaches the local server, such as admin or LoRA routes
// under /v1/, or dot segments a server might normalize.
//
// A decision server is relayed POST /v1/systemone and nothing else, and
// only a decision server is relayed it (SYSTEMONE_PLAN §D13).
func relayable(r *http.Request, decision bool) bool {
	p := r.URL.EscapedPath()
	if r.URL.RawQuery != "" || p != r.URL.Path || strings.Contains(p, "//") {
		return false // encoded characters, a query, or empty segments
	}
	if decision {
		return r.Method == http.MethodPost && p == "/v1/systemone"
	}
	switch {
	case r.Method == http.MethodGet && p == "/v1/models":
		return true
	case r.Method == http.MethodPost && (p == "/v1/chat/completions" || p == "/v1/completions" ||
		p == "/v1/embeddings" || p == "/v1/responses"):
		return true
	}
	rest, ok := strings.CutPrefix(p, "/v1/responses/")
	if !ok {
		return false
	}
	id, cancel := strings.CutSuffix(rest, "/cancel")
	if !safeSegment(id) {
		return false
	}
	return (r.Method == http.MethodGet && !cancel) || (r.Method == http.MethodPost && cancel)
}

// safeSegment matches the response-ID alphabet the edge enforces.
func safeSegment(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// stats counts what the agent relayed, for its status log lines.
type stats struct {
	active   atomic.Int64 // requests being relayed now
	requests atomic.Int64 // requests relayed
	errors   atomic.Int64 // requests the local server failed (no response or 5xx)
	lastOK   atomic.Int64 // unix nanos of the last relayed response below 500
}

// recentlyServed reports whether the local server answered a relayed
// request within d: live traffic is better evidence of health than a
// health probe that a busy server answers slowly.
func (s *stats) recentlyServed(d time.Duration) bool {
	t := s.lastOK.Load()
	return t != 0 && time.Since(time.Unix(0, t)) < d
}

// statusInterval is how often an attached agent logs its counters while
// there is traffic.
var statusInterval = time.Minute

// logStatus logs the agent's counters every statusInterval while
// attached, skipping quiet intervals.
func logStatus(ctx context.Context, o Options, st *stats, done <-chan struct{}) {
	t := time.NewTicker(statusInterval)
	defer t.Stop()
	var last int64 = -1
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
		}
		req, act := st.requests.Load(), st.active.Load()
		if req == last && act == 0 {
			continue
		}
		last = req
		o.Log.Info("agent status", "active", act, "requests", req, "errors", st.errors.Load())
	}
}

// statusWriter records the response status and size for the debug log.
// It forwards Flush (SSE) through Unwrap, which http.ResponseController
// and httputil.ReverseProxy use.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// backendHandler relays the edge's requests to the local inference
// server. Only /v1/ paths are relayed: the edge forwards nothing else,
// and the agent does not widen what reaches the server.
func backendHandler(o Options, st *stats) http.Handler {
	rp := &httputil.ReverseProxy{
		// A fresh loopback connection per request. Inference servers
		// close idle keep-alive connections on their own schedule
		// (llama.cpp after a few requests, uvicorn after seconds), and a
		// request written onto a connection the server is closing fails
		// with EOF; a streamed POST body cannot be replayed, so the
		// transport cannot retry it. Connecting is negligible next to an
		// inference.
		Transport: func() *http.Transport {
			t := backendTransport(o)
			t.DisableCompression = true
			return t
		}(),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(o.Backend)
			pr.Out.Header.Del("Authorization")
			if o.BackendKey != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+o.BackendKey)
			}
		},
		// Stream every byte as it arrives (SSE).
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			o.Log.Warn("backend request failed", "path", r.URL.Path, "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"agent backend unavailable","type":"api_error","code":"backend_unavailable"}}`))
		},
	}
	metrics := metricsHandler(o.Metrics, localFetch(o))
	types := modelTypesHandler(o.Decision)
	var models http.Handler
	if o.Decision != nil {
		models = decisionModelList(o.Decision)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == MetricsPath {
			metrics.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == ModelTypesPath {
			types.ServeHTTP(w, r)
			return
		}
		if models != nil && r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			models.ServeHTTP(w, r)
			return
		}
		if !relayable(r, o.Decision != nil) {
			http.NotFound(w, r)
			return
		}
		st.active.Add(1)
		st.requests.Add(1)
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		// Deferred: when the client hangs up mid-stream, ReverseProxy
		// aborts with panic(http.ErrAbortHandler), and a count left
		// raised would make the health watcher ignore a dead server.
		defer func() {
			st.active.Add(-1)
			switch {
			case r.Context().Err() != nil:
				// The edge's client hung up: ReverseProxy answers 502
				// or nothing, which says nothing about the server.
			case sw.status >= 500 || sw.status == 0:
				st.errors.Add(1)
			default:
				st.lastOK.Store(time.Now().UnixNano())
			}
			// Paths and sizes only: never bodies, which carry prompts.
			o.Log.Debug("relayed", "method", r.Method, "path", r.URL.Path, "status", sw.status,
				"bytes", sw.bytes, "duration_ms", time.Since(start).Milliseconds(), "canceled", r.Context().Err() != nil)
		}()
		rp.ServeHTTP(sw, r)
	})
}
