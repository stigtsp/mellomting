package term

import (
	"math"
	"strings"
)

// Color is a 256-colour palette index plus one; the zero Color is the
// terminal's default.
type Color uint16

// Ansi256 is palette entry n.
func Ansi256(n uint8) Color { return Color(n) + 1 }

// Style is how a cell is drawn.
type Style struct {
	FG, BG             Color
	Bold, Dim, Reverse bool
}

// Gradient is the one load colour scale, used by every graph and meter:
// green at 0, yellow at 0.5, red at 1, on the 6×6×6 colour cube.
func Gradient(f float64) Color {
	f = min(max(f, 0), 1)
	if math.IsNaN(f) {
		f = 0
	}
	r, g := 5, 5
	if f < 0.5 {
		r = int(math.Round(f * 2 * 5))
	} else {
		g = int(math.Round((1 - f) * 2 * 5))
	}
	return Ansi256(uint8(16 + 36*r + 6*g))
}

// Braille draws values as an area graph w cells wide and h high, two
// values per cell, newest at the right. top <= 0 scales to the largest
// value. A value above zero always shows at least one dot, so a trickle
// of traffic is visible. Empty cells are spaces.
func Braille(values []float64, w, h int, top float64) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	cols := make([]int, 2*w) // dot heights, right-aligned
	if len(values) > 2*w {
		values = values[len(values)-2*w:]
	}
	if top <= 0 {
		for _, v := range values {
			top = math.Max(top, v)
		}
	}
	off := 2*w - len(values)
	for i, v := range values {
		if top <= 0 || !(v > 0) {
			continue
		}
		d := int(math.Round(math.Min(v/top, 1) * float64(4*h)))
		cols[off+i] = min(max(d, 1), 4*h)
	}
	// Dot bits by row from the top, for the left and right column.
	left := [4]rune{0x01, 0x02, 0x04, 0x40}
	right := [4]rune{0x08, 0x10, 0x20, 0x80}
	out := make([]string, h)
	for row := range h {
		var b strings.Builder
		base := (h - 1 - row) * 4 // dot height at this cell's bottom
		for c := range w {
			var bits rune
			for k := range 4 {
				level := base + 3 - k
				if cols[2*c] > level {
					bits |= left[k]
				}
				if cols[2*c+1] > level {
					bits |= right[k]
				}
			}
			if bits == 0 {
				b.WriteByte(' ')
			} else {
				b.WriteRune(0x2800 + bits)
			}
		}
		out[row] = b.String()
	}
	return out
}

var sparks = []rune(" ▁▂▃▄▅▆▇█")

// Spark draws the last w values as one row of block heights.
func Spark(values []float64, w int) string {
	if len(values) > w {
		values = values[len(values)-w:]
	}
	var top float64
	for _, v := range values {
		top = math.Max(top, v)
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", w-len(values)))
	for _, v := range values {
		i := 0
		if top > 0 && v > 0 {
			i = max(1, int(math.Round(v/top*float64(len(sparks)-1))))
		}
		b.WriteRune(sparks[i])
	}
	return b.String()
}

// Meter is a w-cell bar filled to frac, and the gradient colour for it.
func Meter(frac float64, w int) (string, Color) {
	f := min(max(frac, 0), 1)
	if math.IsNaN(frac) {
		f = 0
	}
	n := int(math.Round(f * float64(w)))
	return strings.Repeat("■", n) + strings.Repeat("·", w-n), Gradient(f)
}
