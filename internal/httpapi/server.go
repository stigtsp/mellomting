// Package httpapi is the ingress HTTP surface (PLAN §8, §9, §11, §14,
// §69, §72, §73): explicit allow-listed routes with no catch-all proxy
// (PLAN §11.3), bearer authentication against the key store, request
// IDs, global inflight admission, and sanitized OpenAI-shaped errors.
package httpapi

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"mellomting/internal/auth"
	"mellomting/internal/config"
	"mellomting/internal/limiter"
	"mellomting/internal/proxy"
	"mellomting/internal/routing"
)

// snapshot is the key store together with the limit registry built for
// it. They are one pointer because they must be read as one: a request
// that took its key record from one generation and its limit registry
// from another materializes the registry entry from the wrong record,
// and Registry.For caches that entry for the life of the generation. A
// SIGHUP that tightens a key's limits would then never apply to the key
// it was aimed at (PLAN §30, §34, §35).
type snapshot struct {
	store  *auth.Store
	limits *limiter.Registry
}

// Server owns the ingress routes and limits.
type Server struct {
	cfg         *config.Config
	log         *slog.Logger
	snap        atomic.Pointer[snapshot] // swapped atomically on SIGHUP reload (PLAN §30)
	router      *routing.Router
	proxy       *proxy.Proxy
	inflight    chan struct{}
	globalRPS   *limiter.Bucket
	sourceLimit *limiter.SourceRegistry
	authLog     *limiter.Bucket
	authDropped atomic.Int64
	// peers decides whose X-Forwarded-For the ingress believes
	// (PLAN §18.1). Empty by default: the socket peer is the client.
	peers       trustedPeers
	ready       atomic.Bool
	startedUnix int64
}

// New builds the HTTP surface. All dependencies are final at this point
// (configuration is immutable after startup, PLAN §30).
func New(cfg *config.Config, log *slog.Logger, store *auth.Store, router *routing.Router, p *proxy.Proxy) *Server {
	if log == nil {
		log = slog.Default()
	}
	size := max(cfg.Server.MaxInflightRequests, 1)
	// Configuration validation guarantees rate > 0 after defaults;
	// fall back to the PLAN defaults if an un-validated configuration
	// ever reaches the daemon (PLAN §76).
	rps, burst := cfg.Limits.GlobalRequestsPerSecond, cfg.Limits.GlobalBurst
	if rps <= 0 || burst < 1 {
		rps, burst = 100, 200
	}
	globalRPS, err := limiter.NewBucket(rps, burst)
	if err != nil {
		panic("httpapi: invalid global rate limit: " + err.Error())
	}
	// Pre-auth per-source flood protection (PLAN §33): always present,
	// bounded by the source registry. Defaults mirror the config
	// defaults if an un-validated configuration ever arrives.
	prps, pburst := cfg.Limits.PreauthRequestsPerSecond, cfg.Limits.PreauthBurst
	if prps <= 0 || pburst < 1 {
		prps, pburst = 100, 200
	}
	sourceLimit, err := limiter.NewSourceRegistry(prps, pburst, config.DefaultPreauthSources)
	if err != nil {
		panic("httpapi: invalid preauth rate limit: " + err.Error())
	}
	// Bounded invalid-auth logging (PLAN §33): at most
	// auth_failure_log_rate warn-lines per second, each carrying the
	// count of suppressed attempts. The burst is fixed small so output
	// is never a flood.
	alr := cfg.Limits.AuthFailureLogRate
	if alr <= 0 {
		alr = 1
	}
	authLog, err := limiter.NewBucket(alr, 3)
	if err != nil {
		panic("httpapi: invalid auth log rate: " + err.Error())
	}
	s := &Server{
		cfg:         cfg,
		log:         log,
		router:      router,
		proxy:       p,
		inflight:    make(chan struct{}, size),
		globalRPS:   globalRPS,
		sourceLimit: sourceLimit,
		authLog:     authLog,
		peers:       newTrustedPeers(cfg.Server.Listen.Network, cfg.Server.TrustedProxies),
		startedUnix: time.Now().Unix(),
	}
	s.snap.Store(&snapshot{store: store, limits: limiter.NewRegistry()})
	return s
}

