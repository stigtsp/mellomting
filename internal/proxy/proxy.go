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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

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
type operation struct {
	path       string // backend path
	method     string
	needsModel bool // body must carry a "model" field
	capture    bool // record created response IDs (responses create)
	respID     bool // ResponseID is authoritative (retrieve/cancel)
}

var (
	opChat       = operation{path: "/v1/chat/completions", method: "POST", needsModel: true}
	opLegacy     = operation{path: "/v1/completions", method: "POST", needsModel: true}
	opEmbed      = operation{path: "/v1/embeddings", method: "POST", needsModel: true}
	opResp       = operation{path: "/v1/responses", method: "POST", needsModel: true, capture: true}
	opRespGet    = operation{path: "/v1/responses", method: "GET", respID: true}
	opRespCancel = operation{path: "/v1/responses", method: "POST", respID: true}
)

// Proxy wires configuration, routing, and backend clients together.
type Proxy struct {
	router   *routing.Router
	clients  map[string]*backend.Client
	cfg      *config.Config
	log      *slog.Logger
	affinity *affinity
	budget   *budget
	draining atomic.Bool
}

// New builds a Proxy. clients is the backend-name -> client table built
// from the same configuration.
func New(cfg *config.Config, router *routing.Router, clients map[string]*backend.Client, log *slog.Logger) (*Proxy, error) {
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
}

