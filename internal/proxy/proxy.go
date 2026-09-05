// Package proxy implements the authenticated inference pipeline
// (PLAN §10, §12, §13, §19, §21, §24, §31, §43): shallow-parsed request
// bodies, model rewriting, bounded buffering, backend admission, SSE
// streaming with idleness bounds, Responses API affinity, and sanitized
// client-facing errors.
//
// Nothing the client sent is trusted beyond the allow-listed fields
// (PLAN §12); nothing the backend returns is forwarded verbatim into an
// error (PLAN §43); prompts and responses are never logged (PLAN §24,
// §43).
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"mellomting/internal/accounting"
	"mellomting/internal/apierr"
	"mellomting/internal/auth"
	"mellomting/internal/backend"
	"mellomting/internal/config"
	"mellomting/internal/routing"
)

// Req is one authenticated request as seen by the proxy.
type Req struct {
	W         http.ResponseWriter
	R         *http.Request
	Key       *auth.Key
	RequestID string
	Remote    string
	// ResponseID is set for /v1/responses/{id} operations.
	ResponseID string
}

// operation describes one allow-listed backend operation (PLAN §11.1).
// routeKind says where an operation's target comes from. The two are
// exhaustive and mutually exclusive, so one field states what two
// complementary booleans used to.
type routeKind int

const (
	// routeByModel resolves the backend from the body's "model" field.
	routeByModel routeKind = iota
	// routeByResponseID resolves it from the response ID in the path,
	// through the affinity table (retrieve/cancel).
	routeByResponseID
)

type operation struct {
	path     string // backend path
	method   string
	route    routeKind
	capture  bool // record created response IDs (responses create)
	endpoint string
	// capField and altCapField name the generative output-limit request
	// fields (PLAN §36). An operation is generative exactly when
	// capField is set, so that is the single spelling of the predicate:
	// a separate flag could contradict it.
	capField    string
	altCapField string
}

// generative reports whether the operation streams output tokens and so
// carries an output cap (PLAN §36).
func (o operation) generative() bool { return o.capField != "" }

var (
	opChat       = operation{path: "/v1/chat/completions", method: "POST", route: routeByModel, endpoint: "chat.completions", capField: "max_completion_tokens", altCapField: "max_tokens"}
	opLegacy     = operation{path: "/v1/completions", method: "POST", route: routeByModel, endpoint: "completions", capField: "max_tokens"}
	opEmbed      = operation{path: "/v1/embeddings", method: "POST", route: routeByModel, endpoint: "embeddings"}
	opResp       = operation{path: "/v1/responses", method: "POST", route: routeByModel, capture: true, endpoint: "responses", capField: "max_output_tokens"}
	opRespGet    = operation{path: "/v1/responses", method: "GET", route: routeByResponseID, endpoint: "responses"}
	opRespCancel = operation{path: "/v1/responses", method: "POST", route: routeByResponseID, endpoint: "responses"}
)

// Proxy wires configuration, routing, and backend clients together.
type Proxy struct {
	router      *routing.Router
	clients     map[string]*backend.Client
	cfg         *config.Config
	log         *slog.Logger
	affinity    *affinity
	budget      *budget
	quota       *accounting.Quota  // token windows (PLAN §39); nil = disabled
	acc         *accounting.Writer // JSONL writer (PLAN §42); nil = disabled
	ensureUsage bool               // inject stream_options.include_usage (§38)
	draining    atomic.Bool
}

// New builds a Proxy. clients is the backend-name -> client table built
// from the same configuration. quota and acc enable token-usage quota and
// JSONL accounting respectively; passing nil for both disables accounting.
func New(cfg *config.Config, router *routing.Router, clients map[string]*backend.Client, log *slog.Logger, quota *accounting.Quota, acc *accounting.Writer, quotaConfigured bool) (*Proxy, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg == nil || router == nil || len(clients) == 0 {
		return nil, fmt.Errorf("proxy: configuration, router, and clients are required")
	}
	total := int64(cfg.Server.MaxBufferedRequestBytes) * int64(cfg.Server.MaxInflightRequests)
	if total <= 0 {
		total = 1 << 20
	}
	return &Proxy{
		router:   router,
		clients:  clients,
		cfg:      cfg,
		log:      log,
		affinity: newAffinity(cfg.Responses.AffinityTTL.Duration(), cfg.Responses.MaxAffinityEntries),
		budget:   newBudget(total),
		quota:    quota,
		acc:      acc,
		// FIX-04/N10: stream-usage injection is driven by whether a token
		// quota is in effect, not just by accounting.enabled. With
		// accounting disabled but quotas active, without injection every
		// stream settles the whole output cap against the quota. A nil
		// ensure_stream_usage (never configured) defaults to enabled when
		// a quota is in effect, so a quota-only deployment does not have
		// to set it explicitly; the serve-time fail-closed check still
		// keys off the explicit (non-nil) value.
		ensureUsage: (cfg.Accounting.EnsureStreamUsage == nil || *cfg.Accounting.EnsureStreamUsage) &&
			(cfg.Accounting.Enabled || quotaConfigured),
	}, nil
}

// BeginDraining makes the proxy reject new inferences during graceful
// shutdown (PLAN §74 step 2).
func (p *Proxy) BeginDraining() { p.draining.Store(true) }

// Draining reports whether new inferences are rejected.
func (p *Proxy) Draining() bool { return p.draining.Load() }

// ChatCompletions implements POST /v1/chat/completions (PLAN §11.1).
func (p *Proxy) ChatCompletions(q *Req) { p.dispatch(q, opChat) }