// ReloadStore atomically swaps the key store and the per-key limit
// registry (SIGHUP reload, PLAN §30). In-flight requests keep serving
// against the store and limits they were admitted under, so a reload
// never severs an active stream (PLAN §74). Store and registry are
// published as one pointer, so no request can pair a key record with a
// registry from a different generation. Token buckets are carried over
// across the reload for keys whose rate and burst are unchanged, so a
// reload does not gift every key a fresh burst; changed limits rebuild
// the bucket from the reloaded store (FIX-22).
func (s *Server) ReloadStore(st *auth.Store) {
	s.snap.Store(&snapshot{
		store:  st,
		limits: limiter.NewRegistryCarrying(s.snap.Load().limits),
	})
}

// SetReady flips readiness (PLAN §69 /readyz).
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

// Handler returns the ingress handler with an explicit route table.
// There is intentionally no catch-all: every path not listed below is a
// 404 and is never forwarded to a backend (PLAN §11.3).
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.route)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	// The address this request counts as coming from (PLAN §18.1): the
	// socket peer, or the client a trusted reverse proxy forwarded.
	// Resolved once, so the source it is limited under and the source
	// in every log and accounting record agree.
	remote := s.peers.clientIP(r)

	// Defence-in-depth panic containment (PLAN §5.1 robustness
	// envelope): a panic on the handler goroutine must become a
	// sanitized 500, never a torn connection or a process kill. The
	// pump path additionally contains its own spawned goroutine, which
	// net/http's per-connection recover cannot reach.
	rw := &committedWriter{ResponseWriter: w}
	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("panic recovered in request handler", "remote", remote, "path", r.URL.Path, "panic", sanitizePanic(rec))
			if !rw.committed {
				writeErr(rw, http.StatusInternalServerError, "api_error", "internal", "internal error")
			}
		}
	}()
	routeBody(s, rw, r, remote)
}

// committedWriter tracks whether a response header has been written so a
// recovery path can avoid a superfluous second WriteHeader.
type committedWriter struct {
	http.ResponseWriter
	committed bool
}

func (c *committedWriter) WriteHeader(code int) {
	c.committed = true
	c.ResponseWriter.WriteHeader(code)
}

