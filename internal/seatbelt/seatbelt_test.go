package seatbelt

import (
	"fmt"
	"os"
	"path/filepath"
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
		`(allow file-write-data (literal "/var/log/mellomting/usage.jsonl"))`,
		`(allow network-inbound (local unix-socket (path-literal "/run/mellomting/mellomting.sock")))`,
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
// The listener grant is accept-only: a policy whose point is to shrink
// what a compromised process can do must not hand it the right to
// delete files for a cleanup the next start performs anyway.
func TestProfileDoesNotGrantSocketRemoval(t *testing.T) {
	p := Profile(testPolicy())
	if strings.Contains(p, "file-write-unlink") {
		t.Fatalf("profile grants socket removal:\n%s", p)
	}
}

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

// macOS resolves /etc, /var and /tmp into /private, and Seatbelt matches
// the path the kernel evaluates rather than the one the operator wrote.
// A policy naming only the configured form would compile, apply, and
// then deny the daemon its own users file.
func TestProfileGrantsBothPathForms(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	// On macOS even a temporary directory sits under /var, which is
	// itself a link into /private, so the resolved form is what the
	// kernel will evaluate.
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	// Every kind of path in the policy goes through the same
	// resolution, and the ones this is really about — the accounting
	// log under /var, the socket under /var or /tmp — are the write
	// and listener grants, not the read.
	// Both exist by the time the daemon builds its policy: the log is
	// open and the socket is bound before the sandbox is applied. An
	// unresolvable path falls back to the configured spelling, which
	// would quietly hide the very substitution under test.
	socket := filepath.Join(link, "s.sock")
	logFile := filepath.Join(link, "usage.jsonl")
	for _, f := range []string{socket, logFile} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := Profile(sandbox.Policy{
		ReadPaths:  []string{link},
		WriteFiles: []string{logFile},
		Listen:     sandbox.Listener{UnixPath: socket},
	})
	for _, want := range []struct{ rule, path string }{
		{`(allow file-read* (subpath "%s"))`, link},
		{`(allow file-read* (subpath "%s"))`, resolved},
		{`(allow file-write-data (literal "%s"))`, logFile},
		{`(allow file-write-data (literal "%s"))`, filepath.Join(resolved, "usage.jsonl")},
		{`(allow network-inbound (local unix-socket (path-literal "%s")))`, socket},
		{`(allow network-inbound (local unix-socket (path-literal "%s")))`, filepath.Join(resolved, "s.sock")},
	} {
		if rule := fmt.Sprintf(want.rule, want.path); !strings.Contains(p, rule) {
			t.Fatalf("profile is missing %s\n%s", rule, p)
		}
	}
}

// A path that does not resolve is granted as written: that is the name
// the daemon will open.
func TestProfileGrantsUnresolvablePathAsWritten(t *testing.T) {
	p := Profile(sandbox.Policy{ReadPaths: []string{"/no/such/directory/here"}})
	if !strings.Contains(p, `(subpath "/no/such/directory/here")`) {
		t.Fatalf("profile dropped an unresolvable path:\n%s", p)
	}
}