// Completions implements POST /v1/completions (PLAN §11.1).
func (p *Proxy) Completions(q *Req) { p.dispatch(q, opLegacy) }

// Embeddings implements POST /v1/embeddings (PLAN §11.1).
func (p *Proxy) Embeddings(q *Req) { p.dispatch(q, opEmbed) }

// ResponsesCreate implements POST /v1/responses (PLAN §11.1, §21).
func (p *Proxy) ResponsesCreate(q *Req) { p.dispatch(q, opResp) }

// ResponsesRetrieve implements GET /v1/responses/{id} (PLAN §11.1, §21).
func (p *Proxy) ResponsesRetrieve(q *Req, id string) { q.ResponseID = id; p.dispatch(q, opRespGet) }

// ResponsesCancel implements POST /v1/responses/{id}/cancel (PLAN §11.1, §21).
func (p *Proxy) ResponsesCancel(q *Req, id string) { q.ResponseID = id; p.dispatch(q, opRespCancel) }

// result is the structured outcome logged per request (PLAN §43).
type result struct {
	status   int
	class    string // sanitized error class; "ok" on success
	model    string
	backend  string
	bytesIn  int
	bytesOut int
	retries  int

	// Accounting inputs. Carrying them here rather than in loop locals
	// is what lets the deferred call be the only accounting call site,
	// so every one of dispatch's exit paths settles exactly once by
	// construction instead of by remembering to.
	usage       accounting.Usage
	usageStatus accounting.UsageStatus
	reservation int64
}

