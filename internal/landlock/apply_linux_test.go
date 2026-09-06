//go:build linux

package landlock_test

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/landlock-lsm/go-landlock/landlock/lltest"
	"mellomting/internal/landlock"
	"mellomting/internal/sandbox"
)

// TestAllThreadsEnforced is the PLAN §56 acceptance test, plus the
// filesystem (PLAN §58), TCP (PLAN §60), and scoped-IPC (PLAN §62)
// checks of the confined daemon.
//
// It runs in a subprocess (lltest.RunInSubprocess) because applying a
// Landlock policy is irreversible for the confined process; the
// subprocess keeps the rest of the test suite unconstrained.
//
// Sequence: set up files and listeners and prove the forbidden
// operations are possible WITHOUT a sandbox (controls); pin goroutines
// across OS threads; apply the policy to all threads; then every
// forbidden operation attempted from every thread must fail, the
// allowed operations must succeed, and a signal to a process outside
// the domain must be denied.
// requireABIOrFail enforces the configured minimum ABI. In ordinary test
// runs an insufficient kernel skips (lltest.RequireABI); when the CI job
// sets MELLOMTING_LANDLOCK_STRICT=1 an ABI >= minimum kernel is expected
// and any skip becomes a failure, so the acceptance test can never
// silently not run (FIX-02/X2 of FIX_REVIEW_2026-08-22).
func requireABIOrFail(t *testing.T, minABI int) {
	t.Helper()
	if os.Getenv("MELLOMTING_LANDLOCK_STRICT") != "" {
		report := landlock.Check()
		if !report.Supported {
			t.Fatalf("strict landlock run: sandbox unsupported: %s", report.Reason)
		}
		if report.KernelABI < minABI {
			t.Fatalf("strict landlock run: kernel ABI %d < required %d: acceptance test would have skipped", report.KernelABI, minABI)
		}
		return
	}
	lltest.RequireABI(t, minABI)
}

