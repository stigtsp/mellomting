// Package adminapi serves the daemon's live-request view over a
// dedicated socket (PLAN §43).
//
// It is deliberately not part of the ingress surface. What it answers —
// every caller's address, user agent, key and token use — is exactly
// what one tenant must not learn about another, and the ingress is open
// to every client that holds a key. The separation is the socket's file
// permissions: the daemon creates it 0600, so only the service account
// and root can read it, and no API key grants access to it.
package adminapi

import (
	"encoding/json"
	"net/http"

	"mellomting/internal/inflight"
)

// Snapshot is the response of GET /inflight.
type Snapshot struct {
	Requests []inflight.Record `json:"requests"`
}

// Handler serves the admin routes. Like the ingress it is an explicit
// route table with no catch-all (PLAN §11.3): this socket answers one
// question and nothing else.
func Handler(live *inflight.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /inflight", func(w http.ResponseWriter, _ *http.Request) {
		body, err := json.Marshal(Snapshot{Requests: live.Snapshot()})
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	return mux
}
