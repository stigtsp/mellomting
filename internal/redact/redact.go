// Package redact strips credentials from values that appear in operator
// errors and logs. It is a dependency-free leaf so that both
// internal/config and internal/landlock can use one implementation:
// config imports landlock, so neither could own the helper without the
// other mirroring it (T-M13).
package redact

import "strings"

// Marker replaces the userinfo of a redacted URL.
const Marker = "<redacted>"

// URL removes any userinfo (credentials) from a URL string. It is
// best-effort by design: it works on strings that failed to parse, which
// is exactly when a malformed base_url would otherwise reach an operator
// message with inlined credentials. net/url's URL.Redacted is not a
// substitute — it requires a successful parse and masks only the
// password.
func URL(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw
	}
	at := strings.Index(rest, "@")
	if at < 0 {
		return raw
	}
	// Redact only when the '@' is part of the authority, before any
	// path, query, or fragment separator — not an email-like string
	// inside a path.
	if slash := strings.IndexAny(rest, "/?#"); slash >= 0 && at > slash {
		return raw
	}
	return scheme + "://" + Marker + "@" + rest[at+1:]
}
