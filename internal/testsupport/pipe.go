package testsupport

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// PipeServer serves an http.Handler over in-memory connections. It
// stands in for httptest.Server where binding even a loopback port is
// refused, such as under a development sandbox, and speaks real HTTP/1.1,
// so streaming, Content-Length and connection reuse behave as they do on
// a socket. It is reached only through Transport. URL is unique per
// server, so callers can tell which server a configured base URL names;
// it is a loopback literal so configuration under the default
// loopback-only egress policy accepts it, and nothing listens on it.
type PipeServer struct {
	URL       string
	Transport *http.Transport
}

// pipeBasePort and pipePorts bound the ports PipeServer URLs name.
const pipeBasePort, pipePorts = 40000, 20000

var pipeSeq atomic.Int64

// NewPipeServer starts serving h; the server stops when t ends.
func NewPipeServer(t testing.TB, h http.Handler) *PipeServer {
	t.Helper()
	ln := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	tr := &http.Transport{
		DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return ln.dial(ctx) },
		MaxIdleConns:    16,
		IdleConnTimeout: 30 * time.Second,
	}
	t.Cleanup(func() {
		tr.CloseIdleConnections()
		_ = srv.Close()
	})
	return &PipeServer{
		URL:       fmt.Sprintf("http://127.0.0.1:%d", pipeBasePort+pipeSeq.Add(1)%pipePorts),
		Transport: tr,
	}
}

// pipeListener hands the server ends of net.Pipe connections to
// http.Server.Serve.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *pipeListener) dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
	case <-ctx.Done():
	}
	_ = client.Close()
	_ = server.Close()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, net.ErrClosed
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }
