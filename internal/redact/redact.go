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
//
// Ambiguity is resolved towards redacting: a helper that only ever runs
// on a value suspected of carrying credentials must not answer "no
// credentials here" by returning the value verbatim.
func URL(raw string) string {
	prefix, rest := "", raw
	if scheme, after, ok := strings.Cut(raw, "://"); ok {
		prefix, rest = scheme+"://", after
	}
	// The authority ends at the first path, query, or fragment
	// separator — assuming the userinfo holds none of them.
	authority := rest
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		authority = rest[:end]
	}
	// Userinfo inside the authority. The LAST '@' delimits it, so a
	// password containing '@' does not survive in the tail.
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		return prefix + Marker + "@" + rest[at+1:]
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return raw
	}
	// No '@' in the authority but one after it: either the '@' really
	// is in the path, or the userinfo itself held a '/', '?' or '#' and
	// the split above cut through the credentials. A ':' in the
	// authority is the tell — "user:pa/ss@host" splits to "user:pa" —
	// and guessing wrong here emits the credential, so it guesses
	// closed.
	if strings.Contains(authority, ":") {
		return prefix + Marker + "@" + rest[at+1:]
	}
	return raw
}
