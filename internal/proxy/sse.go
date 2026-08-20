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
// when the stream ends. Read errors that indicate the upstream stopped
// mid-event surface as (nil, err).
func (p *sseParser) nextEvent() ([]byte, error) {
	if p.have {
		out := append([]byte{}, p.event...)
		p.event = nil
		p.line = nil
		p.have = false
		return out, nil
	}
	for {
		line, err := p.readLine()
		if err != nil {
			if len(p.event) > 0 {
				// A partial event at EOF is malformed framing.
				return nil, err
			}
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
			p.event = nil
			p.line = nil
			p.have = false
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

// readLine reads one \n-terminated line (a trailing \r is dropped).
// Lines are bounded by maxSSELine with a byte-wise scan.
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
	for _, line := range bytes.Split(TrimEventNewline(event), []byte("\n")) {
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
