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

// TestSSEParserEventsTerminatedExceptFinal asserts the T-Q8 invariant:
// every event emitted before clean EOF carries its own terminating blank
// line; only the final flush of a cleanly-closed stream may lack one
// (T-X13). A parser bug that emitted a mid-stream event without its
// terminator (the deleted unreachable `have` fast path) would trip it.
func TestSSEParserEventsTerminatedExceptFinal(t *testing.T) {
	t.Parallel()
	cases := []string{
		"data: a\n\ndata: b\n\n",
		"data: a\r\n\r\ndata: b\r\n\r\n",
		"data: a\r\rdata: b\r\r",
		"data: a\n\ndata: b",
		"data: a\ndata: b\n\n",
		"data: a\r\ndata: b\r\r",
		"data: a\n\n\ndata: b\n\n",
	}
	for _, s := range cases {
		p := newSSEParser(strings.NewReader(s))
		var events [][]byte
		for {
			ev, err := p.nextEvent()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("%q: %v", s, err)
			}
			events = append(events, ev)
		}
		if len(events) == 0 {
			t.Fatalf("%q: no events", s)
		}
		for i, ev := range events {
			if i < len(events)-1 && !bytes.HasSuffix(ev, []byte("\n\n")) {
				t.Fatalf("%q: event %d lacks its terminating blank line: %q", s, i, ev)
			}
		}
	}
}

// T-T7: the aggregated event bound (ErrSSEEventTooLarge) and the
// per-line bound (ErrSSELineTooLarge) are distinct and each must be
// exercised. Many short data lines whose aggregate exceeds maxSSEEvent
// must trip the event bound (not the line bound); a single line beyond
// maxSSELine must trip the line bound.
func TestSSEParserEventBound(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	for i := 0; i < 20; i++ {
		buf.WriteString("data: ")
		buf.WriteString(strings.Repeat("a", 64*1024))
		buf.WriteString("\n")
	}
	p := newSSEParser(bytes.NewReader(buf.Bytes()))
	_, err := p.nextEvent()
	if !errors.Is(err, ErrSSEEventTooLarge) {
		t.Fatalf("aggregated event: err = %v, want ErrSSEEventTooLarge", err)
	}
	if errors.Is(err, ErrSSELineTooLarge) {
		t.Fatal("aggregated event tripped the line bound, not the event bound")
	}

	p2 := newSSEParser(strings.NewReader(strings.Repeat("a", maxSSELine+1) + "\n\n"))
	if _, err := p2.nextEvent(); !errors.Is(err, ErrSSELineTooLarge) {
		t.Fatalf("single oversized line: err = %v, want ErrSSELineTooLarge", err)
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
