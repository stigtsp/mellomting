// Package discovery parses bounded OpenAI model-discovery responses
// (D7). The parser is network-free: it receives an already-bounded reader
// and fixed bounds, and it never includes response bytes in errors.
package discovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxModelIDBytes is the D7 byte bound for one discovered model ID.
const MaxModelIDBytes = 256

// ParseModels decodes the bounded D7 subset of an OpenAI /v1/models
// response and returns the model IDs sorted by UTF-8 bytes.
//
// The reader is read with at most maxBytes+1 bytes so an oversized
// response is detected without buffering more than the bound plus one.
// maxModels bounds the accepted ID list. Errors are caller-agnostic: the
// caller layer adds the server identity.
func ParseModels(r io.Reader, maxBytes int64, maxModels int) ([]string, error) {
	if maxBytes <= 0 {
		return nil, errors.New("discovery response: maxBytes must be > 0")
	}
	if maxModels <= 0 {
		return nil, errors.New("discovery response: maxModels must be > 0")
	}

	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, errors.New("discovery response: read failed")
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("discovery response exceeds %d bytes", maxBytes)
	}

	ids, err := parseModelList(bytes.NewReader(data), maxModels)
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)
	return ids, nil
}

type modelEntry struct {
	Object string          `json:"object"`
	ID     json.RawMessage `json:"id"`
}

func parseModelList(r io.Reader, maxModels int) ([]string, error) {
	dec := json.NewDecoder(r)

	tok, err := dec.Token()
	if err != nil {
		return nil, invalidJSON(err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("discovery response: top-level value must be an object")
	}

	var object string
	objectSeen := false
	dataSeen := false
	ids := make([]string, 0, maxModels)
	seen := make(map[string]struct{}, maxModels)

	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, invalidJSON(err)
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("discovery response: top-level member key must be a string")
		}

		switch key {
		case "object":
			if objectSeen {
				return nil, errors.New("discovery response: duplicate object field")
			}
			if err := dec.Decode(&object); err != nil {
				return nil, invalidJSON(err)
			}
			objectSeen = true
		case "data":
			if dataSeen {
				return nil, errors.New("discovery response: duplicate data field")
			}
			dataSeen = true
			parsed, err := parseData(dec, maxModels)
			if err != nil {
				return nil, err
			}
			for _, id := range parsed {
				if _, dup := seen[id]; dup {
					return nil, errors.New("discovery response: duplicate model ID")
				}
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, invalidJSON(err)
			}
		}
	}

	tok, err = dec.Token()
	if err != nil {
		return nil, invalidJSON(err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '}' {
		return nil, errors.New("discovery response: top-level object is not closed")
	}

	if !objectSeen || object != "list" {
		return nil, errors.New(`discovery response: top-level object must be "list"`)
	}
	if !dataSeen {
		return nil, errors.New("discovery response: data is required")
	}

	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("discovery response: exactly one JSON document is allowed")
	}

	return ids, nil
}

func parseData(dec *json.Decoder, maxModels int) ([]string, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, invalidJSON(err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, errors.New("discovery response: data must be an array")
	}

	ids := make([]string, 0, maxModels)
	for dec.More() {
		if len(ids) >= maxModels {
			return nil, fmt.Errorf("discovery response exceeds %d models", maxModels)
		}

		var entry modelEntry
		if err := dec.Decode(&entry); err != nil {
			return nil, errors.New("discovery response: data entry must be a JSON object")
		}
		if entry.Object != "model" {
			return nil, errors.New(`discovery response: data entry object must be "model"`)
		}
		id, err := decodeJSONString(entry.ID)
		if err != nil || !validModelID(id) {
			return nil, errors.New("discovery response: model ID is invalid")
		}
		ids = append(ids, id)
	}

	tok, err = dec.Token()
	if err != nil {
		return nil, invalidJSON(err)
	}
	if d, ok := tok.(json.Delim); !ok || d != ']' {
		return nil, errors.New("discovery response: data array is not closed")
	}

	return ids, nil
}

// validModelID applies the D7 model-ID rules: valid UTF-8, non-empty, at
// most MaxModelIDBytes bytes, no control/format character, no Unicode
// line or paragraph separator, and no leading or trailing Unicode
// whitespace.
func validModelID(s string) bool {
	if s == "" || len(s) > MaxModelIDBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) ||
			unicode.Is(unicode.Cf, r) ||
			unicode.Is(unicode.Zl, r) ||
			unicode.Is(unicode.Zp, r) {
			return false
		}
	}
	if first, _ := utf8.DecodeRuneInString(s); unicode.IsSpace(first) {
		return false
	}
	if last, _ := utf8.DecodeLastRuneInString(s); unicode.IsSpace(last) {
		return false
	}
	return true
}

// decodeJSONString strictly decodes one JSON string literal. Go's standard
// JSON decoder silently replaces invalid UTF-8, so discovery decodes the
// raw literal itself and fails closed on invalid UTF-8 or lone surrogates
// (D7).
func decodeJSONString(raw json.RawMessage) (string, error) {
	b := bytes.TrimSpace(raw)
	n := len(b)
	if n < 2 || b[0] != '"' || b[n-1] != '"' {
		return "", errors.New("not a JSON string")
	}

	var out strings.Builder
	i := 1
	for i < n-1 {
		c := b[i]
		if c != '\\' {
			if c < 0x20 {
				return "", errors.New("unescaped control character")
			}
			out.WriteByte(c)
			i++
			continue
		}

		if i+1 >= n-1 {
			return "", errors.New("truncated escape")
		}
		esc := b[i+1]
		switch esc {
		case '"', '\\', '/':
			out.WriteByte(esc)
			i += 2
		case 'b':
			out.WriteByte('\b')
			i += 2
		case 'f':
			out.WriteByte('\f')
			i += 2
		case 'n':
			out.WriteByte('\n')
			i += 2
		case 'r':
			out.WriteByte('\r')
			i += 2
		case 't':
			out.WriteByte('\t')
			i += 2
		case 'u':
			if i+6 > n-1 {
				return "", errors.New("truncated unicode escape")
			}
			code, err := jsonHex16(b[i+2 : i+6])
			if err != nil {
				return "", err
			}
			if code >= 0xD800 && code <= 0xDBFF {
				if i+12 > n-1 || b[i+6] != '\\' || b[i+7] != 'u' {
					return "", errors.New("lone surrogate")
				}
				low, err := jsonHex16(b[i+8 : i+12])
				if err != nil || low < 0xDC00 || low > 0xDFFF {
					return "", errors.New("invalid surrogate pair")
				}
				r := rune(0x10000 + uint32(code-0xD800)<<10 + uint32(low-0xDC00))
				out.WriteRune(r)
				i += 12
			} else if code >= 0xDC00 && code <= 0xDFFF {
				return "", errors.New("lone surrogate")
			} else {
				out.WriteRune(rune(code))
				i += 6
			}
		default:
			return "", errors.New("invalid escape")
		}
	}

	return out.String(), nil
}

func jsonHex16(b []byte) (uint16, error) {
	var v uint16
	for _, c := range b {
		var d uint16
		switch {
		case c >= '0' && c <= '9':
			d = uint16(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint16(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint16(c-'A') + 10
		default:
			return 0, errors.New("invalid unicode escape")
		}
		v = v<<4 | d
	}
	return v, nil
}

func invalidJSON(err error) error {
	return fmt.Errorf("discovery response: invalid JSON: %v", err)
}
