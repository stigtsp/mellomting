package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func request(remoteAddr string, forwarded ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = remoteAddr
	r.Header.Del("X-Forwarded-For")
	for _, f := range forwarded {
		r.Header.Add("X-Forwarded-For", f)
	}
	return r
}

// PLAN §18.1: X-Forwarded-For is consumed only from a configured
// trusted proxy. Every other case resolves to the socket peer, because
// a header any client can set decides which per-source rate bucket the
// request lands in.
func TestClientIP(t *testing.T) {
	for _, tc := range []struct {
		name      string
		trusted   []string
		remote    string
		forwarded []string
		want      string
	}{{
		name:      "no trusted proxies ignores the header",
		remote:    "203.0.113.7:5555",
		forwarded: []string{"198.51.100.9"},
		want:      "203.0.113.7",
	}, {
		name:      "untrusted peer ignores the header",
		trusted:   []string{"127.0.0.1/32"},
		remote:    "203.0.113.7:5555",
		forwarded: []string{"198.51.100.9"},
		want:      "203.0.113.7",
	}, {
		name:      "trusted peer yields the forwarded client",
		trusted:   []string{"127.0.0.1/32"},
		remote:    "127.0.0.1:5555",
		forwarded: []string{"198.51.100.9"},
		want:      "198.51.100.9",
	}, {
		// The client controls everything to the LEFT of what the proxy
		// appended, so walking right to left never reaches its forgery.
		name:      "spoofed prefix is not reached",
		trusted:   []string{"127.0.0.1/32"},
		remote:    "127.0.0.1:5555",
		forwarded: []string{"1.2.3.4, 198.51.100.9"},
		want:      "198.51.100.9",
	}, {
		name:      "chained trusted proxies are skipped",
		trusted:   []string{"127.0.0.1/32", "10.0.0.0/8"},
		remote:    "127.0.0.1:5555",
		forwarded: []string{"198.51.100.9, 10.0.0.7", "10.0.0.8"},
		want:      "198.51.100.9",
	}, {
		// A client that sets its own header before the proxy appends
		// produces two header values; they are one chain, in order.
		name:      "multiple header values are one chain",
		trusted:   []string{"127.0.0.1/32"},
		remote:    "127.0.0.1:5555",
		forwarded: []string{"1.2.3.4", "198.51.100.9"},
		want:      "198.51.100.9",
	}, {
		name:      "a broken chain falls back to the socket peer",
		trusted:   []string{"127.0.0.1/32"},
		remote:    "127.0.0.1:5555",
		forwarded: []string{"198.51.100.9, not-an-address"},
		want:      "127.0.0.1",
	}, {
		name:      "an all-trusted chain falls back to the socket peer",
		trusted:   []string{"127.0.0.1/32"},
		remote:    "127.0.0.1:5555",
		forwarded: []string{"127.0.0.1"},
		want:      "127.0.0.1",
	}, {
		name:      "an empty header falls back to the socket peer",
		trusted:   []string{"127.0.0.1/32"},
		remote:    "127.0.0.1:5555",
		forwarded: []string{""},
		want:      "127.0.0.1",
	}, {
		name:      "a forwarded port is stripped",
		trusted:   []string{"127.0.0.1/32"},
		remote:    "127.0.0.1:5555",
		forwarded: []string{"198.51.100.9:41234"},
		want:      "198.51.100.9",
	}, {
		name:      "an ipv6 client is unbracketed",
		trusted:   []string{"127.0.0.1/32"},
		remote:    "127.0.0.1:5555",
		forwarded: []string{"[2001:db8::1]:41234"},
		want:      "2001:db8::1",
	}, {
		name:      "an ipv4-mapped peer matches its ipv4 prefix",
		trusted:   []string{"127.0.0.1/32"},
		remote:    "[::ffff:127.0.0.1]:5555",
		forwarded: []string{"198.51.100.9"},
		want:      "198.51.100.9",
	}, {
		// A unix listener's peer has no address of its own: whoever may
		// open the socket is the proxy, and the operator says so.
		name:      "unix peer trusted by the unix entry",
		trusted:   []string{"unix"},
		remote:    "@",
		forwarded: []string{"198.51.100.9"},
		want:      "198.51.100.9",
	}, {
		name:      "unix peer untrusted without the unix entry",
		trusted:   []string{"127.0.0.1/32"},
		remote:    "@",
		forwarded: []string{"198.51.100.9"},
		want:      "@",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			peers := newTrustedPeers(tc.trusted)
			if got := peers.clientIP(request(tc.remote, tc.forwarded...)); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// A chain of trusted hops is bounded: the header is attacker-controlled
// up to the trusted proxy, so an enormous one must not be walked in
// full.
func TestClientIPBoundsTheChain(t *testing.T) {
	peers := newTrustedPeers([]string{"127.0.0.1/32"})
	hops := make([]string, maxForwardedHops+10)
	for i := range hops {
		hops[i] = "127.0.0.1"
	}
	hops[0] = "198.51.100.9" // beyond the bound, so never reached
	r := request("127.0.0.1:5555", hops...)
	if got := peers.clientIP(r); got != "127.0.0.1" {
		t.Fatalf("clientIP = %q, want the socket peer once the hop bound is hit", got)
	}
}
