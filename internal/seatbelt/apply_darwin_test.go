//go:build darwin

package seatbelt

import (
	"bufio"
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

	// The daemon binds its listener before confining itself and must
	// keep accepting afterwards, so the child is driven for both
	// listener kinds the configuration offers.
	for _, kind := range []string{"unix", "tcp"} {
		t.Run(kind+" listener", func(t *testing.T) {
			// A unix socket path is capped near 104 bytes, which a
			// macOS temporary directory alone can exceed.
			short, err := os.MkdirTemp("/tmp", "sb")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(short)

			cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$")
			cmd.Env = append(os.Environ(),
				"MELLOMTING_SEATBELT_CHILD=1",
				"SB_LISTEN_KIND="+kind,
				"SB_LISTEN_UNIX="+filepath.Join(short, "s.sock"),
				"SB_GRANTED_DIR="+granted,
				"SB_WITHHELD_FILE="+withheld,
				"SB_GRANTED_PORT="+portOf(t, grantedLn),
				"SB_WITHHELD_PORT="+portOf(t, withheldLn),
			)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Wait()

			var got strings.Builder
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				line := scanner.Text()
				got.WriteString(line + "\n")
				addr, ok := strings.CutPrefix(line, "LISTENING ")
				if !ok {
					continue
				}
				// The confined child is now waiting to accept. If the
				// sandbox filtered the accept, this connection hangs
				// until the child's own deadline and it reports so.
				network := "unix"
				if kind == "tcp" {
					network = "tcp"
				}
				c, err := net.DialTimeout(network, addr, 5*time.Second)
				if err != nil {
					t.Fatalf("dialling the confined listener: %v", err)
				}
				buf := make([]byte, 16)
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, err := c.Read(buf)
				c.Close()
				if err != nil || string(buf[:n]) != "served" {
					t.Fatalf("confined listener did not serve the connection: %q %v", buf[:n], err)
				}
			}
			out := got.String()
			if strings.Contains(out, "APPLY-REFUSED") {
				if os.Getenv("MELLOMTING_SEATBELT_STRICT") != "" {
					t.Fatalf("strict seatbelt run: enforcement could not be tested: %s", out)
				}
				t.Skipf("this process is already sandboxed, so it may not apply another profile: %s", out)
			}
			for _, want := range []string{
				"RUNTIME-ALIVE",
				"ACCEPTED",
				"GRANTED-READ ok",
				"WITHHELD-READ denied",
				"GRANTED-CONNECT ok",
				"WITHHELD-CONNECT denied",
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q in confined child output:\n%s", want, out)
				}
			}
		})
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

	// Bind before confining, exactly as the daemon does: the policy
	// names a listener that already exists (PLAN §57).
	var ln net.Listener
	var err error
	listen := sandbox.Listener{}
	if os.Getenv("SB_LISTEN_KIND") == "unix" {
		path := os.Getenv("SB_LISTEN_UNIX")
		if ln, err = net.Listen("unix", path); err != nil {
			fmt.Println("LISTEN-FAILED", err)
			os.Exit(1)
		}
		listen.UnixPath = path
	} else {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			fmt.Println("LISTEN-FAILED", err)
			os.Exit(1)
		}
		listen.TCPPort = uint16(ln.Addr().(*net.TCPAddr).Port)
	}

	if err := Apply(sandbox.Policy{
		ReadPaths:  []string{grantedDir},
		ConnectTCP: []uint16{grantedPort},
		Listen:     listen,
	}); err != nil {
		fmt.Println("APPLY-REFUSED", err)
		os.Exit(0)
	}

	// Accepting is the daemon's whole job, and Seatbelt filters accepts
	// as well as binds: a policy that named no listener would confine it
	// into answering nothing.
	fmt.Println("LISTENING", ln.Addr().String())
	if l, ok := ln.(interface{ SetDeadline(time.Time) error }); ok {
		_ = l.SetDeadline(time.Now().Add(10 * time.Second))
	}
	conn, err := ln.Accept()
	if err != nil {
		fmt.Println("ACCEPT-FAILED", err)
	} else {
		_, _ = conn.Write([]byte("served"))
		conn.Close()
		fmt.Println("ACCEPTED")
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

// SBPL accepts only "*" or "localhost" as the host of a network
// address, so a backend rule cannot name the destination IP. This pins
// that constraint against the real compiler: without it, narrowing the
// rule to the backend's address looks like an obvious improvement and
// produces a profile that no longer compiles.
func TestCompileRejectsANamedRemoteHost(t *testing.T) {
	requireSeatbelt(t)
	_, err := compile(`(version 1)
(deny default)
(allow network-outbound (remote ip "127.0.0.1:8001"))
`)
	if err == nil {
		t.Fatal("a literal remote host was accepted; the port-only rule could be narrowed after all")
	}
	if !strings.Contains(err.Error(), "host must be") {
		t.Fatalf("err = %v, want the grammar's complaint about the host", err)
	}
}
