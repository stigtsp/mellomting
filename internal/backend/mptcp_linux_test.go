//go:build linux

package backend

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// tcpMPTCP is linux/tcp.h TCP_MPTCP (42); x/sys does not export it.
// getsockopt(TCP_MPTCP) returns the number of MPTCP subflows, so 0 means
// a plain-TCP socket and any non-zero value means MPTCP was negotiated.
const tcpMPTCP = 42

// assertNotMPTCP fails the test if the established socket negotiated
// MPTCP. A non-zero TCP_MPTCP value means MPTCP was negotiated. The
// error signals EPERM (Go's net runtime yields this after
// SetMultipathTCP(false)), ENOPROTOOPT and EOPNOTSUPP (no MPTCP support
// on this kernel) all mean the socket is not MPTCP and therefore pass.
func assertNotMPTCP(t *testing.T, conn interface {
	SyscallConn() (syscall.RawConn, error)
}) {
	t.Helper()
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var v int
	var soErr error
	if err := raw.Control(func(fd uintptr) {
		v, soErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, tcpMPTCP)
	}); err != nil {
		t.Fatalf("Control: %v", err)
	}
	switch {
	case soErr == unix.EPERM || soErr == unix.ENOPROTOOPT || soErr == unix.EOPNOTSUPP:
		// Socket is not MPTCP (see comment above).
	case soErr != nil:
		t.Fatalf("TCP_MPTCP getsockopt: %v", soErr)
	case v != 0:
		t.Fatalf("socket is an MPTCP connection (TCP_MPTCP=%d); MPTCP must be disabled (PLAN §61, §83)", v)
	}
}

// PLAN §83 MPTCP regression, behavioural form: a connection made through
// the policyDial dialer must be plain TCP, never MPTCP. This replaces the
// former source grep with a socket-level assertion.
func TestDialerConnectionIsPlainTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()
	dialer := policyDial(Policy{Mode: "loopback-only"}, time.Second, net.DefaultResolver)
	conn, err := dialer(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial through policyDial: %v", err)
	}
	defer conn.Close()
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("dial returned %T, want *net.TCPConn", conn)
	}
	assertNotMPTCP(t, tcp)
}
