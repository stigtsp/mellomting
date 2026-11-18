package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mellomting/internal/auth"
	"mellomting/internal/backend"
	"mellomting/internal/backendmetrics"
	"mellomting/internal/config"
	"mellomting/internal/discovery"
	"mellomting/internal/routing"
)

const (
	// discoveryTimeout bounds the attach-time GET /v1/models.
	discoveryTimeout = 30 * time.Second
	// keyCheckInterval is how often attachments are re-checked against
	// the live key store.
	keyCheckInterval = 5 * time.Second
)

// HTTP/2 keepalive, used on both ends: a silent peer is detected and the
// connection closed. Variables only so tests can shorten them.
var (
	pingInterval = 15 * time.Second
	pingTimeout  = 15 * time.Second
)

// backendPrefix starts every attachment's backend name. Configured
// backends are named "auto-...", so the two never collide.
const backendPrefix = "agent-"

// IsBackendName reports whether a backend name is an attached agent's.
func IsBackendName(name string) bool { return strings.HasPrefix(name, backendPrefix) }

// Detach reasons, as logged and as the last state of an attachment.
const (
	reasonConnLost   = "connection closed or lost" // agent hung up, network or keepalive failure
	reasonGoAway     = "agent detached gracefully" // GOAWAY: agent stopping, server down or models changed
	reasonKey        = "agent key no longer active"
	reasonShutdown   = "edge shutting down"
	reasonDiscovery  = "model discovery failed"
	reasonNoModels   = "no servable models"
	reasonClientInit = "backend client setup failed"
)

// Registry is the edge's table of attached agents (PLAN §104).
type Registry struct {
	cfg              config.Agents
	router           *routing.Router
	maxResponseBytes int
	log              *slog.Logger

	active atomic.Pointer[func(*auth.Key) bool]

	mu     sync.Mutex
	atts   map[string]*attachment
	seq    uint64
	closed bool
}

type attachment struct {
	name     string
	key      auth.Key // the record it authenticated with, for re-checks
	keyID    string
	keyName  string
	ns       string
	remote   string
	since    time.Time
	conn     *trackedConn
	tr       *http.Transport
	counters *counters

	// Set once discovery finishes; read under Registry.mu.
	client *backend.Client
	models []Model

	// metrics is the agent's latest report of its server's load.
	metrics atomic.Pointer[backendmetrics.Reading]

	reason   atomic.Pointer[string] // why the attachment ended, if it did
	draining atomic.Bool            // no new requests routed to it
	ready    chan struct{}          // closed once conn is set
}

// counters is what the edge observed on one attachment's transport.
type counters struct {
	requests atomic.Int64 // requests forwarded to the agent
	failures atomic.Int64 // transport-level failures (no response)
	active   atomic.Int64 // requests whose response body is still open
}

// Model is one model an attachment registered.
type Model struct {
	Name          string `json:"name"`
	ContextLength int    `json:"context_length,omitempty"`
}

// Record is one attachment as `mellomting top` and the admin socket
// show it. It carries no key material.
type Record struct {
	Name           string  `json:"name"`
	KeyName        string  `json:"key_name"`
	Namespace      string  `json:"namespace"`
	Remote         string  `json:"remote"`
	AgeMillis      int64   `json:"age_ms"`
	State          string  `json:"state"` // "discovering", "serving" or "draining"
	Models         []Model `json:"models,omitempty"`
	Admitted       int     `json:"admitted"`
	Queued         int     `json:"queued"`
	MaxConcurrency int     `json:"max_concurrency"`
	Streams        int64   `json:"streams"`
	Requests       int64   `json:"requests"`
	Failures       int64   `json:"failures"`
	// Metrics is the agent's report of its server's load (PLAN §104),
	// nil until the edge has read one.
	Metrics *backendmetrics.Reading `json:"metrics,omitempty"`
}

// NewRegistry builds an empty registry. maxResponseBytes is the server's
// buffered-response bound, applied to agents like any backend.
func NewRegistry(cfg config.Agents, router *routing.Router, maxResponseBytes int, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{
		cfg:              cfg,
		router:           router,
		maxResponseBytes: maxResponseBytes,
		log:              log,
		atts:             make(map[string]*attachment),
	}
}

// SetActive installs the live key check: it reports whether the key
// record an attachment authenticated with is still in force unchanged.
func (g *Registry) SetActive(f func(*auth.Key) bool) { g.active.Store(&f) }

