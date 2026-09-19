package backend

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
)

// Bounds on the extracted diagnostic. MaxDetailBytes is the whole
// budget; the per-field caps sum to it, so the total needs no second
// truncation pass. Message gets the largest share because it is the
// only field that carries an operator-actionable sentence.
const (
	maxDetailType    = 64
	maxDetailCode    = 64
	maxDetailMessage = 256

	// MaxDetailBytes is how much text a backend can add to anything
	// Mellomting writes about it. It is exported because the proxy may
	// append a Detail to a client-facing error, and that growth needs a
	// stated ceiling.
	MaxDetailBytes = maxDetailType + maxDetailCode + maxDetailMessage

	// maxDetailParseBytes bounds what is handed to the JSON decoder.
	// Error bodies are already capped by max_response_bytes, which is
	// 64 MiB by default — far too large to parse on every upstream
	// failure. A backend that needs more than this to say what is wrong
	// gets no diagnostic rather than a slow one.
	maxDetailParseBytes = 64 << 10
)

// Detail is the sanitized diagnostic extracted from an OpenAI-shaped
// backend error body: the three scalar fields of `error`, each bounded
// and stripped of control characters.
//
// It exists because the actionable part of a backend rejection — "only
// one system message is allowed" — was being discarded with the body,
// leaving an operator unable to tell a chat-template mismatch from an
// output-limit or context-window failure (PLAN §43). It is a diagnostic,
// not a channel: it is logged at debug level and relayed to a client
// only when the backend is explicitly configured to, because a backend's
// message can quote the request it rejected.
type Detail struct {
	Type    string
	Code    string
	Message string
}

// upstreamDetail extracts the sanitized diagnostic from a backend error
// body. Everything about it fails closed: a body that is not JSON, is
// not an object, carries no `error` object, or is larger than the parse
// bound yields the zero Detail. Only the three named keys are read, so
// unrelated response fields — including secret-shaped ones a backend
// might echo — cannot reach a log or a client by construction.
func upstreamDetail(body []byte) Detail {
	if len(body) == 0 || len(body) > maxDetailParseBytes {
		return Detail{}
	}
	// A map, not a struct: the `error` value must be an object for any
	// field to be read. A string, array, or number there fails the
	// decode and yields nothing.
	var envelope struct {
		Error map[string]json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Detail{}
	}
	return Detail{
		Type:    scalarField(envelope.Error["type"], maxDetailType),
		Code:    scalarField(envelope.Error["code"], maxDetailCode),
		Message: scalarField(envelope.Error["message"], maxDetailMessage),
	}
}

// scalarField renders one JSON scalar as bounded, printable text.
// Numbers and booleans are accepted because backends spell `code` both
// ways (SGLang sends the HTTP status as a number where OpenAI sends a
// string). Objects, arrays, and null yield "": a nested structure is
// exactly the arbitrary backend JSON that must not be relayed.
func scalarField(raw json.RawMessage, limit int) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case float64:
		s = strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		s = strconv.FormatBool(t)
	default:
		return ""
	}
	return sanitizeDetailText(s, limit)
}

// sanitizeDetailText reduces a backend's text to one printable line,
// then truncates it on a rune boundary so the result is always valid
// UTF-8.
//
// Control characters go because a backend must not be able to inject
// newlines into a structured log line or escape sequences into an
// operator's console. Format characters (Cf) go with them: they are not
// control characters to unicode.IsControl, but the bidi overrides and
// zero-width joiners among them reorder and hide text in exactly the
// terminal this is read in. The path matters because it is reachable
// from a client — a prompt can steer what the backend quotes back.
func sanitizeDetailText(s string, limit int) string {
	s = strings.Map(func(c rune) rune {
		switch {
		case c == '\t' || c == '\n' || c == '\r',
			unicode.Is(unicode.Zl, c), unicode.Is(unicode.Zp, c):
			return ' '
		case unicode.IsControl(c), unicode.Is(unicode.Cf, c):
			return -1
		}
		return c
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= limit {
		return s
	}
	// Cut at the last rune boundary at or below the byte limit.
	cut := limit
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// utf8Start reports whether b starts a UTF-8 sequence (it is not a
// continuation byte).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
