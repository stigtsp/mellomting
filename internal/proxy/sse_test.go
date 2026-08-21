package proxy

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestSSEParserFinalEventFlushed is the T-X13 regression test: a cleanly
// closed stream whose final event lacks a terminating blank line must
// still deliver that event, whichever line terminator ends it (or none).
func TestSSEParserFinalEventFlushed(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, stream string }{
		{"no-trailer", `data: {"x":1}`},
		{"lf-trailer", "data: {\"x\":1}\n"},
		{"cr-trailer", "data: {\"x\":1}\r"},
		{"crlf-trailer", "data: {\"x\":1}\r\n"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			p := newSSEParser(strings.NewReader(tc.stream))
			ev, err := p.nextEvent()
			if err != nil {
				t.Fatalf("nextEvent: %v", err)
			}
			if !bytes.Contains(ev, []byte(`"x":1`)) {
				t.Fatalf("final event dropped: %q", ev)
			}
			if _, err := p.nextEvent(); err != io.EOF {
				t.Fatalf("after final event: err = %v, want io.EOF", err)
			}
		})
	}
}

// TestSSEParserCleanEOFFlushesPartialEvent asserts the stronger invariant
// (T-X13): whatever terminator ends the last line of a cleanly-closed
// stream, the final data line is never dropped.
func TestSSEParserCleanEOFFlushesPartialEvent(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"data: final",
		"data: final\n",
		"data: final\r",
		"data: final\r\n",
		"data: a\ndata: final",
		"data: a\r\ndata: final\r",
		"data: a\n\ndata: final\n",
	} {
		p := newSSEParser(strings.NewReader(s))
		var sawFinal bool
		for {
			ev, err := p.nextEvent()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("%q: %v", s, err)
			}
			if bytes.Contains(ev, []byte("data: final")) {
				sawFinal = true
			}
		}
		if !sawFinal {
			t.Fatalf("%q: final data line dropped", s)
		}
	}
}

// TestSSEParserCRTerminatorsEquivalent checks that LF, CRLF and CR line
// terminators all parse the same event (spec-legal CR termination, T-X13).
func TestSSEParserCRTerminatorsEquivalent(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"data: a\n\n", "data: a\r\n\r\n", "data: a\r\r"} {
		p := newSSEParser(strings.NewReader(s))
		ev, err := p.nextEvent()
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if data, ok := dataField(ev); !ok || data != "a" {
			t.Fatalf("%q: data = %q ok=%v", s, data, ok)
		}
		if _, err := p.nextEvent(); err != io.EOF {
			t.Fatalf("%q: after event err = %v, want io.EOF", s, err)
		}
	}
}

// FuzzSSEParser feeds arbitrary bytes to the parser (PLAN §80, T-X13):
// it must never panic, only surface bounded framing errors, never emit an
// empty event, and stay idempotent after a clean EOF.
func FuzzSSEParser(f *testing.F) {
	f.Add([]byte("data: a\n\n"))
	f.Add([]byte("data: a\r\n\r\n"))
	f.Add([]byte("data: a\r\r"))
	f.Add([]byte("data: a"))
	f.Add([]byte("data: [DONE]"))
	f.Add([]byte("event: x\rdata: y\n\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		p := newSSEParser(bytes.NewReader(data))
		for {
			ev, err := p.nextEvent()
			if err == io.EOF {
				break
			}
			if err != nil {
				if !errors.Is(err, ErrSSEEventTooLarge) && !errors.Is(err, ErrSSELineTooLarge) {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if len(ev) == 0 {
				t.Fatal("empty event emitted")
			}
		}
		if _, err := p.nextEvent(); err != io.EOF {
			t.Fatalf("post-EOF nextEvent = %v, want io.EOF", err)
		}
	})
}