// fullLocked names the limit that refuses an attachment for keyID, or
// returns "" when one more fits. g.mu must be held.
//
// Draining attachments do not count against the caps, so an agent can
// replace its attachment (a model change) while the old one's streams
// finish. They are still bounded: draining and live together may reach
// twice each cap, so an agent that never closes drained connections
// cannot pile them up.
func (g *Registry) fullLocked(keyID string) string {
	if g.closed {
		return "edge shutting down"
	}
	live, all, keyLive, keyAll := 0, 0, 0, 0
	for _, a := range g.atts {
		draining := a.draining.Load()
		all++
		if !draining {
			live++
		}
		if a.keyID == keyID {
			keyAll++
			if !draining {
				keyLive++
			}
		}
	}
	switch {
	case live >= g.cfg.MaxAttachments || all >= 2*g.cfg.MaxAttachments:
		return "agents.max_attachments"
	case keyLive >= g.cfg.MaxAttachmentsPerKey || keyAll >= 2*g.cfg.MaxAttachmentsPerKey:
		return "agents.max_attachments_per_key"
	}
	return ""
}

// Client returns an attached agent's backend client.
func (g *Registry) Client(name string) (*backend.Client, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.atts[name]
	if !ok || a.client == nil {
		return nil, false
	}
	return a.client, true
}

// Inflight reports an attached agent's admitted plus queued requests.
func (g *Registry) Inflight(name string) int {
	if c, ok := g.Client(name); ok {
		return c.Inflight()
	}
	return 0
}

// Snapshot lists the attachments, oldest first.
func (g *Registry) Snapshot() []Record {
	now := time.Now()
	g.mu.Lock()
	out := make([]Record, 0, len(g.atts))
	for _, a := range g.atts {
		r := Record{
			Name:           a.name,
			KeyName:        a.keyName,
			Namespace:      a.ns,
			Remote:         a.remote,
			AgeMillis:      now.Sub(a.since).Milliseconds(),
			State:          "discovering",
			Models:         slices.Clone(a.models),
			MaxConcurrency: g.cfg.MaxConcurrency,
			Streams:        a.counters.active.Load(),
			Requests:       a.counters.requests.Load(),
			Failures:       a.counters.failures.Load(),
			Metrics:        a.metrics.Load(),
		}
		if a.client != nil {
			r.State = "serving"
			r.Admitted, r.Queued = a.client.Admitted(), a.client.Queued()
		}
		if a.draining.Load() {
			r.State = "draining"
		}
		out = append(out, r)
	}
	g.mu.Unlock()
	slices.SortFunc(out, func(x, y Record) int { return int(y.AgeMillis - x.AgeMillis) })
	return out
}

// DrainAll releases requests queued for any attached agent (graceful
// shutdown, PLAN §74).
func (g *Registry) DrainAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, a := range g.atts {
		if a.client != nil {
			a.client.Drain()
		}
	}
}

// Close detaches every agent and refuses new ones.
func (g *Registry) Close() {
	g.mu.Lock()
	g.closed = true
	atts := make([]*attachment, 0, len(g.atts))
	for _, a := range g.atts {
		atts = append(atts, a)
	}
	g.mu.Unlock()
	for _, a := range atts {
		a.end(reasonShutdown)
	}
}

// Run re-checks attachments against the key store until ctx ends, so
// revoking, disabling or expiring an agent key detaches its agents.
func (g *Registry) Run(ctx context.Context) {
	t := time.NewTicker(keyCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.CheckKeys()
		}
	}
}

// CheckKeys detaches every agent whose key is no longer active. Run
// calls it periodically; the daemon also calls it right after a key
// store reload, so a revocation applies at once.
func (g *Registry) CheckKeys() {
	fp := g.active.Load()
	if fp == nil {
		return
	}
	active := *fp
	g.mu.Lock()
	var stale []*attachment
	for _, a := range g.atts {
		if !active(&a.key) {
			stale = append(stale, a)
		}
	}
	g.mu.Unlock()
	for _, a := range stale {
		a.end(reasonKey)
	}
}

// end records why the attachment ends (first reason wins) and closes its
// connection. A reservation not yet attached is ended as soon as its
// connection arrives.
func (a *attachment) end(reason string) {
	a.reason.CompareAndSwap(nil, &reason)
	go func() {
		<-a.ready
		if a.conn != nil { // nil for a cancelled reservation
			_ = a.conn.Close()
		}
	}()
}

