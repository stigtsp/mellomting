package landlock

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Policy is the post-startup Landlock policy of the daemon (PLAN
// §58-60): the small set of capabilities the process needs after all
// secrets have been preloaded and their file descriptors closed. It is
// plain data so it can be built and tested on any platform; applying it
// is Linux-only (see Apply).
type Policy struct {
	// ReadFiles are pathnames the daemon may open read-only after
	// confinement. This is the users file: a SIGHUP reload (PLAN §30)
	// re-reads it by pathname.
	ReadFiles []string
	// WriteFiles are pathnames the daemon may open for writing after
	// confinement. This is the accounting log only if it is (re-)opened
	// by pathname (PLAN §58).
	WriteFiles []string
	// ConnectTCP are the TCP destination ports the daemon may connect
	// to. Landlock TCP rules are port-based and carry no destination IP
	// (PLAN §60); external address restriction is the job of the backend
	// network modes (PLAN §16).
	ConnectTCP []uint16
}

// Summarize renders the policy for operational logs (PLAN §43): the
// summary carries no secret.
func (p Policy) Summarize() []string {
	out := make([]string, 0, len(p.ReadFiles)+len(p.WriteFiles)+len(p.ConnectTCP))
	for _, f := range p.ReadFiles {
		out = append(out, "read "+f)
	}
	for _, f := range p.WriteFiles {
		out = append(out, "write "+f)
	}
	for _, port := range p.ConnectTCP {
		out = append(out, fmt.Sprintf("connect tcp %d", port))
	}
	return out
}

// BackendPorts extracts the TCP destination port of each backend base
// URL (PLAN §60). An omitted port uses the scheme default (80 for
// http, 443 for https). The result is sorted and de-duplicated. A
// malformed URL, an unsupported scheme, or an invalid port is an error:
// the policy is fail-closed (PLAN §55).
func BackendPorts(baseURLs ...string) ([]uint16, error) {
	seen := make(map[uint16]bool, len(baseURLs))
	var ports []uint16
	for _, raw := range baseURLs {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("%q is not a valid backend base URL", redactURL(raw))
		}
		switch u.Scheme {
		case "http", "https":
		default:
			return nil, fmt.Errorf("%q: unsupported URL scheme %q (want http or https)", redactURL(raw), u.Scheme)
		}
		port := u.Port()
		if port == "" {
			var ok bool
			port, ok = DefaultPortForScheme(u.Scheme)
			if !ok {
				return nil, fmt.Errorf("%q: unsupported URL scheme %q (want http or https)", redactURL(raw), u.Scheme)
			}
		}
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%q carries an invalid TCP port %q", redactURL(raw), port)
		}
		if !seen[uint16(n)] {
			seen[uint16(n)] = true
			ports = append(ports, uint16(n))
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return ports, nil
}

// redactURL removes any userinfo (credentials) from a URL string for use
// in error messages, so a malformed backend base_url with inlined
// credentials never leaks them to the operator or logs (T-M13). The
// policy is fail-closed (PLAN §55), so a valid URL that already passed
// config validation carries no userinfo; this is defence-in-depth for
// direct callers. Best-effort: it works even when the URL failed to
// parse. Mirrored locally because internal/config cannot be imported
// here (config imports landlock).
func redactURL(raw string) string {
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 0 {
		return raw
	}
	rest := raw[schemeEnd+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return raw
	}
	// Only redact when the '@' is part of the authority (before any
	// path/query/fragment separator), not an email-like string in a
	// path.
	slash := strings.IndexAny(rest, "/?#")
	if slash >= 0 && at > slash {
		return raw
	}
	return raw[:schemeEnd+3] + "***@" + rest[at+1:]
}
