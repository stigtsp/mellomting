package term

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// wide lists the East Asian Wide and Fullwidth ranges and the emoji
// presentation blocks: two cells each. Compact rather than complete; a
// rune it misses renders one cell wider than measured, never narrower.
var wide = [][2]rune{
	{0x1100, 0x115f}, {0x231a, 0x231b}, {0x2329, 0x232a}, {0x23e9, 0x23ec}, {0x23f0, 0x23f0},
	{0x23f3, 0x23f3}, {0x25fd, 0x25fe}, {0x2614, 0x2615}, {0x2648, 0x2653}, {0x267f, 0x267f},
	{0x2693, 0x2693}, {0x26a1, 0x26a1}, {0x26aa, 0x26ab}, {0x26bd, 0x26be}, {0x26c4, 0x26c5},
	{0x26ce, 0x26ce}, {0x26d4, 0x26d4}, {0x26ea, 0x26ea}, {0x26f2, 0x26f3}, {0x26f5, 0x26f5},
	{0x26fa, 0x26fa}, {0x26fd, 0x26fd}, {0x2705, 0x2705}, {0x270a, 0x270b}, {0x2728, 0x2728},
	{0x274c, 0x274c}, {0x274e, 0x274e}, {0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797},
	{0x27b0, 0x27b0}, {0x27bf, 0x27bf}, {0x2b1b, 0x2b1c}, {0x2b50, 0x2b50}, {0x2b55, 0x2b55},
	{0x2e80, 0x303e}, {0x3041, 0x33ff}, {0x3400, 0x4dbf}, {0x4e00, 0x9fff}, {0xa000, 0xa4cf},
	{0xa960, 0xa97f}, {0xac00, 0xd7a3}, {0xf900, 0xfaff}, {0xfe10, 0xfe19}, {0xfe30, 0xfe6f},
	{0xff00, 0xff60}, {0xffe0, 0xffe6}, {0x1f004, 0x1f004}, {0x1f0cf, 0x1f0cf}, {0x1f18e, 0x1f18e},
	{0x1f191, 0x1f19a}, {0x1f200, 0x1f2ff}, {0x1f300, 0x1f64f}, {0x1f680, 0x1f6ff}, {0x1f900, 0x1f9ff},
	{0x1fa70, 0x1faff}, {0x20000, 0x2fffd}, {0x30000, 0x3fffd},
}

// RuneWidth is the cells r occupies: 0 for combining and format
// characters, 2 for wide ones, 1 otherwise.
func RuneWidth(r rune) int {
	if r < 0x300 {
		return 1
	}
	if unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf) {
		return 0
	}
	lo, hi := 0, len(wide)
	for lo < hi {
		m := (lo + hi) / 2
		switch {
		case r < wide[m][0]:
			hi = m
		case r > wide[m][1]:
			lo = m + 1
		default:
			return 2
		}
	}
	return 1
}

// Width is the cells s occupies.
func Width(s string) int {
	n := 0
	for _, r := range s {
		n += RuneWidth(r)
	}
	return n
}

// Truncate cuts s to at most cells, ending in "…" when it cut. A wide
// rune that would straddle the edge is left out whole.
func Truncate(s string, cells int) string {
	if cells <= 0 {
		return ""
	}
	if Width(s) <= cells {
		return s
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := RuneWidth(r)
		if used+w > cells-1 {
			break
		}
		b.WriteRune(r)
		used += w
	}
	b.WriteRune('…')
	return b.String()
}

// Pad truncates or space-pads s to exactly cells.
func Pad(s string, cells int) string {
	s = Truncate(s, cells)
	return s + strings.Repeat(" ", max(cells-Width(s), 0))
}

// PadLeft is Pad aligned right.
func PadLeft(s string, cells int) string {
	s = Truncate(s, cells)
	return strings.Repeat(" ", max(cells-Width(s), 0)) + s
}

// Sanitize makes text from a client, a backend or an agent safe to
// print: escape sequences, C0 and C1 controls, DEL and bidirectional
// overrides are dropped, and invalid UTF-8 becomes U+FFFD.
func Sanitize(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == 0x1b:
			i += escapeLen(s[i:])
			continue
		case r == 0x9b: // C1 CSI: drop its parameters too
			i += n + escapeLen("\x1b["+s[i+n:]) - 2
			continue
		case r == utf8.RuneError && n == 1:
			b.WriteRune(utf8.RuneError)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
		case (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069):
		default:
			b.WriteRune(r)
		}
		i += n
	}
	return b.String()
}

// escapeLen is the length of the escape sequence at the start of s:
// CSI to its final byte, OSC to BEL or ST, else ESC and one byte.
func escapeLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	switch s[1] {
	case '[':
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return len(s)
	case ']', 'P', '_', '^':
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return len(s)
	}
	return 2
}
