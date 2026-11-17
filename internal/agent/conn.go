package agent

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync"
)

// trackedConn is the upgraded connection. Reads drain the handshake's
// buffered reader first, since the peer may have sent HTTP/2 bytes right
// behind the handshake. Done is closed exactly once, when the connection
// is closed by either HTTP/2 side or by the registry.
type trackedConn struct {
	net.Conn
	r    *bufio.Reader
	once sync.Once
	done chan struct{}
}

func newTrackedConn(c net.Conn, r *bufio.Reader) *trackedConn {
	return &trackedConn{Conn: c, r: r, done: make(chan struct{})}
}

func (c *trackedConn) Read(p []byte) (int, error) {
	if c.r != nil {
		if c.r.Buffered() > 0 {
			return c.r.Read(p)
		}
		c.r = nil
	}
	return c.Conn.Read(p)
}

func (c *trackedConn) Close() error {
	err := net.ErrClosed
	c.once.Do(func() {
		err = c.Conn.Close()
		close(c.done)
	})
	return err
}

// Done is closed when the connection is closed.
func (c *trackedConn) Done() <-chan struct{} { return c.done }

// dialOnce hands out the upgraded connection to the first dial and fails
// every later one: there is exactly one connection to an agent, and a
// transport that lost it must not get another. A later dial means the
// transport can no longer use the connection for new requests (the agent
// sent GOAWAY, or it died), so it calls gone, once.
func dialOnce(c net.Conn, gone func()) func(context.Context, string, string) (net.Conn, error) {
	var mu sync.Mutex
	used, told := false, false
	return func(context.Context, string, string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		if used {
			if !told && gone != nil {
				told = true
				go gone()
			}
			return nil, errAgentGone
		}
		used = true
		return c, nil
	}
}

var errAgentGone = errors.New("agent connection closed")

// oneConnListener serves a single connection: Accept returns it once,
// then blocks until the connection or the listener is closed, and then
// reports the listener closed, which ends http.Server.Serve without
// touching the connection (a graceful shutdown drains it separately).
type oneConnListener struct {
	c      *trackedConn
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newOneConnListener(c *trackedConn) *oneConnListener {
	l := &oneConnListener{c: c, ch: make(chan net.Conn, 1), closed: make(chan struct{})}
	l.ch <- c
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	default:
	}
	select {
	case <-l.c.Done():
	case <-l.closed:
	}
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.c.LocalAddr() }

// goAwayConn watches the HTTP/2 frames an agent sends for GOAWAY, which
// it sends when it starts a graceful detach, and calls onGoAway once,
// synchronously in the read path (so it must not block), before the
// connection can be seen to close. The
// edge then stops routing to the attachment before any request finds the
// connection closed to new streams. Only the 9-byte frame headers are
// read (RFC 9113 §4.1: 24-bit length, type, flags, stream id); payloads
// are skipped, never interpreted. The edge is the HTTP/2 client, so every
// byte it reads is a server frame.
type goAwayConn struct {
	*trackedConn
	onGoAway func()
	seen     bool
	hdr      [9]byte
	hdrLen   int // header bytes collected so far
	skip     int // payload bytes left in the current frame
}

const frameTypeGoAway = 0x7

func (c *goAwayConn) Read(p []byte) (int, error) {
	n, err := c.trackedConn.Read(p)
	if !c.seen {
		c.scan(p[:n])
	}
	return n, err
}

func (c *goAwayConn) scan(b []byte) {
	for len(b) > 0 && !c.seen {
		if c.skip > 0 {
			k := min(c.skip, len(b))
			c.skip -= k
			b = b[k:]
			continue
		}
		k := copy(c.hdr[c.hdrLen:], b)
		c.hdrLen += k
		b = b[k:]
		if c.hdrLen < len(c.hdr) {
			return
		}
		c.hdrLen = 0
		c.skip = int(c.hdr[0])<<16 | int(c.hdr[1])<<8 | int(c.hdr[2])
		if c.hdr[3] == frameTypeGoAway {
			c.seen = true
			c.onGoAway()
		}
	}
}