// dispatch runs the full pipeline for one allow-listed operation.
func (p *Proxy) dispatch(q *Req, o operation) {
	out := result{status: 500, class: "internal_error", bytesIn: -1, usageStatus: accounting.UsageUnknown}
	start := time.Now()
	defer func() {
		if out.status == 0 {
			out.status = 500
			out.class = "internal_error"
		}
		p.log.Info("request",
			"request_id", q.RequestID,
			"key_id", q.Key.ID,
			"remote", q.Remote,
			"endpoint", o.method+" "+o.path,
			"public_model", logModel(out.model),
			"backend", out.backend,
			"status", out.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"bytes_in", out.bytesIn,
			"bytes_out", out.bytesOut,
			"retry_count", out.retries,
			"error_class", out.class,
		)
		// Every request is accounted exactly once, here (PLAN §41).
		// Rejections (draining 503, 415/400/404 validation, 429 quota,
		// the upstream-error return) produce a zero-usage record so
		// rejection patterns stay queryable in usage.jsonl (FIX-26);
		// their non-2xx status means account settles nothing, so a
		// reject never touches quota.
		p.account(q, o, start, out)
	}()

	fail := func(e apiError) {
		out.status, out.class = e.status, e.class
		// Every proxy 429 carries Retry-After (T-Q12). A caller that
		// already set one — the terminal path forwarding the upstream's
		// value — keeps it; otherwise the conservative default applies.
		if e.status == http.StatusTooManyRequests && q.W.Header().Get("Retry-After") == "" {
			q.W.Header().Set("Retry-After", apierr.DefaultRetryAfter)
		}
		apierr.Write(q.W, e.status, e.typ, e.code, e.msg)
	}

	// 1. Draining admission (PLAN §74.2).
	if p.Draining() {
		fail(errDraining)
		return
	}

	// 2. Request body (bounded; PLAN §12.1). Authentication already
	// happened before body acquisition (PLAN §10). The byte budget must
	// stay reserved for as long as the body is in memory, so the release
	// is deferred here rather than inside readBody.
	var stream bool
	var publicModel string
	body, release, bodyErr := p.readBody(q, o)
	defer release()
	if bodyErr != nil {
		fail(*bodyErr)
		return
	}
	if o.method == "POST" {
		out.bytesIn = len(body)
	}

	// 3. Resolve where the request goes and under which policy
	// (PLAN §12, §13, §31), then 3.5 apply the output cap, stream-usage
	// injection, and quota admission (PLAN §36, §38, §39).
	t, targetErr := p.resolveTarget(q, o, body)
	if targetErr != nil {
		out.model = t.model
		fail(*targetErr)
		return
	}
	body, publicModel, stream = t.body, t.model, t.stream
	fixedBackend, outPath := t.backend, t.path
	out.model = publicModel

	ob, prepErr := p.prepare(q, o, t)
	if prepErr != nil {
		fail(*prepErr)
		return
	}
	reservation, injectedUsage := ob.reservation, ob.injectedUsage

	// 4. Forward under the bounded pre-stream retry/fallback budget
	// (PLAN §22, §23, §93). The request may make at most retry.max_attempts
	// attempts in total across all backends (a global bound: no
	// multiplicative amplification). A fallback to an untried eligible
	// backend is immediate; repeating a backend that already failed
	// waits the jittered exponential backoff. Nothing is retried once
	// any byte has reached the client (PLAN §24).
	maxAttempts := max(p.cfg.Retry.MaxAttempts, 1)
	uctx, ucancel := context.WithCancel(q.R.Context())
	defer ucancel()

	headers := passthroughHeaders(q.R)
	if ob.fields != nil || len(ob.body) > 0 {
		headers["Content-Type"] = []string{"application/json"}
	}

	tried := make([]string, 0, maxAttempts)

	var lastErr error
	var lastFailed string
	var lastRetryAfter string // upstream Retry-After on the final result (T-Q12)
	// markClientCanceled flags the request outcome as the client going
	// away before any response byte was sent (PLAN §43). No response is
	// possible, so the log uses 499 (client closed request) rather than
	// implying a server-side failure.
	markClientCanceled := func() {
		out.status = 499
		out.class = "client_canceled"
		out.bytesOut = 0
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if q.R.Context().Err() != nil {
			markClientCanceled()
			return
		}

		// Selection (PLAN §19, §22): prefer an untried eligible
		// backend; when every candidate is unavailable (cooldown or
		// excluded) a retryable last failure may be repeated in place.
		backendName := fixedBackend
		if backendName == "" {
			t, serr := p.router.Select(publicModel, tried)
			if serr != nil {
				if lastErr != nil && classifyBackendError(lastErr).retryable && lastFailed != "" {
					backendName = lastFailed
				} else if attempt == 0 {
					fail(errNoBackend)
					return
				} else {
					break // budget exhausted below
				}
			} else {
				backendName = t.Backend
			}
		}
		// The single encode of the outbound body, once per attempt: the
		// upstream model differs across replicas, so it cannot be hoisted
		// out of the loop. A response-ID route has no decoded body and is
		// relayed byte-for-byte.
		bd := ob.body
		if ob.fields != nil {
			up, _ := p.router.UpstreamFor(backendName)
			var rerr error
			if bd, rerr = encodeOutbound(ob.fields, up); rerr != nil {
				fail(errNotNormal)
				return
			}
		}

		// Same-backend repeat: exponential backoff with optional full
		// jitter (PLAN §23). Fallback to a different backend waits on
		// nothing.
		if attempt > 0 && backendName == lastFailed {
			out.retries++
			if !p.sleepBackoff(q.R.Context(), attempt) {
				markClientCanceled()
				return
			}
		}

		client, ok := p.clients[backendName]
		if !ok {
			fail(errInternal)
			return
		}
		out.backend = backendName

		res, err := client.Forward(uctx, backend.Request{
			Method:  o.method,
			Path:    outPath,
			Body:    bd,
			Headers: headers,
			Stream:  stream,
		})
		if err != nil {
			if errors.Is(err, context.Canceled) && q.R.Context().Err() != nil {
				// The client went away; no response is sent or possible.
				markClientCanceled()
				return
			}
			lastErr = err
			lastFailed = backendName
			// Remember any upstream-provided Retry-After so a terminal
			// 429 can forward it (T-Q12). The 429 branch only fires
			// when lastErr is that 429, whose result is this res.
			if res != nil {
				if ra := res.Header.Get("Retry-After"); ra != "" {
					lastRetryAfter = ra
				}
			}
			if f := classifyBackendError(err); f.retryable {
				// Connection-level failures poison the passive-health
				// state for subsequent requests (PLAN §70).
				if f.poisonsHealth {
					p.router.RecordFailure(backendName)
				}
				tried = append(tried, backendName)
				continue
			}
			break
		}
		// A response was received: the backend is up again.
		p.router.RecordSuccess(backendName)

		// 5/6. Success (PLAN §18: only allow-listed headers pass through).
		// Branch on whether a LIVE stream body exists, not on the
		// client's stream flag: a stream request answered with a 2xx
		// other than 200 is buffered (res.Body is nil) and is relayed
		// as a normal response instead of being read as a stream (X4:
		// reading a nil body from a spawned goroutine killed the
		// process).
		if res.Body != nil {
			var usage accounting.Usage
			status, bytesOut, cls := p.pump(q, res, o, ucancel, backendName, publicModel, &usage, injectedUsage)
			out.status, out.bytesOut, out.class = status, bytesOut, cls

			out.usage, out.usageStatus = usage, accounting.StatusOf(usage)
			out.reservation = reservation
			return
		}
		res.Close()
		out.status = res.Status
		out.class = "ok"
		out.bytesOut = len(res.BodyBytes)

		usage := accounting.ParseUsage(res.BodyBytes, o.endpoint)
		out.usage, out.usageStatus = usage, accounting.StatusOf(usage)
		out.reservation = reservation
		ct := "application/json"
		if v := res.Header.Get("Content-Type"); v != "" {
			ct = v
		}
		q.W.Header().Set("Content-Type", ct)
		q.W.WriteHeader(res.Status)
		p.writeBuffered(q.W, res.BodyBytes)
		if o.capture {
			if id := topLevelID(res.BodyBytes); isValidResponseID(id) {
				p.affinity.Put(q.Key.ID, id, backendName, publicModel)
			}
		}
		return
	}

	// Budget exhausted or a non-retryable failure: emit the sanitized
	// class of the last error (PLAN §43, §72). Exactly one error object
	// is emitted per terminal error (X7), which one classification and
	// one fail now make structural.
	if lastErr != nil {
		f := classifyBackendError(lastErr)
		if f.resp == errUpstreamRateLimited && lastRetryAfter != "" {
			// Forward the upstream's own Retry-After; fail supplies the
			// conservative default when it sent none (T-Q12).
			q.W.Header().Set("Retry-After", lastRetryAfter)
		}
		fail(f.resp)
	}
}

