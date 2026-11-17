package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"mellomting/internal/agent"
	"mellomting/internal/auth"
)

// handleAttach upgrades an agent's connection and hands it to the agent
// registry (PLAN §104). It runs after authentication and the global and
// pre-auth limits; the per-key limits do not apply, since the request
// ends here and the connection is no longer an HTTP request.
func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request, key *auth.Key, remote string) {
	if s.agents == nil {
		writeErr(w, http.StatusNotFound, "invalid_request_error", "not_found", msgNotFound)
		return
	}
	if !key.IsAgent() {
		s.attachRefused("not an agent key", key, remote)
		writeErr(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", msgBadAuth)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", msgBadMethod)
		return
	}
	if r.ProtoMajor != 1 || !headerHasToken(r.Header, "Connection", "upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), agent.UpgradeProtocol) {
		w.Header().Set("Upgrade", agent.UpgradeProtocol)
		writeErr(w, http.StatusUpgradeRequired, "invalid_request_error", "upgrade_required", "attach requires an HTTP/1.1 upgrade to "+agent.UpgradeProtocol)
		return
	}
	// The slot is claimed before the upgrade: a full cap is a 503 the
	// agent understands, never a 101 followed by a dropped connection.
	res, limit := s.agents.Reserve(key, remote)
	if limit != "" {
		s.attachRefused("limit "+limit, key, remote)
		writeErr(w, http.StatusServiceUnavailable, "overload_error", "server_overloaded", msgOverload)
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		res.Cancel()
		s.log.Error("agent attach: hijack failed", "remote", remote, "error", err)
		writeErr(w, http.StatusInternalServerError, "api_error", "internal", "internal error")
		return
	}
	// The connection now outlives this request: drop the server's read
	// deadline, which would otherwise sever it.
	_ = conn.SetDeadline(time.Time{})
	if _, err := fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", agent.UpgradeProtocol); err != nil {
		res.Cancel()
		_ = conn.Close()
		return
	}
	if err := brw.Flush(); err != nil {
		res.Cancel()
		_ = conn.Close()
		return
	}
	res.Attach(conn, brw.Reader)
}

// attachRefused logs a refused attach, by a key that is not an agent key
// or one at an attachment cap: the client only sees a 401 or 503, and an
// operator enrolling machines needs to see which key and why.
// Rate-limited like "auth rejected" (PLAN §33), since a valid key can
// repeat the request.
func (s *Server) attachRefused(reason string, key *auth.Key, remote string) {
	if ok, _ := s.attachLog.Allow(time.Now()); !ok {
		s.attachDropped.Add(1)
		return
	}
	s.log.Warn("agent attach refused", "reason", reason, "key", key.Name, "remote", remote,
		"suppressed", s.attachDropped.Swap(0))
}

// headerHasToken reports whether a comma-separated header lists token,
// case-insensitively.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}
