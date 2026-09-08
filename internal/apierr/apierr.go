// Package apierr owns the sanitized client-facing error envelope
// (PLAN §72). Every error the product returns to a client is written
// here, so the guarantee that no Go stack trace, filesystem path,
// backend hostname, or backend body reaches a client is enforced in one
// place rather than in each package that answers a request.
//
// It is a dependency-free leaf: internal/proxy and internal/httpapi both
// emit client errors and the dependency between them runs one way, so
// neither can own the envelope for the other.
package apierr

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"
)

// Standard sanitized messages. They deliberately say nothing an
// unauthenticated caller could not already infer.
const (
	MsgNotFound  = "not found"
	MsgBadMethod = "method not allowed"
	MsgBadAuth   = "invalid api key"
	MsgOverload  = "server is overloaded"
	MsgRateLimit = "too many requests"
)

// DefaultRetryAfter is the conservative Retry-After, in seconds, used
// when a 429 has no computable window (T-Q12).
const DefaultRetryAfter = "1"

// Write emits a sanitized OpenAI-shaped error. A marshal failure falls
// back to a fixed literal rather than reporting the marshal error, which
// could carry internal detail.
func Write(w http.ResponseWriter, status int, typ, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	data, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    typ,
			"param":   nil,
			"code":    code,
		},
	})
	if err != nil {
		_, _ = w.Write([]byte(`{"error":{"message":"internal error"}}`))
		return
	}
	_, _ = w.Write(data)
}

// WriteRateLimit issues a sanitized 429, carrying Retry-After whenever a
// wait is computable (PLAN §34). Every proxy 429 carries one (T-Q12).
func WriteRateLimit(w http.ResponseWriter, retryAfter time.Duration) {
	if retryAfter > 0 {
		sec := max(int(math.Ceil(retryAfter.Seconds())), 1)
		w.Header().Set("Retry-After", strconv.Itoa(sec))
	}
	Write(w, http.StatusTooManyRequests, "rate_limit_error", "too_many_requests", MsgRateLimit)
}