// writeBuffered relays a buffered response body under a client
// write-idle bound (X9) — the non-streaming twin of pump.
//
// A client that reads the headers then stops reading must not hold the
// inflight slot forever. stream_write_timeout is the same bound the
// streaming pump applies per event (PLAN §9.1); where unusable (e.g.
// HTTP/2, unsupported here in v1) the client context remains the
// disconnect signal. The deadline is reset around each chunk
// (FIX-19/N5), so a large response is bounded by idleness rather than
// total time: a slow-but-steady client is never cut off for reading too
// long, while a fully stalled one still is.
func (p *Proxy) writeBuffered(w http.ResponseWriter, body []byte) {
	const writeChunk = 32 << 10
	ctrl := http.NewResponseController(w)
	clientIdle := p.cfg.Server.StreamWriteTimeout.Duration()
	deadlineOK := ctrl.SetWriteDeadline(time.Now().Add(clientIdle)) == nil

	var werr error
	for off := 0; off < len(body) && werr == nil; off += writeChunk {
		if deadlineOK {
			deadlineOK = ctrl.SetWriteDeadline(time.Now().Add(clientIdle)) == nil
		}
		_, werr = w.Write(body[off:min(off+writeChunk, len(body))])
	}
	// Clear the deadline only when the write completed, so it cannot leak
	// into the next keep-alive request on this connection. When the write
	// errored (stalled reader, deadline fired) the deadline is left
	// expired: net/http's finishRequest flush then fails fast and the
	// connection is closed instead of blocking forever on a full socket
	// buffer, which would leak a goroutine and fd per stalled client (X9).
	if deadlineOK && werr == nil {
		_ = ctrl.SetWriteDeadline(time.Time{})
	}
}

// target is what phase 3 resolves: which model's policy governs the
// request, and where it goes.
type target struct {
	model   string // public model name; empty for response-ID routes
	stream  bool
	backend string // pinned backend; empty lets the router choose
	path    string // backend path for this request
	// fields is the body decoded once, shared by every later stage and
	// re-encoded exactly once per attempt. It is nil for response-ID
	// routes, whose body is relayed byte-for-byte.
	fields map[string]json.RawMessage
	body   []byte // raw body, relayed when fields is nil
}

// resolveTarget performs the shallow parse, the ACL and model-type
// checks, and the affinity lookup (PLAN §12, §13, §21, §31). It returns
// a sanitized error for the caller to emit; it writes nothing itself.
//
// On failure the returned target still carries whatever model was
// parsed, so the request log and usage record can name it.
func (p *Proxy) resolveTarget(q *Req, o operation, body []byte) (target, *apiError) {
	t := target{body: body, path: o.path}

	if o.route == routeByModel {
		fields, publicModel, stream, err := shallowParse(body)
		t.fields, t.model, t.stream = fields, publicModel, stream
		switch {
		case errors.Is(err, errJSON):
			return t, &errBadJSON
		case errors.Is(err, errMissingModel):
			return t, &errNoModel
		case errors.Is(err, errModelNotString):
			return t, &errModelType
		}
		// OpenAI never streams non-generative endpoints (FIX-03/N3):
		// accepting "stream": true here would otherwise let a misrouted
		// SSE response charge zero tokens. Fail closed with a 4xx before
		// any accounting or forwarding happens.
		if stream && !o.generative() {
			return t, &errNoStream
		}
		// Do not reveal whether the model exists (PLAN §31): unknown and
		// not-allowed read identically.
		if !q.Key.Allows(publicModel) || !p.router.Has(publicModel) {
			return t, &errModelDenied
		}
		// Endpoint/model-type agreement (N12): an embedding model is not
		// servable on a generative endpoint, and a generation model is
		// not servable on the embeddings endpoint. The model is known to
		// exist here, so this is a 400, not a 404.
		switch p.router.TypeOf(publicModel) {
		case "embedding":
			if o.generative() {
				return t, &errNotGenerate
			}
		case "generation":
			if o.endpoint == "embeddings" {
				return t, &errNotEmbed
			}
		}
		if o.capture {
			if prev, ok := stringField(t.fields, "previous_response_id"); ok {
				b, m, ok := p.affinity.Get(q.Key.ID, prev)
				// The owning backend is authoritative (PLAN §21.3) and
				// pinning it skips routing, so the continuation must
				// name the model the ACL and output cap were checked
				// against. A mismatch reads as a miss (PLAN §31).
				if !ok || m != publicModel {
					return t, &errPrevNotFound
				}
				t.backend = b
			}
		}
		return t, nil
	}

	// Responses retrieve/cancel (PLAN §21.1, §21.3): route only to the
	// backend that owns the response for THIS key. A different key must
	// not be able to retrieve or cancel another key's response even if
	// it knows the ID, so on an affinity miss we fail closed rather than
	// forward on backend reachability. Entries outlive a reload, so the
	// ACL is re-checked against the model the response was created under.
	b, m, ok := p.affinity.Get(q.Key.ID, q.ResponseID)
	if !ok || !q.Key.Allows(m) {
		return t, &errRespNotFound
	}
	t.backend = b

	// The ID is restricted to a safe charset here as defense in depth
	// (the route layer also validates it) so it can never inject a path
	// or host into the backend URL. The path is built here rather than
	// by mutating the operation, which is an immutable table entry the
	// request log reads as the route's identity.
	if !isValidResponseID(q.ResponseID) {
		return t, &errRespBadID
	}
	t.path = "/v1/responses/" + q.ResponseID
	if o.method == http.MethodPost {
		t.path += "/cancel"
	}
	return t, nil
}

// outbound is the body as it will be sent, plus what accounting needs to
// know about it.
type outbound struct {
	fields        map[string]json.RawMessage // nil for response-ID routes
	body          []byte                     // raw body, relayed when fields is nil
	reservation   int64
	injectedUsage bool
}

