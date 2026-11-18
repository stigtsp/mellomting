package term

import (
	"bytes"
	"io"
	"strconv"
)

type cell struct {
	r  rune // 0 marks the right half of a wide rune
	st Style
}

// Screen is one frame of cells. Drawing clips at the edges.
type Screen struct {
	W, H    int
	NoColor bool // drop colours, keep bold, dim and reverse
	cells   []cell
}

// NewScreen is a blank w×h frame.
func NewScreen(w, h int) *Screen {
	s := &Screen{W: max(w, 0), H: max(h, 0)}
	s.cells = make([]cell, s.W*s.H)
	for i := range s.cells {
		s.cells[i].r = ' '
	}
	return s
}

func (s *Screen) cell(x, y int) *cell { return &s.cells[y*s.W+x] }

func (s *Screen) in(x, y int) bool { return x >= 0 && y >= 0 && x < s.W && y < s.H }

// Set draws one rune. A wide rune needs two cells; with only one left
// before the edge, a space is drawn instead.
func (s *Screen) Set(x, y int, r rune, st Style) int {
	if !s.in(x, y) {
		return 0
	}
	w := RuneWidth(r)
	if w == 0 {
		return 0
	}
	s.clearWide(x, y)
	if w == 2 {
		if x+1 >= s.W {
			*s.cell(x, y) = cell{r: ' ', st: st}
			return 1
		}
		s.clearWide(x+1, y)
		*s.cell(x, y) = cell{r: r, st: st}
		*s.cell(x+1, y) = cell{r: 0, st: st}
		return 2
	}
	*s.cell(x, y) = cell{r: r, st: st}
	return 1
}

// clearWide blanks the other half of a wide rune that (x, y) is part
// of, so overwriting half of one never leaves the other half behind.
func (s *Screen) clearWide(x, y int) {
	c := s.cell(x, y)
	if c.r == 0 && x > 0 {
		s.cell(x-1, y).r = ' '
	} else if c.r != 0 && RuneWidth(c.r) == 2 && x+1 < s.W {
		s.cell(x+1, y).r = ' '
	}
}

// Text draws s from (x, y) and returns the cells it used. It does not
// sanitize: callers pass text through Sanitize where it came from
// outside.
func (s *Screen) Text(x, y int, str string, st Style) int {
	used := 0
	for _, r := range str {
		if x+used >= s.W {
			break
		}
		used += s.Set(x+used, y, r, st)
	}
	return used
}

// Fill paints a rectangle with r.
func (s *Screen) Fill(x, y, w, h int, r rune, st Style) {
	for j := y; j < y+h; j++ {
		for i := x; i < x+w; i++ {
			s.Set(i, j, r, st)
		}
	}
}

// Box draws a rounded border with title set into the top edge.
func (s *Screen) Box(x, y, w, h int, title string, st Style) {
	if w < 2 || h < 2 {
		return
	}
	for i := x + 1; i < x+w-1; i++ {
		s.Set(i, y, '─', st)
		s.Set(i, y+h-1, '─', st)
	}
	for j := y + 1; j < y+h-1; j++ {
		s.Set(x, j, '│', st)
		s.Set(x+w-1, j, '│', st)
	}
	s.Set(x, y, '╭', st)
	s.Set(x+w-1, y, '╮', st)
	s.Set(x, y+h-1, '╰', st)
	s.Set(x+w-1, y+h-1, '╯', st)
	if title != "" && w > 6 {
		t := Truncate(title, w-6)
		s.Set(x+1, y, '┤', st)
		n := s.Text(x+2, y, t, Style{FG: st.FG, Bold: true})
		s.Set(x+2+n, y, '├', st)
	}
}

// Diff writes what changed since prev in one write: cursor moves and
// runs of changed cells, inside a synchronized update so the terminal
// shows the frame whole. A nil or differently sized prev redraws all.
// An unchanged frame writes nothing.
func (s *Screen) Diff(prev *Screen, w io.Writer) error {
	full := prev == nil || prev.W != s.W || prev.H != s.H
	var b bytes.Buffer
	var cur Style
	styled := false
	for y := range s.H {
		for x := 0; x < s.W; {
			if !full && *s.cell(x, y) == *prev.cell(x, y) {
				x++
				continue
			}
			if s.cell(x, y).r == 0 && x > 0 {
				x-- // start at the wide rune, not its right half
			}
			b.WriteString("\x1b[")
			b.WriteString(strconv.Itoa(y + 1))
			b.WriteByte(';')
			b.WriteString(strconv.Itoa(x + 1))
			b.WriteByte('H')
			for x < s.W && (full || *s.cell(x, y) != *prev.cell(x, y) || s.cell(x, y).r == 0) {
				c := s.cell(x, y)
				x++
				if c.r == 0 {
					continue
				}
				if !styled || c.st != cur {
					s.sgr(&b, c.st)
					cur, styled = c.st, true
				}
				b.WriteRune(c.r)
			}
		}
	}
	if b.Len() == 0 {
		return nil
	}
	var out bytes.Buffer
	out.WriteString("\x1b[?2026h")
	if full {
		out.WriteString("\x1b[0m\x1b[2J")
	}
	out.Write(b.Bytes())
	out.WriteString("\x1b[0m\x1b[?2026l")
	_, err := w.Write(out.Bytes())
	return err
}

func (s *Screen) sgr(b *bytes.Buffer, st Style) {
	b.WriteString("\x1b[0")
	if st.Bold {
		b.WriteString(";1")
	}
	if st.Dim {
		b.WriteString(";2")
	}
	if st.Reverse {
		b.WriteString(";7")
	}
	if !s.NoColor {
		if st.FG != 0 {
			b.WriteString(";38;5;")
			b.WriteString(strconv.Itoa(int(st.FG - 1)))
		}
		if st.BG != 0 {
			b.WriteString(";48;5;")
			b.WriteString(strconv.Itoa(int(st.BG - 1)))
		}
	}
	b.WriteByte('m')
}

// String renders the frame as plain text, one line per row, for tests
// and --once.
func (s *Screen) String() string {
	var b bytes.Buffer
	for y := range s.H {
		line := make([]rune, 0, s.W)
		for x := range s.W {
			if r := s.cell(x, y).r; r != 0 {
				line = append(line, r)
			}
		}
		b.WriteString(string(bytes.TrimRight([]byte(string(line)), " ")))
		b.WriteByte('\n')
	}
	return b.String()
}
