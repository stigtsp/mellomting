package redact

import "testing"

// TestURL merges the tables that the two former copies pinned
// separately: config covered the credential cases, landlock covered the
// '@'-in-path guard, and neither covered the other's.
func TestURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// Credentials in the authority are replaced wholesale, not just
		// the password.
		{"http://user:pass@127.0.0.1:8001", "http://<redacted>@127.0.0.1:8001"},
		{"http://admin:pass@host\x7f", "http://<redacted>@host\x7f"},
		{"https://token@example.com/v1", "https://<redacted>@example.com/v1"},
		// An '@' after the authority is part of the path and must not be
		// mistaken for userinfo: redacting here would fabricate a
		// credential marker where none exists.
		{"http://127.0.0.1/a@b", "http://127.0.0.1/a@b"},
		{"http://127.0.0.1/p?q=a@b", "http://127.0.0.1/p?q=a@b"},
		{"http://127.0.0.1/p#a@b", "http://127.0.0.1/p#a@b"},
		// Nothing to redact, or nothing parseable.
		{"http://127.0.0.1:8001", "http://127.0.0.1:8001"},
		{"", ""},
		{"not-a-url", "not-a-url"},
		{"user:pass@host", "user:pass@host"},
	} {
		if got := URL(tc.in); got != tc.want {
			t.Errorf("URL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
