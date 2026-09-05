package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// SSE re-emission with explicit bounds (PLAN §24).
//
// Mellomting parses the SSE framing only far enough to bound each event
// and to observe response-ID events for Responses affinity (PLAN §21.2);
// every parsed event is re-emitted verbatim, so unknown events and fields
// pass through unchanged (PLAN §24). Whole-event buffering is avoided: an
// event is read, emitted, and released before the next one is read.

const (
	// maxSSELine bounds a single SSE line (PLAN §24).
	maxSSELine = 256 * 1024
	// maxSSEEvent bounds the aggregated size of one event.
	maxSSEEvent = 1024 * 1024
)

var (
	// ErrSSELineTooLarge: a single line exceeded maxSSELine.
	ErrSSELineTooLarge = errors.New("sse line too large")
	// ErrSSEEventTooLarge: an event exceeded maxSSEEvent.
	ErrSSEEventTooLarge = errors.New("sse event too large")
)

// sseParser reads one event at a time from an SSE stream.
type sseParser struct {
	br    *bufio.Reader
	line  []byte
	event []byte
	have  bool
}

func newSSEParser(r io.Reader) *sseParser {
	return &sseParser{br: bufio.NewReaderSize(r, 64<<10)}
}

// nextEvent returns the raw bytes of the next full event (including the
// terminating blank line), re-serialized verbatim. It returns io.EOF
// when the stream ends cleanly. Read errors that indicate the upstream
// stopped mid-event surface as (nil, err). A final event that lacks its
// terminating blank line is still emitted (T-X13): the upstream closed
// cleanly, so the buffered event must not be dropped.
func (p *sseParser) nextEvent() ([]byte, error) {
	for {
		line, err := p.readLine()
		if err != nil {
			if err == io.EOF {
				if len(line) > 0 {
					// A final line without any line terminator: it is
					// the last data line of a cleanly-closed stream.
					if len(p.event)+len(line)+2 > maxSSEEvent {
						p.reset()
						return nil, ErrSSEEventTooLarge
					}
					p.line = line
					p.have = true
					p.event = append(p.event, line...)
				}
				if len(p.event) > 0 {
					// Clean EOF with a buffered partial event: flush it
					// rather than discarding it, so the final data line
					// and its usage chunk are relayed (PLAN §24).
					out := append([]byte{}, p.event...)
					p.reset()
					return out, nil
				}
				return nil, io.EOF
			}
			// Real read error mid-stream: report it and never flush a
			// possibly-corrupt partial event.
			p.reset()
			return nil, err
		}
		p.line = line
		if len(p.line) == 0 {
			// A blank line terminates the event (or is a separator in
			// an empty stream). The event must include its own
			// terminating blank line to be re-emitted verbatim.
			if !p.have {
				continue
			}
			p.event = append(p.event, '\n')
			out := append([]byte{}, p.event...)
			p.reset()
			return out, nil
		}
		if len(p.event)+len(p.line)+2 > maxSSEEvent {
			p.reset()
			return nil, ErrSSEEventTooLarge
		}
		p.have = true
		p.event = append(p.event, p.line...)
		p.event = append(p.event, '\n')
	}
}

// reset drops buffered state after an error so a later retry starts clean.
func (p *sseParser) reset() {
	p.line = nil
	p.event = nil
	p.have = false
}

// readLine reads one line terminated by \n, \r, or \r\n (a trailing \r
// is dropped in all cases; a lone \r is a spec-legal CR terminator,
// T-X13). Lines are bounded by maxSSELine with a byte-wise scan. The
// final unterminated line is returned with io.EOF.
func (p *sseParser) readLine() ([]byte, error) {
	var line []byte
	for {
		b, err := p.br.ReadByte()
		if err == io.EOF {
			if len(line) == 0 {
				return nil, io.EOF
			}
			// Final line without a terminating newline.
			return line, io.EOF
		}
		if err != nil {
			if len(line) == 0 {
				return nil, err
			}
			// Mid-line read failure: report the consumed bytes as an
			// incomplete line (caller treats any error here as fatal).
			return nil, err
		}
		if b == '\n' {
			if len(line) > 0 && line[len(line)-1] == '\r' {
				return line[:len(line)-1], nil
			}
			return line, nil
		}
		if b == '\r' {
			// CR is a spec-legal line terminator. Consume a following
			// \n for CRLF, otherwise the lone CR terminates the line.
			if next, perr := p.br.Peek(1); perr == nil && next[0] == '\n' {
				if _, rerr := p.br.ReadByte(); rerr != nil {
					return line, rerr
				}
			}
			return line, nil
		}
		if len(line) >= maxSSELine {
			return nil, ErrSSELineTooLarge
		}
		line = append(line, b)
	}
}

// dataField extracts the data of an event (SSE semantics: multiple data
// lines joined with \n). Returns ok=false for an event with no data.
func dataField(event []byte) (string, bool) {
	var sb bytes.Buffer
	for line := range bytes.SplitSeq(TrimEventNewline(event), []byte("\n")) {
		l, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		// One leading space after the colon is stripped once.
		if len(l) > 0 && l[0] == ' ' {
			l = l[1:]
		}
		sb.Write(l)
	}
	if sb.Len() == 0 {
		return "", false
	}
	return sb.String(), true
}

// TrimEventNewline removes the trailing \n of the raw event bytes.
func TrimEventNewline(event []byte) []byte { return bytes.TrimRight(event, "\n") }
