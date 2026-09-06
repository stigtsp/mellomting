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
// proxy appends to it; the walk stops at the first untrusted hop, so
// only a chain of trusted addresses reaches this bound.
const maxForwardedHops = 32

// trustedPeers decides whether a request's socket peer is a reverse
// proxy whose forwarded client address may be believed (PLAN §18.1).
// The zero value trusts nobody: without server.trusted_proxies the
// socket peer is the client and X-Forwarded-For is never read.
type trustedPeers struct {
	nets []netip.Prefix
	// unix: the listener is a Unix socket the operator declared to be
	// fronted by a proxy. Its peer has no address, so whoever may open
	// the socket is the proxy.
	unix bool
}

// newTrustedPeers builds the matcher from validated configuration,
// which guarantees "unix" appears only with a unix listener and CIDRs
// only with a TCP one.
func newTrustedPeers(network string, entries []string) trustedPeers {
	var t trustedPeers
	for _, entry := range entries {
		if entry == config.TrustedProxyUnix {
			t.unix = network == "unix"
			continue
		}
		if p, err := netip.ParsePrefix(entry); err == nil {
			t.nets = append(t.nets, p.Masked())
		}
	}
	return t
}

func (t trustedPeers) trusts(addr netip.Addr) bool {
	for _, p := range t.nets {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// clientIP resolves the address to rate-limit and log a request under:
// the socket peer, unless that peer is a trusted proxy, in which case
// the rightmost X-Forwarded-For entry that is not itself trusted — the
// address the closest trusted proxy observed.
//
// Right to left is what makes this safe: a client can prepend anything
// it likes, but its entries sit to the LEFT of the one the trusted
// proxy appended, so they are never reached. A chain that breaks (a hop
// that is not an address) falls back to the socket peer rather than
// believing what lies beyond the break.
func (t trustedPeers) clientIP(r *http.Request) string {
	peer := peerString(r)
	if !t.unix {
		addr, err := netip.ParseAddr(peer)
		if err != nil || !t.trusts(addr.Unmap()) {
			return peer
		}
	}
	hops := 0
	values := r.Header.Values("X-Forwarded-For")
	for i := len(values) - 1; i >= 0; i-- {
		rest := values[i]
		for {
			cut := strings.LastIndexByte(rest, ',')
			if hops++; hops > maxForwardedHops {
				return peer
			}
			addr, ok := forwardedAddr(rest[cut+1:])
			if !ok {
				return peer
			}
			if !t.trusts(addr) {
				return addr.String()
			}
			if cut < 0 {
				break
			}
			rest = rest[:cut]
		}
	}
	return peer
}

// forwardedAddr parses one X-Forwarded-For element. Some proxies append
// a port, and IPv6 elements may be bracketed.
func forwardedAddr(raw string) (netip.Addr, bool) {
	s := strings.TrimSpace(raw)
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}
