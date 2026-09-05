package landlock

import "testing"

// TestDefaultPortForScheme pins the single source of truth for
// scheme-to-port (PLAN §60, T-Q6): http is 80, https is 443, and any
// other scheme reports ok=false so the dialer and the sandbox can never
// silently agree on a port for a scheme neither understands.
func TestDefaultPortForScheme(t *testing.T) {
	cases := []struct {
		scheme string
		port   string
		ok     bool
	}{
		{"http", "80", true},
		{"https", "443", true},
		{"ftp", "", false},
		{"ws", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		port, ok := DefaultPortForScheme(tc.scheme)
		if port != tc.port || ok != tc.ok {
			t.Fatalf("DefaultPortForScheme(%q) = (%q, %v), want (%q, %v)", tc.scheme, port, ok, tc.port, tc.ok)
		}
	}
}
