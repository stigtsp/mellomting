package proxy

import (
	"errors"

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