func TestAllThreadsEnforced(t *testing.T) {
	lltest.RunInSubprocess(t, func() {
		// ABI 6 is the configured default minimum (PLAN §55): it gives
		// TCP connect (ABI 4) and scoped IPC (ABI 6), enough to prove
		// filesystem, network, and signal confinement. On ABI 8+ the
		// all-thread TSYNC path is exercised; below it go-landlock's
		// all-thread prctl/restrict-self sequence is.
		requireABIOrFail(t, landlock.DefaultMinimumABI)
		report := landlock.Check()
		abi := report.KernelABI
		if abi > landlock.MaxABI {
			abi = landlock.MaxABI
		}

		dir := lltest.TempDir(t)
		allowed := filepath.Join(dir, "allowed.jsonl")
		users := filepath.Join(dir, "users.yaml")
		secret := filepath.Join(dir, "secret.txt")
		for _, p := range []string{allowed, users, secret} {
			if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		// Two loopback listeners: the policy allows connects to the
		// first port only. Both listeners exist, so a refusal of a
		// connect cannot be confused with a Landlock denial.
		allowedLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		deniedLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer allowedLn.Close()
		defer deniedLn.Close()
		allowedAddr := allowedLn.Addr().String()
		deniedAddr := deniedLn.Addr().String()

		pol := sandbox.Policy{
			ReadPaths:  []string{users},
			WriteFiles: []string{allowed},
			ConnectTCP: []uint16{uint16(portOf(t, allowedLn))},
		}

		// Controls: without a sandbox the process owns these files and
		// both loopback listeners are reachable. This proves the
		// denials below are caused by the sandbox, not by unrelated
		// permissions. The controls also cover the §82 failure list that
		// is expressed with fresh temp files (read, /tmp write, create,
		// exec, bind) so each post-sandbox denial has a proven-possible
		// pre-sandbox counterpart.
		if err := openForbiddenWrite(t, secret); err != nil {
			t.Fatalf("control: pre-sandbox write access expected, got %v", err)
		}
		if err := readFile(t, secret); err != nil {
			t.Fatalf("control: pre-sandbox read access expected, got %v", err)
		}
		if err := readFile(t, users); err != nil {
			t.Fatalf("control: pre-sandbox read access expected, got %v", err)
		}
		if err := writeTmp(t); err != nil {
			t.Fatalf("control: pre-sandbox write to /tmp expected, got %v", err)
		}
		if err := createFile(t, filepath.Join(dir, "new.txt")); err != nil {
			t.Fatalf("control: pre-sandbox file creation expected, got %v", err)
		}
		if err := execSh(t); err != nil {
			t.Fatalf("control: pre-sandbox exec expected, got %v", err)
		}
		if err := bindTCP(t, reservePort(t)); err != nil {
			t.Fatalf("control: pre-sandbox TCP bind expected, got %v", err)
		}
		if err := dialTCP(deniedAddr); err != nil {
			t.Fatalf("control: pre-sandbox connect expected, got %v", err)
		}

		// PLAN §56: pinned, active goroutines across OS threads. Each
		// worker busy-spins (yields with Gosched) until released so the
		// Go runtime pins it to a distinct OS thread.
		const workers = 4
		var spin, done atomic.Bool
		// Each worker reports its forbidden-write and allowed-write
		// results as one unit so the reader can pair them without
		// depending on channel interleaving across workers. Workers
		// push right after spin (not after done): done only releases
		// their pinned OS thread once the main thread has collected
		// and verified every result.
		results := make(chan [2]error, workers+2)
		for range workers {
			go func() {
				runtime.LockOSThread()
				for !spin.Load() {
					runtime.Gosched()
				}
				results <- [2]error{openForbiddenWrite(t, secret), appendFile(t, allowed)}
				for !done.Load() {
					runtime.Gosched()
				}
				runtime.UnlockOSThread()
			}()
		}

		if err := landlock.Apply(abi, pol); err != nil {
			t.Fatalf("apply: %v", err)
		}
		spin.Store(true)

		// Threads started AFTER the confinement: they must inherit the
		// confined domain at clone time (PLAN §56).
		for range 2 {
			go func() {
				results <- [2]error{openForbiddenWrite(t, secret), nil}
			}()
		}

		// 1. Every pinned thread: the forbidden write must be denied,
		// the allowed write must succeed.
		for range workers {
			pair := <-results
			if !isLandlockDenial(pair[0]) {
				t.Fatalf("pinned thread: write to %q: expected Landlock denial, got %v", secret, pair[0])
			}
			if pair[1] != nil {
				t.Fatalf("pinned thread: write to %q failed: %v", allowed, pair[1])
			}
		}
		// 2. Threads created after the confinement: the forbidden
		// write must be denied.
		for range 2 {
			pair := <-results
			if !isLandlockDenial(pair[0]) {
				t.Fatalf("post-apply thread: write to %q: expected Landlock denial, got %v", secret, pair[0])
			}
		}
		// Release the pinned workers; they exit and unlock their
		// threads after collecting the remaining results.
		done.Store(true)

		// 3. Main thread: the policy holds here as well.
		if err := openForbiddenWrite(t, secret); !isLandlockDenial(err) {
			t.Fatalf("main thread: write to %q: expected Landlock denial, got %v", secret, err)
		}
		if err := appendFile(t, allowed); err != nil {
			t.Fatalf("main thread: write to %q failed: %v", allowed, err)
		}

		// 4. Read confinement (PLAN §58, §82): a file not in the
		// ReadPaths grant (the auth.pepper / backend-secret /
		// unrelated-home / /etc/shadow analogue) is unreadable, while
		// the granted users file stays readable for a SIGHUP reload
		// (PLAN §30).
		if err := readFile(t, secret); !isLandlockDenial(err) {
			t.Fatalf("read of %q: expected Landlock denial, got %v", secret, err)
		}
		if err := readFile(t, users); err != nil {
			t.Fatalf("read of %q (SIGHUP reload) failed: %v", users, err)
		}

		// 5. Remaining §82 failure modes: writing /tmp, creating a
		// fresh file (no AccessFSMakeReg), executing /bin/sh (no
		// execute right), and binding a new TCP listener (no TCP bind
		// right) are all denied.
		if err := writeTmp(t); !isLandlockDenial(err) {
			t.Fatalf("write to /tmp: expected Landlock denial, got %v", err)
		}
		if err := createFile(t, filepath.Join(dir, "new.txt")); !isLandlockDenial(err) {
			t.Fatalf("create file: expected Landlock denial, got %v", err)
		}
		if err := execSh(t); !isLandlockDenial(err) {
			t.Fatalf("exec /bin/sh: expected Landlock denial, got %v", err)
		}
		if err := bindTCP(t, reservePort(t)); !isLandlockDenial(err) {
			t.Fatalf("bind TCP: expected Landlock denial, got %v", err)
		}

		// 6. TCP (PLAN §60): the allowed port stays reachable; the
		// denied port refuses the connect even though a listener is
		// present.
		if err := dialTCP(allowedAddr); err != nil {
			t.Fatalf("connect to allowed port %s failed: %v", allowedAddr, err)
		}
		if err := dialTCP(deniedAddr); !isLandlockDenial(err) {
			t.Fatalf("connect to denied port %s: expected Landlock denial, got %v", deniedAddr, err)
		}

		// 7. Accept on the pre-opened listener (PLAN §57 step 10) keeps
		// working after confinement: a connection to the allowed port
		// is accepted by the listener opened before the sandbox.
		accCh := make(chan struct {
			c   net.Conn
			err error
		}, 1)
		go func() {
			c, err := allowedLn.Accept()
			accCh <- struct {
				c   net.Conn
				err error
			}{c, err}
		}()
		if err := dialTCP(allowedAddr); err != nil {
			t.Fatalf("connect for accept round-trip failed: %v", err)
		}
		a := <-accCh
		if a.err != nil {
			t.Fatalf("accept on pre-opened listener failed: %v", a.err)
		}
		if a.c == nil {
			t.Fatal("accept on pre-opened listener returned nil conn")
		}
		_ = a.c.Close()

		// 8. Scoped IPC (PLAN §62): signalling a process outside the
		// domain (the test parent) must be refused. Signal 0 performs
		// the permission check without delivering a signal.
		if err := syscall.Kill(os.Getppid(), 0); !errors.Is(err, unix.EPERM) {
			t.Fatalf("signal to out-of-domain parent: expected EPERM, got %v", err)
		}
	})
}

// portOf returns the port of a TCP listener.
func portOf(t *testing.T, ln net.Listener) int {
	t.Helper()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("listener address is not TCP")
	}
	return addr.Port
}

