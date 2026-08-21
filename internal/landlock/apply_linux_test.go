//go:build linux

package landlock_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/landlock-lsm/go-landlock/landlock/lltest"
	"mellomting/internal/landlock"
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
func TestAllThreadsEnforced(t *testing.T) {
	lltest.RunInSubprocess(t, func() {
		// ABI 6 is the configured default minimum (PLAN §55): it gives
		// TCP connect (ABI 4) and scoped IPC (ABI 6), enough to prove
		// filesystem, network, and signal confinement. On ABI 8+ the
		// all-thread TSYNC path is exercised; below it go-landlock's
		// all-thread prctl/restrict-self sequence is.
		lltest.RequireABI(t, landlock.DefaultMinimumABI)
		report := landlock.Check()
		abi := report.KernelABI
		if abi > landlock.MaxABI {
			abi = landlock.MaxABI
		}

		dir := lltest.TempDir(t)
		allowed := filepath.Join(dir, "allowed.jsonl")
		secret := filepath.Join(dir, "secret.txt")
		for _, p := range []string{allowed, secret} {
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

		pol := landlock.Policy{
			WriteFiles: []string{allowed},
			ConnectTCP: []uint16{uint16(portOf(t, allowedLn))},
		}

		// Controls: without a sandbox the process owns these files and
		// both loopback listeners are reachable. This proves the
		// denials below are caused by the sandbox, not by unrelated
		// permissions.
		if err := openForbiddenWrite(t, secret); err != nil {
			t.Fatalf("control: pre-sandbox write access expected, got %v", err)
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

		// 4. TCP (PLAN §60): the allowed port stays reachable; the
		// denied port refuses the connect even though a listener is
		// present.
		if err := dialTCP(allowedAddr); err != nil {
			t.Fatalf("connect to allowed port %s failed: %v", allowedAddr, err)
		}
		if err := dialTCP(deniedAddr); !isLandlockDenial(err) {
			t.Fatalf("connect to denied port %s: expected Landlock denial, got %v", deniedAddr, err)
		}

		// 5. Scoped IPC (PLAN §62): signalling a process outside the
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
