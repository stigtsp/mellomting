//go:build linux

package main

import (
	"errors"
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// tcpMPTCP is linux/tcp.h TCP_MPTCP (42); x/sys does not export it.
// getsockopt(TCP_MPTCP) returns the number of MPTCP subflows, so 0 means
// a plain-TCP socket and any non-zero value means MPTCP was negotiated.
const tcpMPTCP = 42

// assertListenerNotMPTCP fails the test if the listener socket has MPTCP
// enabled. On kernels without MPTCP support the getsockopt returns
// ENOPROTOOPT/EOPNOTSUPP, which is equivalent to "cannot be MPTCP" and
// therefore also passes.
func assertListenerNotMPTCP(t *testing.T, ln net.Listener) {
	t.Helper()
	tcp, ok := ln.(*net.TCPListener)
	if !ok {
		t.Fatalf("listener is %T, want *net.TCPListener", ln)
	}
	raw, err := tcp.SyscallConn()
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
	case errors.Is(soErr, unix.EPERM) || errors.Is(soErr, unix.ENOPROTOOPT) || errors.Is(soErr, unix.EOPNOTSUPP):
		// Socket is not MPTCP: EPERM is what Go's net runtime yields
		// after SetMultipathTCP(false); the others mean no MPTCP support.
	case soErr != nil:
		t.Fatalf("TCP_MPTCP getsockopt: %v", soErr)
	case v != 0:
		t.Fatalf("listener socket has MPTCP enabled (TCP_MPTCP=%d); MPTCP must be disabled (PLAN §61, §83)", v)
	}
}

// PLAN §83 MPTCP regression, behavioural form: the TCP ingress listener
// must be plain TCP, never MPTCP. This replaces the former source grep.
func TestMPTCPListenersBehaviorallyDisabled(t *testing.T) {
	ln, err := mptcpOffListen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	assertListenerNotMPTCP(t, ln)
}