// prepare applies the generative output cap (PLAN §36) and stream-usage
// injection (PLAN §38), then admits the request against the token quota
// (PLAN §39). These are client-facing and deterministic, so they run once
// before the retry budget; the model field is rewritten per attempt
// later.
func (p *Proxy) prepare(q *Req, o operation, t target) (outbound, *apiError) {
	ob := outbound{body: t.body}
	if o.route != routeByModel {
		return ob, nil
	}

	cap := 0
	if m, ok := p.cfg.Models[t.model]; ok {
		cap = m.Policy.MaxOutputTokens
	}
	reservation, injected, err := prepareOutbound(
		t.fields, o, cap, t.stream, p.ensureUsage, p.cfg.Accounting.UnknownUsageReservation)
	switch {
	case errors.Is(err, errCapExceeded):
		return ob, &errOutputCapExceeded
	case errors.Is(err, errCapInvalid):
		return ob, &errOutputLimitInvalid
	case err != nil:
		return ob, &errNotNormal
	}
	ob = outbound{fields: t.fields, reservation: reservation, injectedUsage: injected}

	if p.quota != nil {
		if ok, _ := p.quota.Admit(q.Key.ID, windowLimits(q.Key.Limits), reservation, time.Now()); !ok {
			return ob, &errQuota
		}
	}
	return ob, nil
}

// readBody acquires the byte budget and reads the bounded request body
// (T-X12, PLAN §12.1).
//
// The budget is reserved BEFORE the body is read and decoded: a body
// whose known size cannot fit is rejected with a clean 503 before a
// single byte is allocated, and the reservation covers the shallow-parse
// and rewrite decode/re-encode peak, so the aggregate bound
// (MaxBufferedRequestBytes × MaxInflightRequests) actually holds. An
// unknown-size (chunked) body reserves the worst case up front and
// releases the unused remainder after the read, so the concurrent budget
// is never over-subscribed while a large body is being read (FIX-15).
//
// The returned release must be deferred by the caller for the lifetime
// of the request; it is never nil.
func (p *Proxy) readBody(q *Req, o operation) ([]byte, func(), *apiError) {
	var reserve int64
	release := func() {
		if reserve > 0 {
			p.budget.Release(reserve)
			reserve = 0
		}
	}
	if o.method != "POST" {
		return nil, release, nil
	}

	enc := q.R.Header.Get("Content-Encoding")
	if enc != "" && !strings.EqualFold(enc, "identity") {
		return nil, release, &errUnsupported
	}

	maxBody := int64(p.cfg.Server.MaxBodyBytes)
	if cl := q.R.ContentLength; cl > 0 && cl <= maxBody {
		if !p.budget.Acquire(cl) {
			return nil, release, &errOverloaded
		}
		reserve = cl
	} else if cl < 0 {
		if !p.budget.Acquire(maxBody) {
			return nil, release, &errOverloaded
		}
		reserve = maxBody
	}

	body, err := readBodyLimited(q.R, p.cfg.Server.MaxBodyBytes)
	if errors.Is(err, errBodyTooLarge) {
		return nil, release, &errBodyTooBig
	}
	if err != nil {
		return nil, release, &errBodyUnread
	}
	if n := int64(len(body)); n < reserve {
		// Body smaller than the reservation (an unknown-size request
		// reserved the worst case): release the excess now.
		p.budget.Release(reserve - n)
		reserve = n
	}
	return body, release, nil
}