// dispatch runs the full pipeline for one allow-listed operation.
func (p *Proxy) dispatch(q *Req, o operation) {
	out := result{status: 500, class: "internal_error", bytesIn: -1}
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
			"public_model", out.model,
			"backend", out.backend,
			"status", out.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"bytes_in", out.bytesIn,
			"bytes_out", out.bytesOut,
			"error_class", out.class,
		)
	}()

	fail := func(status int, typ, code, msg, class string) {
		out.status, out.class = status, class
		writeError(q.W, status, typ, code, msg)
	}

	// 1. Draining admission (PLAN §74.2).
	if p.Draining() {
		fail(503, "overload_error", "server_overloaded", "server is shutting down", "overload")
		return
	}

	// 2. Request body (bounded; PLAN §12.1). Authentication already
	// happened before body acquisition (PLAN §10).
	var body []byte
	var stream bool
	var publicModel string
	backendOverride := ""

	if o.method == "POST" {
		enc := q.R.Header.Get("Content-Encoding")
		if enc != "" && !strings.EqualFold(enc, "identity") {
			fail(415, "invalid_request_error", "unsupported_media_type",
				"only identity request encoding is supported", "bad_request")
			return
		}
		var rerr error
		body, rerr = readBodyLimited(q.R, p.cfg.Server.MaxBodyBytes)
		if errors.Is(rerr, errBodyTooLarge) {
			fail(413, "invalid_request_error", "body_too_large",
				"request body exceeds the size limit", "bad_request")
			return
		}
		if rerr != nil {
			fail(400, "invalid_request_error", "body_unreadable",
				"request body could not be read", "bad_request")
			return
		}
		out.bytesIn = len(body)
		if !p.budget.Acquire(int64(len(body))) {
			fail(503, "overload_error", "server_overloaded",
				"server is overloaded", "overload")
			return
		}
		defer p.budget.Release(int64(len(body)))
	}

	// 3. Shallow parse, model resolution, ACL (PLAN §12, §13, §31).
	if o.needsModel {
		var perr error
		body, publicModel, stream, perr = shallowParse(body)
		out.model = publicModel
		switch {
		case errors.Is(perr, errJSON):
			fail(400, "invalid_request_error", "invalid_json",
				"request body is not a valid JSON object", "bad_request")
			return
		case errors.Is(perr, errMissingModel):
			fail(400, "invalid_request_error", "missing_model",
				"the model field is required", "bad_request")
			return
		case errors.Is(perr, errModelNotString):
			fail(400, "invalid_request_error", "invalid_model",
				"the model field must be a string", "bad_request")
			return
		}
		// Do not reveal whether the model exists (PLAN §31): unknown
		// and not-allowed read identically.
		if !q.Key.Allows(publicModel) || !p.router.Has(publicModel) {
			fail(404, "invalid_request_error", "model_not_found_or_not_allowed",
				"model not found or not allowed", "authz")
			return
		}
		t, _ := p.router.Resolve(publicModel)
		backendOverride = t.Backend
		// Rewrite the outbound model name (PLAN §13).
		body, perr = rewriteModel(body, t.Upstream)
		if perr != nil {
			fail(400, "invalid_request_error", "invalid_json",
				"request body could not be normalized", "bad_request")
			return
		}
		if o.capture {
			if prev, ok := stringField(body, "previous_response_id"); ok {
				b, ok := p.affinity.Get(q.Key.ID, prev)
				if !ok {
					fail(404, "invalid_request_error", "response_not_found",
						"previous response not found", "affinity")
					return
				}
				backendOverride = b
			}
		}
	} else if o.respID {
		// Responses retrieve/cancel (PLAN §21.3): route to the owning
		// backend; an unknown ID may be forwarded only when exactly one
		// backend is possible for this key.
		if b, ok := p.affinity.Get(q.Key.ID, q.ResponseID); ok {
			backendOverride = b
		} else if only := p.singlePossibleBackend(q.Key); only != "" {
			backendOverride = only
		} else {
			fail(404, "invalid_request_error", "response_not_found",
				"response not found", "affinity")
			return
		}
	}

	if o.respID {
		// Build the outbound path with the response ID. The ID is
		// restricted to a safe charset here as defense in depth (the
		// route layer also validates it) so it can never inject a path
		// or host into the backend URL.
		if !isValidResponseID(q.ResponseID) {
			fail(404, "invalid_request_error", "response_not_found",
				"response not found", "bad_request")
			return
		}
		o.path = "/v1/responses/" + q.ResponseID
		if o.method == http.MethodPost {
			o.path += "/cancel"
		}
	}

	client, ok := p.clients[backendOverride]
	if !ok {
		fail(500, "api_error", "internal", "internal error", "internal_error")
		return
	}
	out.backend = backendOverride

	// 4. Forward (PLAN §18: only allow-listed headers are passed).
	uctx, ucancel := context.WithCancel(q.R.Context())
	defer ucancel()

	headers := passthroughHeaders(q.R)
	if len(body) > 0 {
		headers["Content-Type"] = []string{"application/json"}
	}
	res, err := client.Forward(uctx, backend.Request{
		Method:  o.method,
		Path:    o.path,
		Body:    body,
		Headers: headers,
		Stream:  stream,
	})

	// 5. Errors (sanitized, PLAN §43, §72).
	if err != nil {
		if errors.Is(err, context.Canceled) && q.R.Context().Err() != nil {
			// The client went away; no response is sent or possible.
			out.bytesOut = 0
			return
		}
		var up *backend.Upstream
		if errors.As(err, &up) {
			switch {
			case up.Status == 429:
				// 429: upstream is rate-limiting us
				fail(429, "rate_limit_error", "upstream_rate_limited",
					"upstream is rate limited", "backend_429")
				return
			case up.Status >= 500:
				// 5xx: upstream is failing
				fail(502, "api_error", "upstream_unavailable",
					"upstream is unavailable", "backend_5xx")
				return
			default:
				// 4xx: client-facing request was rejected by upstream
				fail(400, "invalid_request_error", "upstream_rejected",
					"upstream rejected the request", "backend_4xx")
				return
			}
		}
		switch {
		case errors.Is(err, backend.ErrQueueFull):
			fail(503, "overload_error", "server_overloaded", "server is overloaded", "queue_full")
		case errors.Is(err, backend.ErrTimeout):
			fail(504, "api_error", "upstream_timeout", "upstream timed out", "backend_timeout")
		case errors.Is(err, backend.ErrConnect):
			fail(502, "api_error", "upstream_unavailable", "upstream is unavailable", "backend_connect")
		case errors.Is(err, backend.ErrTooLarge):
			fail(502, "api_error", "upstream_unavailable", "upstream is unavailable", "backend_5xx")
		case errors.Is(err, backend.ErrPolicy):
			fail(500, "api_error", "internal", "internal error", "policy")
		default:
			fail(502, "api_error", "upstream_unavailable", "upstream is unavailable", "backend_5xx")
		}
		return
	}

	// 6. Success.
	if stream {
		status, bytesOut, cls := p.pump(q, res, o, ucancel, backendOverride)
		out.status, out.bytesOut, out.class = status, bytesOut, cls
		return
	}
	res.Close()
	out.status = res.Status
	out.class = "ok"
	out.bytesOut = len(res.BodyBytes)
	ct := "application/json"
	if v := res.Header.Get("Content-Type"); v != "" {
		ct = v
	}
	q.W.Header().Set("Content-Type", ct)
	q.W.WriteHeader(res.Status)
	_, _ = q.W.Write(res.BodyBytes)
	if o.capture {
		if id := topLevelID(res.BodyBytes); id != "" {
			p.affinity.Put(q.Key.ID, id, backendOverride)
		}
	}
}

