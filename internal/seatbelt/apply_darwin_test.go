//go:build darwin

package seatbelt

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"mellomting/internal/sandbox"
)

// requireSeatbelt skips when this host cannot compile a profile at all.
// MELLOMTING_SEATBELT_STRICT turns the skip into a failure, so a run
// that is meant to prove enforcement cannot pass by skipping.
func requireSeatbelt(t *testing.T) {
	t.Helper()
	if r := Check(); !r.Supported {
		if os.Getenv("MELLOMTING_SEATBELT_STRICT") != "" {
			t.Fatalf("strict seatbelt run: %s", r.Reason)
		}
		t.Skipf("seatbelt unavailable: %s", r.Reason)
	}
}

// The profile the daemon would really apply must compile on this
// machine. Compiling does not confine anything, so this runs anywhere
// macOS exposes the sandbox compiler — including inside another
// sandbox, where applying is refused.
func TestGeneratedProfileCompiles(t *testing.T) {
	requireSeatbelt(t)
	for _, tc := range []struct {
		name string
		pol  sandbox.Policy
	}{
		{"unix listener", testPolicy()},
		{"tcp listener", sandbox.Policy{
			ReadPaths:  []string{"/etc/mellomting"},
			ConnectTCP: []uint16{8001},
			Listen:     sandbox.Listener{TCPPort: 8080},
		}},
		{"no accounting", sandbox.Policy{
			ReadPaths: []string{"/etc/mellomting"},
			Listen:    sandbox.Listener{UnixPath: "/run/mellomting/x.sock"},
		}},
		{"empty", sandbox.Policy{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := compile(Profile(tc.pol))
			if err != nil {
				t.Fatalf("the daemon's own profile does not compile: %v\n%s", err, Profile(tc.pol))
			}
			c.free()
		})
	}
}

// A profile the system cannot express is a startup error, never a
// half-applied policy.
func TestCompileRejectsAnUnknownOperation(t *testing.T) {
	requireSeatbelt(t)
	_, err := compile("(version 1)\n(deny default)\n(allow no-such-operation)\n")
	if err == nil {
		t.Fatal("an unknown sandbox operation was accepted")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("err = %v, want it to say the profile was rejected", err)
	}
	// The reason is one line: libsandbox appends a parser backtrace that
	// would otherwise land in an operator log.
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("error must be a single line for the log: %q", err)
	}
}

// TestApplyEnforces proves the policy is actually enforced, not merely
// accepted: a granted read succeeds, an ungranted one is refused, a
// granted backend port connects, an ungranted one does not — and the Go
// runtime keeps working under the confinement.
//
// Applying a Seatbelt profile is irreversible and confines the whole
// process, so the work happens in a re-executed child. The child is
// also refused by a macOS that is already running this process under a
// sandbox of its own, which is why the parent reports that as a skip.
func TestApplyEnforces(t *testing.T) {
	if os.Getenv("MELLOMTING_SEATBELT_CHILD") == "1" {
		confinedChild()
		return
	}
	requireSeatbelt(t)

	dir := t.TempDir()
	granted := filepath.Join(dir, "granted")
	if err := os.MkdirAll(granted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(granted, "users.yaml"), []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withheld := filepath.Join(dir, "withheld.pepper")
	if err := os.WriteFile(withheld, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	grantedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer grantedLn.Close()
	withheldLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer withheldLn.Close()
	go acceptAll(grantedLn)
	go acceptAll(withheldLn)

	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$")
	cmd.Env = append(os.Environ(),
		"MELLOMTING_SEATBELT_CHILD=1",
		"SB_GRANTED_DIR="+granted,
		"SB_WITHHELD_FILE="+withheld,
		"SB_GRANTED_PORT="+portOf(t, grantedLn),
		"SB_WITHHELD_PORT="+portOf(t, withheldLn),
	)
	out, err := cmd.CombinedOutput()
	got := string(out)
	if strings.Contains(got, "APPLY-REFUSED") {
		if os.Getenv("MELLOMTING_SEATBELT_STRICT") != "" {
			t.Fatalf("strict seatbelt run: enforcement could not be tested: %s", got)
		}
		t.Skipf("this process is already sandboxed, so it may not apply another profile: %s", got)
	}
	if err != nil {
		t.Fatalf("confined child failed: %v\n%s", err, got)
	}
	for _, want := range []string{
		"RUNTIME-ALIVE",
		"GRANTED-READ ok",
		"WITHHELD-READ denied",
		"GRANTED-CONNECT ok",
		"WITHHELD-CONNECT denied",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in confined child output:\n%s", want, got)
		}
	}
}

// confinedChild applies the policy to itself and reports what it can
// still do. It prints rather than asserts: the parent owns the verdict.
func confinedChild() {
	port := func(name string) uint16 {
		n, err := strconv.ParseUint(os.Getenv(name), 10, 16)
		if err != nil {
			fmt.Println("BAD-PORT", name, err)
			os.Exit(1)
		}
		return uint16(n)
	}
	grantedDir := os.Getenv("SB_GRANTED_DIR")
	withheldFile := os.Getenv("SB_WITHHELD_FILE")
	grantedPort, withheldPort := port("SB_GRANTED_PORT"), port("SB_WITHHELD_PORT")

	if err := Apply(sandbox.Policy{
		ReadPaths:  []string{grantedDir},
		ConnectTCP: []uint16{grantedPort},
	}); err != nil {
		fmt.Println("APPLY-REFUSED", err)
		os.Exit(0)
	}

	// The runtime has to survive the confinement: a sandbox that kills
	// the scheduler or the allocator is not a usable one.
	done := make(chan int, 1)
	go func() { done <- len(make([]byte, 1<<20)) }()
	if <-done == 1<<20 && time.Now().Year() > 2000 {
		fmt.Println("RUNTIME-ALIVE")
	}

	if _, err := os.ReadFile(filepath.Join(grantedDir, "users.yaml")); err == nil {
		fmt.Println("GRANTED-READ ok")
	} else {
		fmt.Println("GRANTED-READ FAILED", err)
	}
	if _, err := os.ReadFile(withheldFile); err != nil {
		fmt.Println("WITHHELD-READ denied")
	} else {
		fmt.Println("WITHHELD-READ LEAKED")
	}

	dial := func(p uint16) error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(int(p)), 3*time.Second)
		if err == nil {
			c.Close()
		}
		return err
	}
	if err := dial(grantedPort); err == nil {
		fmt.Println("GRANTED-CONNECT ok")
	} else {
		fmt.Println("GRANTED-CONNECT FAILED", err)
	}
	if err := dial(withheldPort); err != nil {
		fmt.Println("WITHHELD-CONNECT denied")
	} else {
		fmt.Println("WITHHELD-CONNECT LEAKED")
	}
}

func acceptAll(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Close()
	}
}

func portOf(t *testing.T, ln net.Listener) string {
	t.Helper()
	_, p, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
