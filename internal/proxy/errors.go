package proxy

import (
	"errors"
	"net/http"

	"mellomting/internal/backend"
)

// apiError is one sanitized client-facing outcome: the HTTP status, the
// OpenAI-shaped error triple, and the class recorded in the request log
// and the usage record. Collecting them here makes the product's entire
// client-visible error surface auditable in one place (PLAN §72), and
// keeps the class — which drives log and accounting classification —
// next to the response it belongs to rather than as a fifth positional
// string at the call site.
type apiError struct {
	status int
	typ    string
	code   string
	msg    string
	class  string
}

// The sanitized errors the proxy can return. None reveals a backend
// hostname, a filesystem path, a Go error, or an upstream body.
var (
	errDraining           = apiError{503, "overload_error", "server_overloaded", "server is shutting down", "overload"}
	errOverloaded         = apiError{503, "overload_error", "server_overloaded", "server is overloaded", "overload"}
	errNoBackend          = apiError{503, "overload_error", "server_overloaded", "no backend is available", "no_backend_available"}
	errUnsupported        = apiError{415, "invalid_request_error", "unsupported_media_type", "request compression is not supported", "bad_request"}
	errBodyTooBig         = apiError{413, "invalid_request_error", "body_too_large", "request body exceeds the size limit", "bad_request"}
	errBodyUnread         = apiError{400, "invalid_request_error", "body_unreadable", "request body could not be read", "bad_request"}
	errBadJSON            = apiError{400, "invalid_request_error", "invalid_json", "request body is not a valid JSON object", "bad_request"}
	errNoModel            = apiError{400, "invalid_request_error", "missing_model", "the model field is required", "bad_request"}
	errModelType          = apiError{400, "invalid_request_error", "invalid_model", "the model field must be a string", "bad_request"}
	errNoStream           = apiError{400, "invalid_request_error", "stream_not_supported", "streaming is not supported for this endpoint", "bad_request"}
	errModelDenied        = apiError{404, "invalid_request_error", "model_not_found_or_not_allowed", "model not found or not allowed", "authz"}
	errNotGenerate        = apiError{400, "invalid_request_error", "model_type_mismatch", "the requested model is not a generation model", "bad_request"}
	errNotEmbed           = apiError{400, "invalid_request_error", "model_type_mismatch", "the requested model is not an embedding model", "bad_request"}
	errPrevNotFound       = apiError{404, "invalid_request_error", "response_not_found", "previous response not found", "affinity"}
	errRespNotFound       = apiError{404, "invalid_request_error", "response_not_found", "response not found", "affinity"}
	errRespBadID          = apiError{404, "invalid_request_error", "response_not_found", "response not found", "bad_request"}
	errOutputCapExceeded  = apiError{400, "invalid_request_error", "output_limit_exceeded", "requested output tokens exceed the model limit", "bad_request"}
	errOutputLimitInvalid = apiError{400, "invalid_request_error", "invalid_output_limit", "output limit must be a non-negative integer", "bad_request"}
	errNotNormal          = apiError{400, "invalid_request_error", "invalid_json", "request body could not be processed", "bad_request"}
	errQuota              = apiError{429, "rate_limit_error", "token_quota_exceeded", "token quota exceeded for this window", "token_quota"}
	errInternal           = apiError{500, "api_error", "internal", "internal error", "internal_error"}
	errPolicy             = apiError{500, "api_error", "internal", "internal error", "policy"}

	// Terminal backend failures.
	errUpstreamRateLimited = apiError{429, "rate_limit_error", "upstream_rate_limited", "upstream is rate limited", "backend_429"}
	errUpstream5xx         = apiError{502, "api_error", "upstream_unavailable", "upstream is unavailable", "backend_5xx"}
	errUpstreamRejected    = apiError{400, "invalid_request_error", "upstream_rejected", "upstream rejected the request", "backend_4xx"}
	errUpstreamTimeout     = apiError{504, "api_error", "upstream_timeout", "upstream timed out", "backend_timeout"}
	errUpstreamConnect     = apiError{502, "api_error", "upstream_unavailable", "upstream is unavailable", "backend_connect"}
	errQueueFull           = apiError{503, "overload_error", "server_overloaded", "server is overloaded", "queue_full"}
)

// backendFailure is the complete behaviour of one backend error: whether
// the request may be retried (PLAN §23), whether the failure poisons the
// backend's passive health state (PLAN §70), and the sanitized response
// it produces. Keeping the three together means each error's behaviour
// reads as one row, instead of being spread over three switches that had
// to be kept in agreement.
type backendFailure struct {
	retryable     bool
	poisonsHealth bool
	resp          apiError
}