// Reservation is an attachment slot claimed before the upgrade, so the
// edge answers 503 for a full cap instead of upgrading a connection it
// would then have to drop. Exactly one of Attach or Cancel follows.
type Reservation struct {
	g *Registry
	a *attachment
}

// Reserve claims an attachment slot for key, or names the limit that
// refuses it. The caps are checked and the slot taken under one lock, so
// two agents racing for the last slot cannot both be upgraded.
func (g *Registry) Reserve(key *auth.Key, remote string) (*Reservation, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if limit := g.fullLocked(key.ID); limit != "" {
		return nil, limit // logged, rate-limited, by the caller
	}
	g.seq++
	a := &attachment{
		name:     fmt.Sprintf("%s%s-%d", backendPrefix, key.Name, g.seq),
		key:      *key,
		keyID:    key.ID,
		keyName:  key.Name,
		ns:       key.Agent,
		remote:   remote,
		since:    time.Now(),
		counters: &counters{},
		ready:    make(chan struct{}),
	}
	g.atts[a.name] = a
	return &Reservation{g: g, a: a}, ""
}

// Cancel releases a reservation whose upgrade did not happen.
func (r *Reservation) Cancel() {
	r.g.mu.Lock()
	delete(r.g.atts, r.a.name)
	r.g.mu.Unlock()
	close(r.a.ready) // releases an end that ran meanwhile
}

// Attach takes over the upgraded connection and serves it until it
// closes. br holds any bytes read past the handshake. It returns
// immediately; the attachment lives on its own goroutine.
func (r *Reservation) Attach(conn net.Conn, br *bufio.Reader) {
	g, a := r.g, r.a
	g.mu.Lock()
	a.conn = newTrackedConn(conn, br)
	g.mu.Unlock()
	close(a.ready)
	// The key was checked against the store snapshot this request was
	// authenticated under; a reload since then has already run its
	// CheckKeys, so check this attachment against the current one.
	g.CheckKeys()
	go g.serve(a)
}

func (g *Registry) serve(a *attachment) {
	defer g.detach(a)

	a.tr = newEdgeTransport(a.conn, func() {
		// Runs in the connection's read path, before any EOF: record
		// the reason (without closing; streams drain first) and leave
		// the routing change to its own goroutine.
		r := reasonGoAway
		a.reason.CompareAndSwap(nil, &r)
		go g.stopRouting(a)
	}, func() { g.stopRouting(a) })
	start := time.Now()
	// With a register list the server may advertise many more models
	// than it may register (an Ollama host lists everything it has
	// pulled): read up to the probe bound, filter, then apply max_models
	// to what is left (PLAN §104.4).
	readMax := g.cfg.MaxModels
	if len(a.key.Register) > 0 {
		readMax = probeMaxModels
	}
	models, err := discover(a.tr, readMax, a.conn.Done())
	if err == nil && len(a.key.Register) > 0 {
		models, err = g.filterRegister(a, models)
	}
	if err != nil {
		g.log.Warn("agent model discovery failed", "agent", a.name, "key", a.keyName, "remote", a.remote,
			"error", err, "max_models", g.cfg.MaxModels)
		a.end(reasonDiscovery)
		return
	}

	client, err := backend.New(backend.Options{
		Name:             a.name,
		Cfg:              config.AgentBackend(g.cfg),
		Network:          backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: g.maxResponseBytes,
		Log:              g.log,
		Transport:        &countingTransport{rt: a.tr, c: a.counters},
		Untrusted:        true,
	})
	if err != nil {
		g.log.Error("agent client setup failed", "agent", a.name, "error", err)
		a.end(reasonClientInit)
		return
	}

	// The client is published before any route names this attachment:
	// the proxy resolves a selected backend through Client, and a route
	// without one would fail the request.
	g.mu.Lock()
	a.client = client
	g.mu.Unlock()

	var registered []Model
	for _, m := range models {
		if m.public != "" {
			// A register entry, validated with the key and again by
			// filterRegister; the alias is the public name.
			if err := g.router.AddDynamic(m.public, a.name, m.ID, m.ContextLength, a.keyID); err != nil {
				g.log.Warn("agent model ignored", "agent", a.name, "model", m.public, "upstream", m.ID, "error", err)
				continue
			}
			registered = append(registered, Model{Name: m.public, ContextLength: m.ContextLength})
			continue
		}
		if !validAgentModelID(m.ID) {
			g.log.Warn("agent model ignored: ids must be ASCII letters, digits or ._:@+/-", "agent", a.name, "model", m.ID)
			continue
		}
		if a.ns == config.AgentNamespaceWildcard && strings.Contains(m.ID, "/") {
			// Unprefixed names must not look like another namespace's.
			g.log.Warn("agent model ignored: a * agent key cannot register names containing /", "agent", a.name, "model", m.ID)
			continue
		}
		public := config.AgentModelName(a.ns, m.ID)
		if err := config.ValidateModelName(public); err != nil {
			g.log.Warn("agent model ignored", "agent", a.name, "model", public, "error", err)
			continue
		}
		if err := g.router.AddDynamic(public, a.name, m.ID, m.ContextLength, a.keyID); err != nil {
			g.log.Warn("agent model ignored", "agent", a.name, "model", public, "error", err)
			continue
		}
		registered = append(registered, Model{Name: public, ContextLength: m.ContextLength})
	}
	if a.draining.Load() {
		// GOAWAY arrived during discovery: stopRouting ran before the
		// routes existed. It sets draining before removing routes, so
		// one of the two removes them.
		g.router.RemoveDynamic(a.name)
	}
	g.mu.Lock()
	a.models = registered
	g.mu.Unlock()
	if len(registered) == 0 {
		g.log.Warn("agent attached with no servable models; closing", "agent", a.name, "key", a.keyName)
		a.end(reasonNoModels)
		return
	}
	g.log.Info("agent attached",
		"agent", a.name, "key", a.keyName, "namespace", a.ns, "remote", a.remote,
		"models", modelSummary(registered), "max_concurrency", g.cfg.MaxConcurrency,
		"discovery_ms", time.Since(start).Milliseconds())

	<-a.conn.Done()
}