func (c *committedWriter) Write(b []byte) (int, error) {
	c.committed = true
	return c.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController (and net/http's internal probing)
// reach the underlying writer for Flush, SetWriteDeadline, and
// SetReadDeadline on streaming responses.
func (c *committedWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// Flush forwards flushing so SSE pumping and /healthz work through the
// wrapper (PLAN §24, §69). It is a no-op when the underlying writer does
// not support flushing, matching net/http's convention.
func (c *committedWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards connection hijacking for protocols that need the raw
// connection (compatibility with net/http's optional interface).
func (c *committedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := c.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// sanitizePanic reduces a recovered panic value to a short, loggable
// string with no stack trace or filesystem detail (PLAN §43, §72).
func sanitizePanic(rec any) string {
	switch v := rec.(type) {
	case error:
		return v.Error()
	case string:
		if len(v) > 200 {
			return v[:200]
		}
		return v
	default:
		return fmt.Sprintf("%v", rec)
	}
}

func routeBody(s *Server, w http.ResponseWriter, r *http.Request, remote string) {
	rid := newRequestID()
	w.Header().Set("X-Request-ID", rid)

	// Health endpoints: unauthenticated, exempt from inflight limits and
	// from inference routing (PLAN §69).
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case "/healthz":
			writeHealth(w, http.StatusOK, "ok")
			return
		case "/readyz":
			if s.ready.Load() {
				writeHealth(w, http.StatusOK, "ready")
			} else {
				writeHealth(w, http.StatusServiceUnavailable, "not ready")
			}
			return
		}
	}

	// Global inflight admission (PLAN §9.1): bounded, non-blocking.
	select {
	case s.inflight <- struct{}{}:
		defer func() { <-s.inflight }()
	default:
		writeErr(w, http.StatusServiceUnavailable, "overload_error", "server_overloaded", msgOverload)
		return
	}

	// Pre-auth per-source flood protection (PLAN §33): a per-source rate
	// bucket is consumed before the shared authenticated bucket and
	// before authentication, so a bogus-token flood from one host cannot
	// 429 legit keys on the global bucket, and invalid-auth attempts
	// never materialize per-key state.
	if ok, ra := s.sourceLimit.Allow(remote, time.Now()); !ok {
		writeRateLimit(w, ra)
		return
	}

	// Global request-rate limit (PLAN §32–§34), checked before paying
	// the authentication cost beyond the inflight bound above.
	if ok, ra := s.globalRPS.Allow(time.Now()); !ok {
		writeRateLimit(w, ra)
		return
	}

	// One load for the whole request: the key record and the limit
	// registry it is admitted against come from the same generation
	// even when a SIGHUP lands mid-request.
	snap := s.snap.Load()
	key, err := s.authorize(r, snap.store, remote)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", msgBadAuth)
		return
	}

	// Per-key limits (PLAN §34, §35): request rate, then concurrency.
	// Acquired before bodies are read (the proxy reads after this
	// point) so a single key cannot pile up resources (PLAN §35).
	ks := snap.limits.For(key)
	if ok, ra := ks.AllowRate(time.Now()); !ok {
		writeRateLimit(w, ra)
		return
	}
	releaseKey, ok := ks.AcquireConcurrency()
	if !ok {
		// No computable wait for a concurrency slot, so use the same
		// conservative default as the proxy quota 429 (PLAN §34, T-Q12:
		// every 429 carries Retry-After; FIX-10).
		writeRateLimit(w, time.Second)
		return
	}
	defer releaseKey()

	q := &proxy.Req{
		W:         w,
		R:         r,
		Key:       key,
		RequestID: rid,
		Remote:    remote,
	}

	// Explicit allow-listed inference routes (PLAN §11.1).
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		s.handleModels(w, key)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		s.proxy.ChatCompletions(q)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/v1/completions":
		s.proxy.Completions(q)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/v1/embeddings":
		s.proxy.Embeddings(q)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
		s.proxy.ResponsesCreate(q)
		return
	}

	// Responses retrieve/cancel (PLAN §11.1, §21): /v1/responses/{id},
	// /v1/responses/{id}/cancel. The /cancel suffix is POST-only, so a
	// GET on it falls through to the 405 below rather than retrieving
	// (T-Q10).
	if strings.HasPrefix(r.URL.Path, "/v1/responses/") {
		if id, cancel, ok := s.responsesID(r); ok {
			switch {
			case r.Method == http.MethodGet && !cancel:
				s.proxy.ResponsesRetrieve(q, id)
				return
			case r.Method == http.MethodPost && cancel:
				s.proxy.ResponsesCancel(q, id)
				return
			}
		}
	}

	// Known path, wrong method: 405 with Allow (explicit, not a proxy).
	if allow := s.allowFor(r.URL.Path); allow != "" {
		w.Header().Set("Allow", allow)
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", msgBadMethod)
		return
	}

	// Everything else: 404 (PLAN §11.3).
	writeErr(w, http.StatusNotFound, "invalid_request_error", "not_found", msgNotFound)
}

// allowFor lists the permitted methods for a known inference path; ""
// means the path is not allow-listed at all.
func (s *Server) allowFor(path string) string {
	switch {
	case path == "/v1/models":
		return "GET"
	case path == "/v1/responses":
		return "POST"
	case path == "/v1/chat/completions", path == "/v1/completions",
		path == "/v1/embeddings", path == "/healthz", path == "/readyz":
		if path == "/v1/chat/completions" || path == "/v1/completions" || path == "/v1/embeddings" {
			return "POST"
		}
		return "GET"
	case strings.HasPrefix(path, "/v1/responses/"):
		rest := strings.TrimPrefix(path, "/v1/responses/")
		cancel := strings.HasSuffix(rest, "/cancel")
		if cancel {
			rest = strings.TrimSuffix(rest, "/cancel")
		}
		// Only a single safe segment is a known path; anything with a
		// dot segment or extra separators is not allow-listed and 404s
		// (T-M2, PLAN §11.3).
		if !isSafeSegment(rest) {
			return ""
		}
		if cancel {
			return "POST"
		}
		return "GET"
	}
	return ""
}

// authorize extracts and validates the API key (PLAN §25):
//
//	Authorization: Bearer sk-...   (primary)
//	X-Api-Key: sk-...              (compatibility)
//
// Credentials in query parameters are never read. The store and the
// client address are passed in rather than resolved here so the
// caller's snapshot governs the whole request and the logged address
// agrees with the one the request was rate-limited under.
func (s *Server) authorize(r *http.Request, store *auth.Store, remote string) (*auth.Key, error) {
	var bearer string
	auths := r.Header.Values("Authorization")
	if len(auths) > 1 {
		return nil, errAuth
	}
	if len(auths) == 1 {
		scheme, _, ok := strings.Cut(auths[0], " ")
		if !ok {
			return nil, errAuth
		}
		if !strings.EqualFold(scheme, "bearer") {
			return nil, errAuth
		}
		bearer = strings.TrimSpace(auths[0][len(scheme)+1:])
		if bearer == "" {
			return nil, errAuth
		}
	}
	// Duplicate X-Api-Key is rejected like duplicate Authorization
	// (T-L1): a client sending the credential twice is ambiguous and
	// must not silently first-wins.
	apiKeys := r.Header.Values("X-Api-Key")
	if len(apiKeys) > 1 {
		return nil, errAuth
	}
	var apiKey string
	if len(apiKeys) == 1 {
		apiKey = apiKeys[0]
	}
	var rawKey string
	switch {
	case bearer != "" && apiKey == "":
		rawKey = bearer
	case bearer == "" && apiKey != "":
		rawKey = apiKey
	case bearer != "" && apiKey != "":
		if bearer != apiKey {
			return nil, errAuth
		}
		rawKey = bearer
	default:
		return nil, errAuth
	}
	rec, err := store.Lookup(rawKey)
	if err != nil {
		// All key failures read identically to the client (no oracle);
		// the cause is an operator concern, not a client one.
		class := classifyAuthError(err)
		// Bounded invalid-auth logging (PLAN §33): during a bogus-token
		// flood we do not log every invalid token. A single rate-limited
		// warn line is emitted, carrying the count of attempts suppressed
		// since the previous line.
		if ok, _ := s.authLog.Allow(time.Now()); ok {
			s.log.Warn("auth rejected", "class", class, "remote", remote, "suppressed", s.authDropped.Swap(0))
		} else {
			s.authDropped.Add(1)
		}
		return nil, errAuth
	}
	return rec, nil
}

// classifyAuthError maps a key-store lookup failure to an operator-facing
// class (T-Q5). errors.Is is used (not ==) so a wrapped sentinel — e.g. a
// store that annotates ErrDisabled with context — still classifies
// correctly instead of silently degrading to auth_unknown.
func classifyAuthError(err error) string {
	switch {
	case errors.Is(err, auth.ErrDisabled):
		return "auth_disabled"
	case errors.Is(err, auth.ErrExpired):
		return "auth_expired"
	}
	return "auth_unknown"
}

// responsesID extracts a safe {id} from a /v1/responses/... path and
// whether the path targeted the POST-only /cancel suffix (T-Q10): the
// suffix must govern routing so a GET on it 405s instead of retrieving.
func (s *Server) responsesID(r *http.Request) (id string, cancel, ok bool) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/responses/")
	if before, ok0 := strings.CutSuffix(rest, "/cancel"); ok0 {
		rest = before
		cancel = true
	}
	if !isSafeSegment(rest) {
		return "", false, false
	}
	return rest, cancel, true
}