// sleepBackoff sleeps the retry backoff for attempt n (1-based retries)
// with full jitter when enabled, bounded by retry.max_backoff
// (PLAN §23). It returns false if the client context ended while
// waiting.
func (p *Proxy) sleepBackoff(ctx context.Context, attempt int) bool {
	d := p.cfg.Retry.InitialBackoff.Duration()
	max := p.cfg.Retry.MaxBackoff.Duration()
	for i := 1; i < attempt && d < max; i++ {
		d *= 2
		if d > max {
			d = max
		}
	}
	if d > max {
		d = max
	}
	if p.cfg.Retry.JitterEnabled() && d > 0 {
		d = time.Duration(rand.Int64N(int64(d)))
	}
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pump relays an upstream SSE stream to the client (PLAN §24): event
// order is preserved, each event is emitted promptly as it is parsed
// (no whole-response buffering), upstream read-idle is bounded by
// stream_idle_timeout, client write-idle is bounded by
// stream_write_timeout, and any terminal path cancels the upstream
// context.
func (p *Proxy) pump(q *Req, res *backend.Result, o operation, ucancel context.CancelFunc, backendName, publicModel string, usage *accounting.Usage, injectedUsage bool) (int, int, string) {
	defer res.Close()
	defer ucancel() // tear down the upstream on every exit path.
	flusher, _ := q.W.(http.Flusher)
	w := q.W
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	q.W.WriteHeader(200)

	// Client write-idle deadline (PLAN §9.1, §24). On HTTP/1 the
	// underlying conn honours it; on HTTP/2 (unsupported here for the
	// ingress listener in v1) SetWriteDeadline reports unusable and the
	// client context remains the disconnect signal.
	ctrl := http.NewResponseController(w)
	// Upstream read-idle bound (PLAN §24, §15). The per-backend
	// stream_idle_timeout governs this backend's streams (T-M3); the
	// server-level value remains the fallback for unknown backends, and
	// the client write-idle below falls back to it too.
	idle := p.cfg.Server.StreamIdleTimeout.Duration()
	if c, ok := p.clients[backendName]; ok {
		if bIdle := c.StreamIdleTimeout(); bIdle > 0 {
			idle = bIdle
		}
	}
	clientIdle := p.cfg.Server.StreamWriteTimeout.Duration()
	if clientIdle <= 0 {
		clientIdle = idle
	}
	writeDeadlineUsable := true

	parser := newSSEParser(res.Body)
	var bytesOut int
	captured := false
	for {
		if err := q.R.Context().Err(); err != nil {
			return 200, bytesOut, "client_canceled"
		}

		// Read the next event, enforcing upstream stream idle
		// (PLAN §24). One reader goroutine is alive at a time; it
		// unwinds when the upstream is cancelled below.
		type evResult struct {
			out []byte
			err error
		}
		ch := make(chan evResult, 1)
		go func() {
			// Panic containment (PLAN §5.1 robustness envelope): a
			// misbehaving backend body must never take the process
			// down. This goroutine is spawned by the handler, so
			// net/http's per-connection recover cannot reach it; a
			// panic here is converted into a stream error instead.
			defer func() {
				if rec := recover(); rec != nil {
					ch <- evResult{err: errStreamPanic}
				}
			}()
			out, err := parser.nextEvent()
			ch <- evResult{out: out, err: err}
		}()
		var (
			ev    []byte
			rerr  error
			abort bool
		)
		readTimer := time.NewTimer(idle)
		select {
		case r := <-ch:
			ev, rerr = r.out, r.err
		case <-readTimer.C:
			abort = true
		}
		readTimer.Stop()
		if abort {
			// Headers are already committed; the client sees a
			// truncated stream. The class carries the cause.
			return 200, bytesOut, "backend_stream_error"
		}
		if rerr == io.EOF {
			return 200, bytesOut, "ok"
		}
		if rerr != nil {
			// Upstream was cancelled because the client went away.
			if errors.Is(rerr, context.Canceled) && q.R.Context().Err() != nil {
				return 200, bytesOut, "client_canceled"
			}
			return 200, bytesOut, "backend_stream_error"
		}
		if len(ev) == 0 {
			continue
		}

		// Capture response IDs for affinity (PLAN §21.2). A backend
		// may return an unbounded or malformed id; only a bounded,
		// safe ID can ever be retrieved/cancelled by a client, so
		// anything else is skipped rather than stored whole (T-L5).
		if o.capture && !captured {
			if data, ok := dataField(ev); ok {
				if id := responseIDFromData(data); isValidResponseID(id) {
					p.affinity.Put(q.Key.ID, id, backendName, publicModel)
					captured = true
				}
			}
		}

		// Capture token usage (PLAN §37-38) and swallow the synthetic
		// final usage-only chunk when it was injected on the client's
		// behalf (PLAN §38) so the client sees no semantic change.
		if data, ok := dataField(ev); ok {
			if u := accounting.ParseStreamChunk(data); u.Present {
				*usage = u
			}
			if injectedUsage && isUsageOnlyChunk(data) {
				// Record the usage above; do not relay the chunk.
				continue
			}
		}

		// Relay the event verbatim (PLAN §24), bounding client
		// write-idle via the conn write deadline where usable.
		if writeDeadlineUsable {
			if err := ctrl.SetWriteDeadline(time.Now().Add(clientIdle)); err != nil {
				writeDeadlineUsable = false
			}
		}
		// Cumulative emitted-byte bound (PLAN §4, FIX-12): the buffered
		// path is bounded by server.max_response_bytes, and a live SSE
		// stream is bounded by the same cap, so a backend emitting small
		// events forever cannot stream unbounded data. On breach the
		// stream is terminated with backend_stream_error (headers are
		// already committed; the class carries the cause).
		if budget := p.cfg.Server.MaxResponseBytes; budget > 0 && bytesOut+len(ev) > budget {
			return 200, bytesOut, "backend_stream_error"
		}
		if _, werr := w.Write(ev); werr != nil {
			return 200, bytesOut, "client_write_error"
		}
		bytesOut += len(ev)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// shallowParse inspects the routing/policy fields of a JSON body and
// returns a normalized body with unknown fields preserved verbatim
// (PLAN §12).
func shallowParse(body []byte) (fields map[string]json.RawMessage, model string, stream bool, err error) {
	if len(body) == 0 {
		return nil, "", false, errMissingModel
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, "", false, errJSON
	}
	// A JSON `null` unmarshals into a nil map without error; every later
	// stage writes to this map, and writing to a nil map panics (T-L6).
	if fields == nil {
		return nil, "", false, errJSON
	}
	rawModel, ok := fields["model"]
	if !ok {
		return nil, "", false, errMissingModel
	}
	if err := json.Unmarshal(rawModel, &model); err != nil || model == "" {
		return nil, "", false, errModelNotString
	}
	if rawStream, ok := fields["stream"]; ok {
		var b bool
		if err := json.Unmarshal(rawStream, &b); err == nil {
			stream = b
		}
	}
	return fields, model, stream, nil
}

// rewriteModel sets the outbound model field and re-serializes, keeping
// every other field byte-identical via json.RawMessage (PLAN §12, §13).
// encodeOutbound sets the upstream model on the decoded body and
// re-encodes it. This is the single canonicalizing marshal of the
// request: a duplicate "model" key decodes last-wins and re-encodes to
// one key, so a backend can never resolve a second one the ACL never
// saw. Forwarding the client's bytes verbatim would reopen that.
func encodeOutbound(fields map[string]json.RawMessage, upstream string) ([]byte, error) {
	if upstream != "" {
		enc, err := json.Marshal(upstream)
		if err != nil {
			return nil, err
		}
		fields["model"] = enc
	}
	return json.Marshal(fields)
}

// isValidResponseID restricts a response ID to a safe alphabet and
// length (never a path separator, never a control byte). `.` is rejected
// (T-M2): the ID becomes one path segment of the outbound URL, and dot
// segments are not cleaned by url.URL{Path: …}, so `.`/`..` must never be
// allowed to reach the backend verbatim.
func isValidResponseID(id string) bool {
	if len(id) == 0 || len(id) > 256 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// stringField extracts a top-level string field.
func stringField(fields map[string]json.RawMessage, field string) (string, bool) {
	raw, ok := fields[field]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return "", false
	}
	return s, true
}

// maxLoggedModelLen bounds the client-supplied model name written to a
// log record or an accounting JSONL record so a hostile client cannot
// journal an unbounded string (T-L3). The value is a public model name,
// so a short bound suffices. Records with a huge model would otherwise
// exceed the accounting read bound and be skipped as unreadable waste
// (FIX-26 eval / FIX-28).
const maxLoggedModelLen = 128

// logModel truncates a client-supplied model name for structured logs
// and accounting records.
func logModel(m string) string {
	if len(m) > maxLoggedModelLen {
		return m[:maxLoggedModelLen]
	}
	return m
}

// topLevelID extracts the "id" field of a non-stream Responses body.
func topLevelID(body []byte) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return ""
	}
	id, _ := stringField(fields, "id")
	return id
}

// responseIDFromData extracts a response ID from a streaming data
// payload (PLAN §21.2: response.created or equivalent).
func responseIDFromData(data string) string {
	var obj struct {
		Type     string          `json:"type"`
		ID       string          `json:"id"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		return ""
	}
	// Prefer the nested response object.
	if len(obj.Response) > 0 {
		var nested struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(obj.Response, &nested); err == nil && nested.ID != "" {
			return nested.ID
		}
	}
	if obj.ID != "" {
		return obj.ID
	}
	return ""
}

// errBodyTooLarge is returned by readBodyLimited.
var errBodyTooLarge = errors.New("body too large")

// readBodyLimited reads a request body with a hard size bound
// (PLAN §12.1).
func readBodyLimited(r *http.Request, max int) ([]byte, error) {
	if r.ContentLength > int64(max) {
		return nil, errBodyTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > max {
		return nil, errBodyTooLarge
	}
	return body, nil
}

// passthroughHeaders returns the allow-listed end-to-end headers
// (PLAN §18). Client-controlled auth/identity headers are excluded.
func passthroughHeaders(r *http.Request) map[string][]string {
	out := map[string][]string{}
	if ua := r.Header.Get("User-Agent"); ua != "" {
		out["User-Agent"] = []string{ua}
	}
	if acc := r.Header.Get("Accept"); acc != "" {
		out["Accept"] = []string{acc}
	}
	return out
}

var (
	errJSON           = errors.New("invalid json")
	errMissingModel   = errors.New("missing model")
	errModelNotString = errors.New("model not a string")
	errCapExceeded    = errors.New("output limit exceeds policy cap")
	errCapInvalid     = errors.New("output limit must be a non-negative integer")
	errNotJSONObject  = errors.New("body is not a JSON object")
	// errStreamPanic is the sentinel a recovered pump-goroutine panic
	// becomes: the client already has committed stream headers, so the
	// only honest outcome is a truncated stream classified as an
	// upstream error (never a process kill).
	errStreamPanic = errors.New("stream reader panicked")
)

// prepareOutbound applies the generative output cap (PLAN §36) and, for
// streams, injects stream_options.include_usage (PLAN §38). ensureUsage
// selects whether stream-usage injection is enabled for this deployment.
// It returns the transformed body, the configured token reservation to
// charge when usage is unknown (PLAN §39), whether usage was injected on
// the client's behalf, or an error if the client asked for more output
// than the configured cap. configuredReservation is
// accounting.unknown_usage_reservation; when 0 the model's configured
// output cap is the reservation. The reservation is never the client's
// own output limit, so a small max_tokens cannot shrink the conservative
// charge.
//
// The model field is left untouched here; rewriteModel still overrides it
// per-attempt because the upstream model can differ across backends.
func prepareOutbound(fields map[string]json.RawMessage, o operation, cap int, stream, ensureUsage bool, configuredReservation int64) (reservation int64, injectedUsage bool, err error) {
	if o.capField == "" && !(stream && ensureUsage) {
		// Neither the output cap nor stream-usage injection applies
		// (e.g. embeddings, or usage injection disabled). The
		// unknown-usage reservation still does: returning 0 here would
		// let such a request settle nothing against the quota.
		return configuredReservation, false, nil
	}

	// Output cap (PLAN §36): never silently raise a client limit; reject
	// when the client asks for more than the configured cap; inject the
	// cap when the client supplied no output limit. Both the primary and
	// the alternate field are validated independently (T-M1), so an
	// over-cap value in either is caught; a negative or malformed limit
	// fails closed (errCapInvalid); an explicit 0 is forwarded unchanged
	// rather than silently raised.
	var limitSet bool
	if o.capField != "" && cap > 0 {
		present, _, terr := tokenLimit(fields, o.capField, cap)
		if terr != nil {
			return 0, false, terr
		}
		if present {
			limitSet = true
		}
		if o.altCapField != "" {
			present, _, terr := tokenLimit(fields, o.altCapField, cap)
			if terr != nil {
				return 0, false, terr
			}
			if present {
				limitSet = true
			}
		}
		if !limitSet {
			enc, _ := json.Marshal(int64(cap))
			fields[o.capField] = enc
		}
	}
	reservation = int64(cap)
	if configuredReservation > 0 {
		reservation = configuredReservation
	}

	// Stream usage injection (PLAN §38): known OpenAI-compatible
	// Chat/Completions requests. Responses API emits usage in-band, so no
	// injection there. injectedUsage is true only when the proxy actually
	// wrote include_usage; a client-set value (true or false) is preserved
	// and never counted as injected, so the pump does not swallow a chunk
	// the client asked for or one the proxy did not inject (FIX-09).
	if stream && ensureUsage {
		switch o.endpoint {
		case "chat.completions", "completions":
			injectedUsage = injectStreamUsage(fields)
		}
	}

	return reservation, injectedUsage, nil
}

// tokenLimit reads and validates one generative output-limit field
// (T-M1). absent → (false, 0, nil); present and a non-negative integer at
// or below cap → (true, v, nil); present but negative, malformed
// (non-integer, float, exponent, overflowing), or above cap → an error.
// A malformed or negative limit fails closed with errCapInvalid rather
// than being treated as absent, which could otherwise let an over-cap
// value slip past the cap.
func tokenLimit(fields map[string]json.RawMessage, name string, cap int) (present bool, v int64, err error) {
	raw, ok := fields[name]
	if !ok {
		return false, 0, nil
	}
	var num json.Number
	if uerr := json.Unmarshal(raw, &num); uerr != nil {
		return true, 0, errCapInvalid
	}
	n, ierr := num.Int64()
	if ierr != nil {
		return true, 0, errCapInvalid
	}
	if n < 0 {
		return true, 0, errCapInvalid
	}
	if n > int64(cap) {
		return true, 0, errCapExceeded
	}
	return true, n, nil
}

// injectStreamUsage sets stream_options.include_usage=true, preserving
// any other existing stream options (PLAN §38). It reports whether it
// injected the key, so callers set injectedUsage only for a real
// injection (FIX-09).
//
// An explicit include_usage:true is honoured and yields false: the
// client asked for the chunk, so the pump must not swallow it. Any other
// client value is overwritten, because suppressing the usage report
// would make the request settle the unknown-usage reservation instead of
// its real usage — a client-selectable quota bypass (T-A2). The injected
// chunk is swallowed, so the client's stream is unchanged.
func injectStreamUsage(fields map[string]json.RawMessage) bool {
	var so map[string]json.RawMessage
	if raw, ok := fields["stream_options"]; ok {
		_ = json.Unmarshal(raw, &so)
	}
	if so == nil {
		so = map[string]json.RawMessage{}
	}
	if raw, ok := so["include_usage"]; ok {
		var want bool
		if err := json.Unmarshal(raw, &want); err == nil && want {
			return false
		}
	}
	so["include_usage"] = json.RawMessage(`true`)
	enc, err := json.Marshal(so)
	if err != nil {
		return false
	}
	fields["stream_options"] = enc
	return true
}

// isUsageOnlyChunk reports whether a streaming data payload is the
// synthetic final usage-only chunk (usage present, no choices) that
// Mellomting injected on the client's behalf and may swallow (PLAN §38).
func isUsageOnlyChunk(data string) bool {
	var chunk struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   json.RawMessage   `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return false
	}
	// A literal "usage": null is an empty-choices frame carrying provider
	// metadata, not a usage-only chunk, and must be re-emitted verbatim
	// (PLAN §24). json.RawMessage captures "null" as 4 bytes, so without
	// this check the frame would be swallowed (FIX-08).
	return len(chunk.Choices) == 0 &&
		len(chunk.Usage) > 0 && !bytes.Equal(chunk.Usage, []byte("null"))
}

// windowLimits maps a key's configured token budgets to quota windows.
func windowLimits(l auth.KeyLimits) accounting.WindowLimit {
	return accounting.WindowLimit{TokensPerHour: l.TokensPerHour, TokensPerDay: l.TokensPerDay}
}

// account settles token quota and (when enabled) enqueues a JSONL record
// for one completed request (PLAN §39, §41, §42). Unknown usage on a
// successful request conservatively charges the configured reservation;
// the charged amount is recorded in charged_tokens so replay restores it
// (PLAN §40). Errors settle nothing.
func (p *Proxy) account(q *Req, o operation, start time.Time, out result) {
	if p.quota == nil && p.acc == nil {
		return
	}
	total := out.usage.Total
	if out.usageStatus == accounting.UsageUnknown {
		if out.status >= 200 && out.status < 300 {
			total = out.reservation // conservative charge (PLAN §39)
		} else {
			total = 0
		}
	}
	if p.quota != nil && total > 0 {
		p.quota.Settle(q.Key.ID, total, time.Now())
	}
	if p.acc != nil {
		p.acc.Enqueue(accounting.Record{
			Time:            time.Now().UTC(),
			RequestID:       q.RequestID,
			KeyID:           q.Key.ID,
			Model:           logModel(out.model),
			Backend:         out.backend,
			Endpoint:        o.endpoint,
			Status:          out.status,
			DurationMS:      time.Since(start).Milliseconds(),
			InputTokens:     out.usage.Input,
			OutputTokens:    out.usage.Output,
			TotalTokens:     out.usage.Total,
			CachedTokens:    out.usage.Cached,
			ReasoningTokens: out.usage.Reasoning,
			UsageStatus:     out.usageStatus,
			Retries:         out.retries,
			ChargedTokens:   total,
		})
	}
}
