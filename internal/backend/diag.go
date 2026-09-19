package backend

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
)

// Bounds on the extracted diagnostic. maxDetailBytes is the whole
// budget; the per-field caps sum to it, so the total needs no second
// truncation pass. Message gets the largest share because it is the
// only field that carries an operator-actionable sentence.
const (
	maxDetailType    = 64
	maxDetailCode    = 64
	maxDetailMessage = 256

	// MaxMessageBytes is the most text a relayed backend message can
	// add to a client-facing error. It is the one bound that leaves
	// this package: the proxy appends a Detail's message to its own,
	// and that growth needs a stated ceiling.
	MaxMessageBytes = maxDetailMessage

	maxDetailBytes = maxDetailType + maxDetailCode + maxDetailMessage

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

// errorFields is the scalar triple, wherever it sits. Only these three
// keys are decoded; every other member of the object is skipped by the
// decoder and never materialized.
type errorFields struct {
	Type    json.RawMessage `json:"type"`
	Code    json.RawMessage `json:"code"`
	Message json.RawMessage `json:"message"`
}

// upstreamDetail extracts the sanitized diagnostic from a backend error
// body. Everything about it fails closed: a body that is not JSON, is
// not an object, carries neither recognized shape, or is larger than the
// parse bound yields the zero Detail. Only the three named keys are
// decoded, so unrelated response fields — including secret-shaped ones a
// backend might echo — cannot reach a log or a client by construction.
//
// Two shapes are recognized. OpenAI nests the triple under `error`.
// vLLM and SGLang have also shipped a flat ErrorResponse whose triple
// sits at the top level next to `object: "error"`; that marker is what
// makes the flat form safe to read, since without it the three keys
// would be claimed from any JSON object a backend happened to return.
func upstreamDetail(body []byte) Detail {
	if len(body) == 0 || len(body) > maxDetailParseBytes {
		return Detail{}
	}
	var envelope struct {
		Object string       `json:"object"`
		Error  *errorFields `json:"error"`
		errorFields
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Detail{}
	}
	f := envelope.Error
	if f == nil {
		if envelope.Object != "error" {
			return Detail{}
		}
		f = &envelope.errorFields
	}
	return Detail{
		Type:    scalarField(f.Type, maxDetailType),
		Code:    scalarField(f.Code, maxDetailCode),
		Message: scalarField(f.Message, maxDetailMessage),
	}
}

// scalarField renders one JSON scalar as bounded, printable text.
// Numbers and booleans are accepted because backends spell `code` both
// ways (SGLang sends the HTTP status as a number where OpenAI sends a
// string). Objects, arrays, and null yield "": a nested structure is
// exactly the arbitrary backend JSON that must not be relayed.
//
// The dispatch is on the first byte rather than on a decoded value, so
// a nested structure in a scalar slot is rejected without ever being
// materialized — a backend must not be able to make the diagnostic
// allocate its whole body to decide it is unusable. A number is carried
// through as the text the backend wrote: re-formatting a float64 turns
// 1e300 into 301 digits that then truncate into a different number.
func scalarField(raw json.RawMessage, limit int) string {
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return sanitizeDetailText(s, limit)
	case 't', 'f':
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return ""
		}
		return strconv.FormatBool(b)
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		// Already validated as a JSON number by the outer decode, so
		// the raw text needs only bounding. A number long enough to
		// need truncating would truncate into a different number, so
		// it is dropped instead.
		if len(raw) > limit {
			return ""
		}
		return string(raw)
	default: // null, object, array
		return ""
	}
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
		case c == '\t', c == '\n', c == '\r', c == '\v', c == '\f', c == 0x85:
			// Separators become a space rather than vanishing: a
			// backend that form-feeds between two sentences must not
			// hand the operator one glued-together word.
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
