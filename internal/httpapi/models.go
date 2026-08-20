package httpapi

import (
	"encoding/json"
	"net/http"

	"mellomting/internal/auth"
)

// handleModels synthesizes /v1/models filtered by the key's ACL
// (PLAN §14). It returns stable OpenAI-like model objects and never
// exposes backend URLs, backend names, or implementation detail. A key
// without access sees an empty list: /v1/models is the only discovery
// mechanism (PLAN §31).
func (s *Server) handleModels(w http.ResponseWriter, key *auth.Key) {
	names := s.router.List() // sorted by the router
	data := make([]map[string]any, 0, len(names))
	for _, name := range names {
		if !key.Allows(name) {
			continue
		}
		data = append(data, map[string]any{
			"id":       name,
			"object":   "model",
			"created":  s.startedUnix,
			"owned_by": "mellomting",
		})
	}
	out := map[string]any{"object": "list", "data": data}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	buf, err := json.Marshal(out)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "api_error", "internal", "internal error")
		return
	}
	_, _ = w.Write(buf)
}
