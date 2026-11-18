package term

import (
	"testing"
	"unicode/utf8"
)

// Whatever a client puts in a user agent or a key name, nothing that
// steers the terminal survives.
func FuzzSanitize(f *testing.F) {
	f.Add("a\x1b[31mb\u009b2Jc\x1b]0;t\x07")
	f.Fuzz(func(t *testing.T, s string) {
		out := Sanitize(s)
		if !utf8.ValidString(out) {
			t.Fatalf("invalid utf-8 %q", out)
		}
		for _, r := range out {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
				t.Fatalf("%q survived in %q", r, out)
			}
		}
		if w := Width(Truncate(out, 7)); w > 7 {
			t.Fatalf("truncated to %d cells", w)
		}
	})
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte("\x1b[1;5C\x1bOAq\xe6"))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, rest := Decode(b)
		// rest is only ever an unfinished sequence: a short tail.
		if len(rest) > 40 {
			t.Fatalf("rest %q", rest)
		}
	})
}