// stopRouting takes a draining attachment out of routing while its
// in-flight streams finish: the agent sent GOAWAY (a graceful detach) or
// the connection died, and the transport cannot open new streams on it.
func (g *Registry) stopRouting(a *attachment) {
	if a.draining.Swap(true) {
		return
	}
	g.router.RemoveDynamic(a.name)
	g.log.Info("agent draining; no new requests routed to it", "agent", a.name, "key", a.keyName,
		"open_streams", a.counters.active.Load())
}

func (g *Registry) detach(a *attachment) {
	a.end(reasonConnLost) // no-op for the reason if one was already set
	g.router.RemoveDynamic(a.name)
	g.mu.Lock()
	delete(g.atts, a.name)
	client := a.client
	g.mu.Unlock()
	if client != nil {
		client.Drain()
	}
	if a.tr != nil {
		a.tr.CloseIdleConnections()
	}
	g.log.Info("agent detached",
		"agent", a.name, "key", a.keyName, "remote", a.remote, "reason", *a.reason.Load(),
		"attached_for", time.Since(a.since).Round(time.Second).String(),
		"requests", a.counters.requests.Load(), "failures", a.counters.failures.Load(),
		"open_streams", a.counters.active.Load())
}

func modelSummary(ms []Model) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = m.Name
		if m.ContextLength > 0 {
			parts[i] += fmt.Sprintf("(ctx %d)", m.ContextLength)
		}
	}
	return strings.Join(parts, ",")
}

// countingTransport counts the requests the edge sends an agent, the
// ones that got no response, and the responses whose bodies are still
// open (live streams).
type countingTransport struct {
	rt http.RoundTripper
	c  *counters
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.c.requests.Add(1)
	resp, err := t.rt.RoundTrip(req)
	if err != nil {
		t.c.failures.Add(1)
		if errors.Is(err, errAgentGone) {
			// The connection ended, or refused new streams with GOAWAY,
			// before this request was written.
			return nil, backend.ErrGone
		}
		return nil, err
	}
	sanitizeHeaders(resp.Header)
	t.c.active.Add(1)
	resp.Body = &countedBody{ReadCloser: resp.Body, c: t.c}
	return resp, nil
}

// maxAgentRetryAfter caps the backoff an agent can ask clients for.
const maxAgentRetryAfter = 60

// sanitizeHeaders bounds the response headers the proxy relays from an
// agent, whose operator is not the edge's (PLAN §104.6): only the
// content types an inference API answers with, and a short numeric
// Retry-After.
func sanitizeHeaders(h http.Header) {
	if ct := h.Get("Content-Type"); ct != "" {
		mt, _, _ := strings.Cut(ct, ";")
		switch strings.ToLower(strings.TrimSpace(mt)) {
		case "application/json", "text/event-stream":
		default:
			h.Set("Content-Type", "application/octet-stream")
		}
	}
	if ra := h.Get("Retry-After"); ra != "" {
		n, err := strconv.Atoi(strings.TrimSpace(ra))
		switch {
		case err != nil || n < 0:
			h.Del("Retry-After")
		case n > maxAgentRetryAfter:
			h.Set("Retry-After", strconv.Itoa(maxAgentRetryAfter))
		}
	}
}