// pump relays an upstream SSE stream to the client (PLAN §24): event
// order is preserved, each event is emitted promptly as it is parsed
// (no whole-response buffering), upstream read-idle is bounded by
// stream_idle_timeout, client write-idle is bounded by
// stream_write_timeout, and any terminal path cancels the upstream
// context.
func (p *Proxy) pump(q *Req, res *backend.Result, o operation, ucancel context.CancelFunc, backendName string) (int, int, string) {
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
	idle := p.cfg.Server.StreamIdleTimeout.Duration()
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

		// Capture response IDs for affinity (PLAN §21.2).
		if o.capture && !captured {
			if data, ok := dataField(ev); ok {
				if id := responseIDFromData(data); id != "" {
					p.affinity.Put(q.Key.ID, id, backendName)
					captured = true
				}
			}
		}

		// Relay the event verbatim (PLAN §24), bounding client
		// write-idle via the conn write deadline where usable.
		if writeDeadlineUsable {
			if err := ctrl.SetWriteDeadline(time.Now().Add(clientIdle)); err != nil {
				writeDeadlineUsable = false
			}
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

// singlePossibleBackend returns the unique backend that the key may use
// for generation models, if exactly one applies (PLAN §21.3).
func (p *Proxy) singlePossibleBackend(key *auth.Key) string {
	seen := map[string]bool{}
	for _, name := range p.router.List() {
		t, err := p.router.Resolve(name)
		if err != nil || t.Type != "generation" || !key.Allows(name) {
			continue
		}
		seen[t.Backend] = true
	}
	if len(seen) == 1 {
		for b := range seen {
			return b
		}
	}
	return ""
}

// shallowParse inspects the routing/policy fields of a JSON body and
// returns a normalized body with unknown fields preserved verbatim
// (PLAN §12).
func shallowParse(body []byte) (newBody []byte, model string, stream bool, err error) {
	if len(body) == 0 {
		return nil, "", false, errMissingModel
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
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
	return body, model, stream, nil
}

// rewriteModel sets the outbound model field and re-serializes, keeping
// every other field byte-identical via json.RawMessage (PLAN §12, §13).
func rewriteModel(body []byte, upstream string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	enc, err := json.Marshal(upstream)
	if err != nil {
		return nil, err
	}
	fields["model"] = enc
	return json.Marshal(fields)
}

// isValidResponseID restricts a response ID to a safe alphabet and
// length (never a path separator, never a control byte).
func isValidResponseID(id string) bool {
	if len(id) == 0 || len(id) > 256 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return false
		}
	}
	return true
}

// stringField extracts a top-level string field.
func stringField(body []byte, field string) (string, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", false
	}
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

// topLevelID extracts the "id" field of a non-stream Responses body.
func topLevelID(body []byte) string {
	id, _ := stringField(body, "id")
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
)

// writeError emits a sanitized OpenAI-shaped error (PLAN §72).
func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload := map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    typ,
			"param":   nil,
			"code":    code,
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		_, _ = w.Write([]byte(`{"error":{"message":"internal error"}}`))
		return
	}
	_, _ = w.Write(data)
}
