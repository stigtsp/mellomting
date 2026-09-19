package backend

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// TestUpstreamDetailExtraction covers the shapes a backend error body
// actually takes, and the shapes a hostile or broken one could take. The
// rule under test: only the three scalar fields of `error` survive, and
// each survives bounded and printable.
func TestUpstreamDetailExtraction(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
		want Detail
	}{{
		name: "sglang chat template rejection",
		body: `{"error":{"type":"BadRequestError","code":400,"message":"invalid tool schema"}}`,
		want: Detail{Type: "BadRequestError", Code: "400", Message: "invalid tool schema"},
	}, {
		name: "openai string code",
		body: `{"error":{"message":"only one system message is allowed","type":"invalid_request_error","param":null,"code":"invalid_messages"}}`,
		want: Detail{Type: "invalid_request_error", Code: "invalid_messages", Message: "only one system message is allowed"},
	}, {
		name: "malformed json yields nothing",
		body: `{"error":{"message":"truncated`,
		want: Detail{},
	}, {
		name: "empty body yields nothing",
		body: ``,
		want: Detail{},
	}, {
		name: "top-level array yields nothing",
		body: `[{"error":{"message":"hi"}}]`,
		want: Detail{},
	}, {
		name: "error as a string yields nothing",
		body: `{"error":"plain text failure"}`,
		want: Detail{},
	}, {
		name: "no error member yields nothing",
		body: `{"detail":"validation failed","object":"error"}`,
		want: Detail{},
	}, {
		// The report's explicit case: a nested structure in a scalar
		// slot is arbitrary backend JSON and must not be relayed.
		name: "object and array fields are refused",
		body: `{"error":{"type":["a","b"],"code":{"n":1},"message":{"text":"nested"}}}`,
		want: Detail{},
	}, {
		name: "null fields are refused",
		body: `{"error":{"type":null,"code":null,"message":null}}`,
		want: Detail{},
	}, {
		name: "boolean code is rendered",
		body: `{"error":{"code":true,"message":"x"}}`,
		want: Detail{Code: "true", Message: "x"},
	}, {
		// A backend that echoes credentials in unrelated members must
		// not get them into a log: only the three keys are read.
		name: "unrelated secret-shaped fields are dropped",
		body: `{"error":{"message":"bad request","api_key":"sk-live-deadbeef"},"authorization":"Bearer sk-live-deadbeef","request":{"messages":[{"role":"user","content":"my prompt"}]}}`,
		want: Detail{Message: "bad request"},
	}, {
		name: "control characters are stripped and whitespace collapsed",
		body: `{"error":{"message":"line one\nline two\u0000\u001b[31mred\u001b[0m\ttab"}}`,
		want: Detail{Message: "line one line two[31mred[0m tab"},
	}, {
		// A bidi override reorders everything after it in a terminal,
		// and zero-width characters hide text outright. Neither is a
		// control character to unicode.IsControl.
		name: "bidi and zero-width characters are stripped",
		body: "{\"error\":{\"message\":\"safe\u202etxet neddih\u202c\u200bhidden\ufeff\"}}",
		want: Detail{Message: "safetxet neddihhidden"},
	}, {
		name: "line and paragraph separators become spaces",
		body: "{\"error\":{\"message\":\"a\u2028b\u2029c\"}}",
		want: Detail{Message: "a b c"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := upstreamDetail([]byte(tc.body))
			if got != tc.want {
				t.Fatalf("upstreamDetail() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestUpstreamDetailOversizedBodyIsNotParsed proves the parse bound is a
// bound and not a truncation: a body past it yields no diagnostic at
// all, so a backend cannot make every failed request pay to decode
// megabytes of JSON.
func TestUpstreamDetailOversizedBodyIsNotParsed(t *testing.T) {
	t.Parallel()

	pad := strings.Repeat("p", maxDetailParseBytes)
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": "real message", "pad": pad},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= maxDetailParseBytes {
		t.Fatalf("test body (%d bytes) does not exceed the parse bound (%d)", len(body), maxDetailParseBytes)
	}
	if got := upstreamDetail(body); got != (Detail{}) {
		t.Fatalf("oversized body produced a diagnostic: %#v", got)
	}

	// The same body just under the bound is parsed normally, so the
	// test above fails for the size and not for the shape.
	small, err := json.Marshal(map[string]any{"error": map[string]any{"message": "real message"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := upstreamDetail(small); got.Message != "real message" {
		t.Fatalf("in-bound body produced %#v, want the message", got)
	}
}

// TestUpstreamDetailFieldsAreBounded checks every field cap and the
// total budget they sum to, on input designed to blow past all of them.
func TestUpstreamDetailFieldsAreBounded(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("A", 4096)
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{"type": long, "code": long, "message": long},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := upstreamDetail(body)
	if len(got.Type) != maxDetailType || len(got.Code) != maxDetailCode || len(got.Message) != maxDetailMessage {
		t.Fatalf("field lengths = %d/%d/%d, want %d/%d/%d",
			len(got.Type), len(got.Code), len(got.Message),
			maxDetailType, maxDetailCode, maxDetailMessage)
	}
	if total := len(got.Type) + len(got.Code) + len(got.Message); total > MaxDetailBytes {
		t.Fatalf("total diagnostic = %d bytes, exceeds %d", total, MaxDetailBytes)
	}
}

// TestUpstreamDetailTruncatesOnRuneBoundary proves a multi-byte message
// cut at the byte cap stays valid UTF-8 — a mangled final rune would
// corrupt the JSON log line the diagnostic is written into.
func TestUpstreamDetailTruncatesOnRuneBoundary(t *testing.T) {
	t.Parallel()

	// "μ" is two bytes, so a 256-byte cap lands mid-rune for some
	// prefix lengths; prepending one ASCII byte walks the cut across
	// the boundary.
	for _, prefix := range []string{"", "x"} {
		body, err := json.Marshal(map[string]any{
			"error": map[string]any{"message": prefix + strings.Repeat("μ", 400)},
		})
		if err != nil {
			t.Fatal(err)
		}
		got := upstreamDetail(body).Message
		if !utf8.ValidString(got) {
			t.Fatalf("prefix %q: truncated message is not valid UTF-8: %q", prefix, got)
		}
		if len(got) > maxDetailMessage {
			t.Fatalf("prefix %q: message = %d bytes, exceeds %d", prefix, len(got), maxDetailMessage)
		}
	}
}

// FuzzUpstreamDetail asserts the invariants that make the diagnostic
// safe to log, against arbitrary bytes: bounded, printable, valid UTF-8,
// single-line.
func FuzzUpstreamDetail(f *testing.F) {
	f.Add([]byte(`{"error":{"type":"BadRequestError","code":400,"message":"invalid tool schema"}}`))
	f.Add([]byte(`{"error":{"message":{"nested":true}}}`))
	f.Add([]byte(`{"error":`))
	f.Add([]byte(`{"error":{"message":"\u0000\u001b"}}`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, body []byte) {
		d := upstreamDetail(body)
		for name, got := range map[string]string{"type": d.Type, "code": d.Code, "message": d.Message} {
			if !utf8.ValidString(got) {
				t.Fatalf("%s is not valid UTF-8: %q", name, got)
			}
			for _, r := range got {
				if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) ||
					unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
					t.Fatalf("%s carries %U, which does not survive sanitization: %q", name, r, got)
				}
			}
		}
		if len(d.Type) > maxDetailType || len(d.Code) > maxDetailCode || len(d.Message) > maxDetailMessage {
			t.Fatalf("unbounded field: %#v", d)
		}
		if total := len(d.Type) + len(d.Code) + len(d.Message); total > MaxDetailBytes {
			t.Fatalf("total diagnostic = %d bytes, exceeds %d", total, MaxDetailBytes)
		}
	})
}
