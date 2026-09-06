package httpapi

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"mellomting/internal/config"
)

// maxForwardedHops bounds how far back the X-Forwarded-For chain is
// walked. The header is attacker-controlled up to the point the trusted
// proxy appends to it, so a client can make it arbitrarily long within
// max_header_bytes; the walk stops at the first untrusted hop anyway, so
// only a chain of trusted addresses reaches this bound.
const maxForwardedHops = 32

// trustedPeers decides whether a request's socket peer is a reverse
// proxy whose forwarded client address may be believed (PLAN §18.1).
// The zero value trusts nobody, which is the default: without an
// explicit server.trusted_proxies the socket peer is the client and
// X-Forwarded-For is never read.
type trustedPeers struct {
	nets []netip.Prefix
	unix bool
}

// newTrustedPeers builds the matcher from validated configuration.
// Entries that do not parse are dropped rather than trusted; validation
// has already rejected them, so this only guards an un-validated
// configuration reaching the server.
func newTrustedPeers(entries []string) trustedPeers {
	var t trustedPeers
	for _, entry := range entries {
		if entry == config.TrustedProxyUnix {
			t.unix = true
			continue
		}
		if p, err := netip.ParsePrefix(entry); err == nil {
			t.nets = append(t.nets, p.Masked())
		}
	}
	return t
}

// any reports whether any peer is trusted at all, which is what decides
// whether a forwarded header is looked at.
func (t trustedPeers) any() bool { return t.unix || len(t.nets) > 0 }

// trusts reports whether peer is a configured reverse proxy. A peer that
// is not an IP address is the peer of a Unix-socket listener: it has no
// address of its own, so the only question is whether the operator
// declared that socket to be fronted by a proxy.
func (t trustedPeers) trusts(peer string) bool {
	addr, err := netip.ParseAddr(peer)
	if err != nil {
		return t.unix
	}
	addr = addr.Unmap()
	for _, p := range t.nets {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// clientIP resolves the address to rate-limit and log a request under.
// It is the socket peer unless that peer is a trusted proxy, in which
// case it is the rightmost address in X-Forwarded-For that is not
// itself trusted — the address the closest trusted proxy observed.
//
// Right to left is what makes this safe: a client can prepend anything
// it likes to the header, but those entries sit to the LEFT of the one
// the trusted proxy appended, so they are never reached. A chain that
// breaks (a hop that is not an address) falls back to the socket peer
// rather than believing what lies beyond the break.
func (t trustedPeers) clientIP(r *http.Request) string {
	peer := peerString(r)
	if !t.any() || !t.trusts(peer) {
		return peer
	}
	hops := 0
	values := r.Header.Values("X-Forwarded-For")
	for i := len(values) - 1; i >= 0; i-- {
		parts := strings.Split(values[i], ",")
		for j := len(parts) - 1; j >= 0; j-- {
			hops++
			if hops > maxForwardedHops {
				return peer
			}
			hop, ok := forwardedAddr(parts[j])
			if !ok {
				return peer
			}
			if !t.trusts(hop) {
				return hop
			}
		}
	}
	return peer
}

// forwardedAddr normalizes one X-Forwarded-For element to a bare
// address. Some proxies append a port, and IPv6 elements may be
// bracketed; anything that is not an address at all fails.
func forwardedAddr(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return "", false
	}
	return addr.Unmap().String(), true
}