type countedBody struct {
	io.ReadCloser
	c    *counters
	once sync.Once
}

func (b *countedBody) Close() error {
	b.once.Do(func() { b.c.active.Add(-1) })
	return b.ReadCloser.Close()
}

// maxStreamReceiveBuffer bounds what the edge buffers for one stream
// whose client stopped reading. The transport's default (4 MiB) let 128
// stalled clients grow the edge by about 900 MB and delay healthy
// streams; 256 KiB holds a slow client to a few hundred SSE events while
// flow control pushes back on the agent for that stream alone. The
// connection window stays at its default (1 GiB), above max_concurrency
// times this, so stalled streams cannot block the others.
const maxStreamReceiveBuffer = 256 << 10

// newEdgeTransport speaks prior-knowledge HTTP/2 over the agent's
// connection, with keepalive pings so a vanished agent is detected.
// goAway is called (synchronously, from the read path; it must not
// block) when the agent starts a graceful detach, gone once
// the connection can take no new requests for any reason.
func newEdgeTransport(c *trackedConn, goAway, gone func()) *http.Transport {
	t := &http.Transport{
		DialContext:            dialOnce(&goAwayConn{trackedConn: c, onGoAway: goAway}, gone),
		DisableCompression:     true,
		MaxResponseHeaderBytes: 64 << 10,
		// The connection is the agent; it must never be reaped as idle.
		IdleConnTimeout: 0,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout:           pingInterval,
			PingTimeout:               pingTimeout,
			MaxReceiveBufferPerStream: maxStreamReceiveBuffer,
			// Keepalive pings only start once the connection reads
			// nothing, so an agent that keeps sending frames while it
			// stops reading would otherwise block every write, and the
			// requests queued behind it, for as long as it likes.
			WriteByteTimeout: pingInterval + pingTimeout,
		},
	}
	t.Protocols = new(http.Protocols)
	t.Protocols.SetUnencryptedHTTP2(true)
	return t
}

// advertised is one model an agent's server lists, with the public name
// a register entry assigns it ("" without a register list).
type advertised struct {
	discovery.Model
	public string
}

// filterRegister keeps the advertised models the attachment's key lists,
// in register order, each with its public name (PLAN §104.4). Unlisted
// models are dropped with one log line; a listed id the server does not
// advertise is logged and skipped. More matches than agents.max_models
// fail discovery, as an over-long list does without a register list.
func (g *Registry) filterRegister(a *attachment, models []advertised) ([]advertised, error) {
	byID := make(map[string]advertised, len(models))
	for _, m := range models {
		byID[m.ID] = m
	}
	var kept []advertised
	var missing []string
	for _, r := range a.key.Register {
		m, ok := byID[r.ID]
		if !ok {
			missing = append(missing, r.ID)
			continue
		}
		public, err := config.AgentRegisteredName(a.ns, r.ID, r.As)
		if err != nil {
			g.log.Warn("agent model ignored", "agent", a.name, "model", r.ID, "error", err)
			continue
		}
		m.public = public
		kept = append(kept, m)
	}
	if len(missing) > 0 {
		g.log.Warn("agent server does not advertise listed models", "agent", a.name, "key", a.keyName, "models", missing)
	}
	if n := len(models) - len(kept); n > 0 {
		g.log.Info("agent models not in the key's register list ignored", "agent", a.name, "key", a.keyName, "count", n)
	}
	if len(kept) > g.cfg.MaxModels {
		return nil, fmt.Errorf("%d registered models exceed agents.max_models", len(kept))
	}
	return kept, nil
}

// discover asks the agent which models its server advertises.
// It gives up when the connection closes, or after discoveryTimeout.
func discover(rt http.RoundTripper, maxModels int, closed <-chan struct{}) ([]advertised, error) {
	ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	defer cancel()
	go func() {
		select {
		case <-closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent.invalid/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/models: status %d", resp.StatusCode)
	}
	models, err := readModelList(resp.Body, maxModels)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, errors.New("GET /v1/models: no models")
	}
	out := make([]advertised, len(models))
	for i, m := range models {
		out[i] = advertised{Model: m}
	}
	return out, nil
}