// isSafeSegment restricts one path segment to the same alphabet the
// proxy enforces on response IDs. Dot segments are rejected outright
// (T-M2): `.` and `..` are path-normalizing, and the backend URL is built
// with url.URL{Path: …}, which does not clean dot segments, so they must
// never be allowed to reach the allow-listed outbound path verbatim.
func isSafeSegment(seg string) bool {
	if len(seg) == 0 || len(seg) > 256 {
		return false
	}
	for _, r := range seg {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// newRequestID generates an internal-only request ID (PLAN §73).
// Client-supplied request IDs are not accepted in v1, which removes any
// chance of them influencing security decisions.
func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to a fixed-but-distinct value; never fail the
		// request because a log field cannot be named.
		return "req_0000000000000000"
	}
	return "req_" + hex.EncodeToString(b[:])
}

// peerString is the logging-friendly peer address (PLAN §18.1: in v1 the
// socket peer is the only trustworthy source; X-Forwarded-* is never
// consumed on the ingress side).
func peerString(r *http.Request) string {
	// A Unix-socket peer has no address of its own. Naming the
	// transport says that, where "unknown" reads like something went
	// wrong; all such peers share one pre-auth rate bucket either way,
	// which is right, because they are genuinely indistinguishable.
	if r.RemoteAddr == "" || r.RemoteAddr == "@" {
		return "unix"
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// errAuth is the route-layer sentinel for any authentication failure;
// the client always receives the same sanitized 401.
var errAuth = errors.New("unauthorized")
