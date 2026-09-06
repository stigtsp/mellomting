package seatbelt

import (
	"strings"
	"testing"

	"mellomting/internal/sandbox"
)

func testPolicy() sandbox.Policy {
	return sandbox.Policy{
		ReadPaths:  []string{"/etc/mellomting"},
		WriteFiles: []string{"/var/log/mellomting/usage.jsonl"},
		ConnectTCP: []uint16{8001, 8002},
		Listen:     sandbox.Listener{UnixPath: "/run/mellomting/mellomting.sock"},
	}
}

// The profile must deny before it grants: SBPL takes the last matching
// rule, so a blanket deny placed after the grants would revoke them all
// and confine the daemon into doing nothing.
func TestProfileDeniesBeforeGranting(t *testing.T) {
	p := Profile(testPolicy())
	deny := strings.Index(p, "(deny default)")
	if deny < 0 {
		t.Fatal("profile has no default deny")
	}
	for _, grant := range []string{"(allow file-read*", "(allow network-outbound", "(allow network-inbound"} {
		if i := strings.Index(p, grant); i < deny {
			t.Fatalf("%s is granted before the default deny, which revokes it:\n%s", grant, p)
		}
	}
	if !strings.HasPrefix(p, "(version 1)\n") {
		t.Fatalf("profile must open with its version:\n%s", p)
	}
}

func TestProfileGrantsThePolicy(t *testing.T) {
	p := Profile(testPolicy())
	for _, want := range []string{
		`(import "bsd.sb")`,
		`(allow file-read* (subpath "/etc/mellomting"))`,
		`(allow file-write-data file-write-flags (literal "/var/log/mellomting/usage.jsonl"))`,
		`(allow network-inbound (local unix-socket (path-literal "/run/mellomting/mellomting.sock")))`,
		`(allow file-write-unlink (literal "/run/mellomting/mellomting.sock"))`,
		`(allow network-outbound (remote tcp "*:8001"))`,
		`(allow network-outbound (remote tcp "*:8002"))`,
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("profile missing %s:\n%s", want, p)
		}
	}
}

// A TCP listener is granted by port; a unix listener by path. Naming
// neither would leave the daemon unable to accept a connection.
func TestProfileGrantsTheTCPListener(t *testing.T) {
	pol := testPolicy()
	pol.Listen = sandbox.Listener{TCPPort: 8080}
	p := Profile(pol)
	if !strings.Contains(p, `(allow network-inbound (local ip "*:8080"))`) {
		t.Fatalf("TCP listener not granted:\n%s", p)
	}
	if strings.Contains(p, "unix-socket") {
		t.Fatalf("TCP listener must not grant a unix socket:\n%s", p)
	}
}

// A pathname is embedded as an SBPL string literal. An unescaped quote
// would close the literal early and silently change which paths the
// profile grants.
func TestProfileEscapesPaths(t *testing.T) {
	p := Profile(sandbox.Policy{ReadPaths: []string{`/etc/we"ird\path`}})
	if !strings.Contains(p, `(subpath "/etc/we\"ird\\path")`) {
		t.Fatalf("pathname not escaped:\n%s", p)
	}
}

// An empty policy still denies everything, and grants nothing beyond
// the imported baseline.
func TestProfileEmptyPolicyGrantsNothing(t *testing.T) {
	p := Profile(sandbox.Policy{})
	if strings.Contains(p, "(allow ") {
		t.Fatalf("empty policy granted something:\n%s", p)
	}
}