// openForbiddenWrite opens a file for writing; a Landlock policy that
// does not grant the right makes the open fail (PLAN §59: the check
// happens at open).
func openForbiddenWrite(t *testing.T, path string) error {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_ = f.Close()
	return nil
}

// appendFile appends one line; it must succeed for paths the policy
// grants write access to.
func appendFile(t *testing.T, path string) error {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte("line\n")); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// dialTCP dials a TCP address; success proves connect(2) was permitted.
func dialTCP(addr string) error {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

// isLandlockDenial reports whether the error is a Landlock refusal
// (LSM hook denials surface as EACCES/EPERM).
func isLandlockDenial(err error) bool {
	return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM)
}

// readFile opens a path for reading; a Landlock policy that does not
// grant read access makes the open fail.
func readFile(t *testing.T, path string) error {
	t.Helper()
	_, err := os.ReadFile(path)
	return err
}

// writeTmp creates and writes a fresh file in /tmp. With no grant for
// that path the open fails, which is how "write /tmp" (PLAN §82) is
// denied.
func writeTmp(t *testing.T) error {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "mellomting-lltest-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if _, err := f.Write([]byte("x")); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return err
	}
	_ = f.Close()
	return os.Remove(name)
}

// createFile tries to create a brand-new file (O_CREATE); the policy
// deliberately grants no AccessFSMakeReg (PLAN §58), so creation fails.
func createFile(t *testing.T, path string) error {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_ = f.Close()
	return nil
}

// execSh tries to execute /bin/sh; the policy grants no execute right
// anywhere, so execve fails with a Landlock denial.
func execSh(t *testing.T) error {
	t.Helper()
	return exec.Command("/bin/sh", "-c", "exit 0").Run()
}

// bindTCP binds a loopback TCP socket to an explicit port via the raw
// socket/bind syscalls, and reports any error. The policy grants no TCP
// bind right, so any explicit-port bind must fail with a Landlock
// denial. The raw syscall path is used deliberately: Go's net.Listen
// path is not representative of the bind(2) the Landlock hook keys off.
func bindTCP(t *testing.T, port int) error {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.Bind(fd, &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}})
}

