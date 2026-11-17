package agent

import (
	"net"
	"sync/atomic"
	"testing"
)

func frame(typ byte, payload int) []byte {
	b := []byte{byte(payload >> 16), byte(payload >> 8), byte(payload), typ, 0, 0, 0, 0, 0}
	return append(b, make([]byte, payload)...)
}

func TestGoAwayScanner(t *testing.T) {
	var stream []byte
	stream = append(stream, frame(0x4, 18)...)    // SETTINGS
	stream = append(stream, frame(0x0, 70000)...) // DATA, payload over 64 KiB
	stream = append(stream, frame(0x6, 8)...)     // PING
	stream = append(stream, frame(0x7, 8)...)     // GOAWAY
	stream = append(stream, frame(0x7, 8)...)     // a second one is not reported again
	for _, chunk := range []int{1, 3, 7, 1024, len(stream)} {
		var calls atomic.Int32
		done := make(chan struct{}, 2)
		c := &goAwayConn{onGoAway: func() { calls.Add(1); done <- struct{}{} }}
		for b := stream; len(b) > 0; {
			k := min(chunk, len(b))
			c.scan(b[:k])
			b = b[k:]
		}
		<-done
		if calls.Load() != 1 {
			t.Fatalf("chunk %d: onGoAway called %d times", chunk, calls.Load())
		}
	}
	// No GOAWAY, no call: a GOAWAY type byte inside a payload is not a frame.
	payload := frame(0x0, 16)
	payload[9+3] = 0x7
	c := &goAwayConn{onGoAway: func() { t.Error("payload byte mistaken for a GOAWAY frame") }}
	c.scan(payload)
	_ = net.ErrClosed
}
