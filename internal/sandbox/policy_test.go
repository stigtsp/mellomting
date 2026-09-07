package sandbox

import (
	"strings"
	"testing"
)

func TestBackendPortsExplicit(t *testing.T) {
	// Sorted, de-duplicated.
	ports, err := BackendPorts("http://127.0.0.1:8002", "http://127.0.0.1:8001", "http://10.0.0.5:8001")
	if err != nil {
		t.Fatalf("BackendPorts: %v", err)
	}
	want := []uint16{8001, 8002}
	if len(ports) != len(want) {
		t.Fatalf("ports = %v, want %v", ports, want)
	}
	for i := range want {
		if ports[i] != want[i] {
			t.Fatalf("ports = %v, want %v", ports, want)
		}
	}
}

func TestBackendPortsSchemeDefaults(t *testing.T) {
	ports, err := BackendPorts("http://127.0.0.1", "https://127.0.0.1:443")
	if err != nil {
		t.Fatalf("BackendPorts: %v", err)
	}
	want := []uint16{80, 443}
	if len(ports) != len(want) {
		t.Fatalf("ports = %v, want %v", ports, want)
	}
	for i := range want {
		if ports[i] != want[i] {
			t.Fatalf("ports = %v, want %v", ports, want)
		}
	}
}

func TestBackendPortsErrors(t *testing.T) {
	cases := []string{
		"",               // empty
		"127.0.0.1:8001", // no scheme
		"ftp://127.0.0.1:21",
		"http://127.0.0.1:99999", // port out of range
		"http://127.0.0.1:0",     // port 0
		"http://[::1",            // malformed
	}
	for _, raw := range cases {
		if _, err := BackendPorts(raw); err == nil {
			t.Errorf("BackendPorts(%q): expected error, got nil", raw)
		}
	}
}

// A BackendPorts error names what is wrong, never the URL it was given:
// on validated configuration userinfo cannot occur, and a direct caller
// must not have the credential echoed back at it either.
func TestBackendPortsErrorNeverEchoesUserinfo(t *testing.T) {
	for _, raw := range []string{
		"http://user:secret@127.0.0.1:99999", // port mismatch
		"http://user:secret@127.0.0.1:0",     // port mismatch
		"ftp://user:secret@127.0.0.1:8001",   // bad scheme
	} {
		_, err := BackendPorts(raw)
		if err == nil {
			t.Fatalf("BackendPorts(%q): expected error", raw)
		}
		if strings.Contains(err.Error(), "user:secret") {
			t.Errorf("BackendPorts(%q) leaked userinfo: %v", raw, err)
		}
	}
}

func TestPolicySummarize(t *testing.T) {
	pol := Policy{
		ReadPaths:  []string{"/etc/mellomting/users.yaml"},
		WriteFiles: []string{"/var/log/mellomting/usage.jsonl"},
		ConnectTCP: []uint16{8001, 8002},
		Listen:     Listener{UnixPath: "/run/mellomting/mellomting.sock"},
	}
	sum := pol.Summarize()
	if len(sum) != 5 {
		t.Fatalf("summarize = %v", sum)
	}
	joined := strings.Join(sum, " | ")
	for _, want := range []string{
		"read /etc/mellomting/users.yaml",
		"write /var/log/mellomting/usage.jsonl",
		// The listener the daemon keeps answering on is part of what
		// the policy grants, so the operational log has to name it.
		"accept /run/mellomting/mellomting.sock",
		"connect tcp 8001",
		"connect tcp 8002",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("summary missing %q: %s", want, joined)
		}
	}
}