// reservePort finds a currently-free loopback TCP port by binding :0,
// reading the assigned port, and closing the listener.
func reservePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// dialUnix dials an abstract Unix socket; success proves connect(2) to
// it was permitted.
func dialUnix(addr string) error {
	c, err := net.Dial("unix", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

// TestScopedAbstractSocket is the second half of PLAN §62: once the
// scoped-IPC restrictions of ABI 6 are active, connecting to an
// abstract Unix socket owned by an out-of-domain process is denied,
// while the confined process's own in-domain abstract sockets stay
// reachable. The helper subprocess is spawned before the sandbox is
// applied, so it is never confined and owns the out-of-domain socket.
func TestScopedAbstractSocket(t *testing.T) {
	// The helper branch re-executes the test binary and acts as the
	// out-of-domain owner of an abstract socket. It must run its work
	// before the sandbox machinery of the main test.
	if os.Getenv("LL_ABS_HELPER") == "1" {
		ln, err := net.Listen("unix", abstractSockName(os.Getpid()))
		if err != nil {
			fmt.Println("ERR " + err.Error())
			os.Exit(1)
		}
		// Self-terminate: the confined child cannot signal an
		// out-of-domain process (the very scope under test), so the
		// helper must not rely on the child for cleanup.
		_ = ln.(*net.UnixListener).SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Println("READY " + ln.Addr().String())
		for {
			c, err := ln.Accept()
			if err != nil {
				os.Exit(0)
			}
			_ = c.Close()
		}
	}

	lltest.RunInSubprocess(t, func() {
		requireABIOrFail(t, landlock.DefaultMinimumABI)
		report := landlock.Check()
		abi := report.KernelABI
		if abi > landlock.MaxABI {
			abi = landlock.MaxABI
		}

		// Spawn the out-of-domain abstract-socket owner before the
		// sandbox is applied, then read the socket name it printed.
		cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$")
		cmd.Env = append(os.Environ(), "LL_ABS_HELPER=1")
		cmd.Stderr = os.Stderr
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// Cleanup runs at the end of the test, not here: a kill issued
		// now would race the helper's startup. The confined child also
		// cannot SIGKILL the helper once the scope is active
		// (cross-domain signals are denied), so the kill is best-effort
		// and the reap is deferred to a goroutine that returns when the
		// helper self-terminates on its listener deadline.
		defer func() {
			_ = cmd.Process.Kill()
			go func() { _, _ = cmd.Process.Wait() }()
		}()
		sc := bufio.NewScanner(stdout)
		if !sc.Scan() {
			t.Fatalf("helper produced no output: %v", sc.Err())
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "READY ") {
			t.Fatalf("helper: %s", line)
		}
		sock := strings.TrimPrefix(line, "READY ")

		// Control: pre-sandbox the child can reach the abstract socket.
		if err := dialUnix(sock); err != nil {
			t.Fatalf("control: pre-sandbox connect to %q expected, got %v", sock, err)
		}

		if err := landlock.Apply(abi, sandbox.Policy{}); err != nil {
			t.Fatalf("apply: %v", err)
		}

		// The out-of-domain abstract socket must now be refused.
		if err := dialUnix(sock); !isLandlockDenial(err) {
			t.Fatalf("connect to out-of-domain abstract socket %q: expected denial, got %v", sock, err)
		}

		// An in-domain abstract socket created by the confined process
		// itself stays reachable (the scope only cuts cross-domain IPC).
		mine, err := net.Listen("unix", abstractSockName(os.Getpid()+1000))
		if err != nil {
			t.Fatalf("in-domain listen: %v", err)
		}
		defer mine.Close()
		go func() {
			c, err := mine.Accept()
			if err == nil {
				_ = c.Close()
			}
		}()
		if err := dialUnix(mine.Addr().String()); err != nil {
			t.Fatalf("connect to in-domain abstract socket: %v", err)
		}
	})
}

// abstractSockName derives a unique abstract (unnamed-filesystem) Unix
// socket name from a pid. Abstract sockets live in the kernel namespace
// keyed by name, so uniqueness across parallel tests matters.
func abstractSockName(pid int) string {
	return "@mellomting-lltest-" + strconv.Itoa(pid)
}
