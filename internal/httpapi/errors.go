package httpapi

import (
	"encoding/json"
	"net/http"
)

// writeErr emits a sanitized OpenAI-shaped error (PLAN §72). It never
// includes Go stack traces, backend hostnames, filesystem paths, or
// backend bodies.
func writeErr(w http.ResponseWriter, status int, typ, code, msg string) {
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

// Standard sanitized errors for the route layer.
const (
	msgNotFound  = "not found"
	msgBadMethod = "method not allowed"
	msgNoAuth    = "missing authentication"
	msgBadAuth   = "invalid api key"
	msgOverload  = "server is overloaded"
)

// healthz/readyz reveal almost nothing (PLAN §69): a bare "ok" body and
// no operational detail.
func writeHealth(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
	w.(http.Flusher).Flush()
}
