package httpapi

import (
	"net/http"
	"time"

	"mellomting/internal/apierr"
)

// The route layer writes the same sanitized envelope as the proxy; both
// come from internal/apierr so PLAN §72 is enforced in one place.
const (
	msgNotFound  = apierr.MsgNotFound
	msgBadMethod = apierr.MsgBadMethod
	msgBadAuth   = apierr.MsgBadAuth
	msgOverload  = apierr.MsgOverload
)

func writeErr(w http.ResponseWriter, status int, typ, code, msg string) {
	apierr.Write(w, status, typ, code, msg)
}

func writeRateLimit(w http.ResponseWriter, retryAfter time.Duration) {
	apierr.WriteRateLimit(w, retryAfter)
}

// healthz/readyz reveal almost nothing (PLAN §69): a bare "ok" body and
// no operational detail.
func writeHealth(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
	// Guard the flush: a ResponseWriter wrapper without http.Flusher must
	// not panic (N9). The health endpoints and SSE rely on Flush when
	// available; when it is not, the buffered write still reaches the
	// client at finishRequest.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