// classifyBackendError maps a backend error to its full behaviour.
//
// ErrHeaderTimeout is deliberately neither retryable nor
// health-poisoning: it is a latency/capacity signal, retrying re-issues a
// full generation that cannot succeed within the header bound, and
// poisoning would cascade a cooldown across every model and key sharing
// that backend (X6).
//
// A backend 3xx is terminal: redirects are never followed, so the status
// maps to a sanitized upstream_rejected and nothing about its location
// reaches the client.
func classifyBackendError(err error) backendFailure {
	if up, ok := errors.AsType[*backend.Upstream](err); ok {
		switch {
		case up.Status == 429:
			return backendFailure{retryable: true, resp: errUpstreamRateLimited}
		case up.Status >= 500:
			return backendFailure{
				retryable: up.Status == 502 || up.Status == 503 || up.Status == 504,
				resp:      errUpstream5xx,
			}
		default:
			return backendFailure{resp: errUpstreamRejected}
		}
	}
	switch {
	case errors.Is(err, backend.ErrConnect):
		return backendFailure{retryable: true, poisonsHealth: true, resp: errUpstreamConnect}
	case errors.Is(err, backend.ErrDialTimeout):
		return backendFailure{retryable: true, poisonsHealth: true, resp: errUpstreamTimeout}
	case errors.Is(err, backend.ErrDraining):
		return backendFailure{resp: errDraining}
	case errors.Is(err, backend.ErrQueueFull):
		return backendFailure{retryable: true, resp: errQueueFull}
	case errors.Is(err, backend.ErrHeaderTimeout), errors.Is(err, backend.ErrTimeout):
		return backendFailure{resp: errUpstreamTimeout}
	case errors.Is(err, backend.ErrTooLarge):
		return backendFailure{resp: errUpstream5xx}
	case errors.Is(err, backend.ErrPolicy):
		return backendFailure{resp: errPolicy}
	default:
		return backendFailure{resp: errUpstream5xx}
	}
}

// logBackendFailure records one failed attempt for an operator: the
// sanitized class always, and the backend's own bounded, scalar-only
// triple whenever it answered with a status.
//
// It covers every backend failure, not only the ones carrying a body.
// A header timeout is the failure this diagnostic exists for, and it
// has no body at all; a line that appeared only for response statuses
// would be silent in exactly the recurrence an operator turns debug on
// to investigate.
//
// Debug level, because a rejection message can quote the request it
// rejected and prompts stay out of the default operational log
// (PLAN §24, §43). The request line's error_class remains what an
// operator alerts on; this is what tells a chat-template mismatch from
// an output-limit, tool-schema, or context-window failure, which
// error_class alone cannot distinguish. The fields are empty when the
// backend answered with a body this cannot parse — that is the answer,
// not a missing line.
func (p *Proxy) logBackendFailure(requestID, backendName string, err error) {
	attrs := []any{
		"request_id", requestID,
		"backend", backendName,
		"error_class", classifyBackendError(err).resp.class,
	}
	if up, ok := errors.AsType[*backend.Upstream](err); ok {
		attrs = append(attrs,
			"status", up.Status,
			"upstream_type", up.Detail.Type,
			"upstream_code", up.Detail.Code,
			"upstream_message", up.Detail.Message,
		)
	}
	p.log.Debug("backend attempt failed", attrs...)
}

// relayableUpstreamStatus lists the backend statuses whose message is
// about the client's request and so may be relayed to it.
//
// The complement is what matters. A 401 or 403 is about the credential
// Mellomting injected from api_key_file, and gateways quote the key they
// rejected. A 404 names the backend's own model — the upstream name
// routing rewrites precisely so a client never learns it — and vLLM
// spells it as a filesystem path. None of those is a statement about the
// request the client sent, and all of them are what PLAN §72 keeps out
// of a client response.
var relayableUpstreamStatus = map[int]bool{
	http.StatusBadRequest:            true, // 400
	http.StatusRequestEntityTooLarge: true, // 413
	http.StatusUnprocessableEntity:   true, // 422, FastAPI validation
	http.StatusTooManyRequests:       true, // 429
}

// relayUpstreamDetail appends the backend's sanitized message to the
// client-facing error, for a backend configured to relay it.
//
// The status, type, and code are untouched. Clients switch on those, so
// they stay Mellomting's own vocabulary whatever an operator trusts a
// backend with; only the human-readable message grows.
//
// Disclosure is recorded at the level it is disclosed. The debug line is
// for the operator and can stay off; text that has already reached a
// client must appear in the log whatever the level, or nothing says what
// was handed out.
func (p *Proxy) relayUpstreamDetail(e apiError, requestID, backendName string, err error) apiError {
	if !p.cfg.Backends[backendName].RelayUpstreamErrors {
		return e
	}
	up, ok := errors.AsType[*backend.Upstream](err)
	if !ok || !relayableUpstreamStatus[up.Status] || up.Detail.Message == "" {
		return e
	}
	e.msg += ": " + up.Detail.Message
	p.log.Info("relayed backend error detail",
		"request_id", requestID,
		"backend", backendName,
		"status", up.Status,
		"upstream_message", up.Detail.Message,
	)
	return e
}
