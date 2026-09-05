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
		// A userinfo holding '/', '?' or '#' puts the first authority
		// separator inside the credentials, so the split above lands in
		// the middle of them. Returning the value verbatim here handed
		// the password to whoever reads the operator error, which is the
		// one thing this helper exists to prevent.
		{"https://svc:aB3/xY9@10.0.0.5:8001/v1", "https://<redacted>@10.0.0.5:8001/v1"},
		{"https://svc:p/w@10.0.0.5:8001", "https://<redacted>@10.0.0.5:8001"},
		{"https://user:pass?tok@host", "https://<redacted>@host"},
		{"https://user:pass#tok@host", "https://<redacted>@host"},
		// A password containing '@' must not survive in the tail: the
		// last '@' of the authority delimits the userinfo, not the first.
		{"http://user:p@ss@host/v1", "http://<redacted>@host/v1"},
		// Without a scheme separator there is no authority to isolate,
		// so anything before an '@' is treated as credentials.
		{"user:pass@host", "<redacted>@host"},
	} {
		if got := URL(tc.in); got != tc.want {
			t.Errorf("URL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
