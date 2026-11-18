package term

import (
	"bytes"
	"strings"
	"testing"
)

func TestDecodeKeys(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []Key
		rest string
	}{
		{"\x1b[A", []Key{{Code: Up}}, ""},
		{"\x1bOA", []Key{{Code: Up}}, ""},
		{"\x1b[B\x1b[D", []Key{{Code: Down}, {Code: Left}}, ""},
		{"\x1b[5~\x1b[6~", []Key{{Code: PgUp}, {Code: PgDn}}, ""},
		{"\x1b[H\x1b[F\x1b[1~\x1b[4~", []Key{{Code: Home}, {Code: End}, {Code: Home}, {Code: End}}, ""},
		{"\x1b[1;5C", []Key{{Code: Right}}, ""},
		{"q\r\t\x7f", []Key{{Code: Rune, Rune: 'q'}, {Code: Enter}, {Code: Tab}, {Code: Backspace}}, ""},
		{"\x03", []Key{{Code: CtrlC}}, ""},
		{"j\x1b[1;", []Key{{Code: Rune, Rune: 'j'}}, "\x1b[1;"},
		{"\x1b", nil, "\x1b"},
		{"\xe6\xbc", nil, "\xe6\xbc"},
		{"漢", []Key{{Code: Rune, Rune: '漢'}}, ""},
		{"\x1b[99X", nil, ""}, // unknown, consumed
		{"\x1bx", []Key{{Code: Esc}, {Code: Rune, Rune: 'x'}}, ""},
	} {
		got, rest := Decode([]byte(tc.in))
		if !equalKeys(got, tc.want) || string(rest) != tc.rest {
			t.Errorf("%q: got %v rest %q, want %v rest %q", tc.in, got, rest, tc.want, tc.rest)
		}
	}
	if k := Flush([]byte("\x1b")); len(k) != 1 || k[0].Code != Esc {
		t.Errorf("lone ESC after the timeout: %v", k)
	}
}

func equalKeys(a, b []Key) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSanitizeAndWidth(t *testing.T) {
	if got := Sanitize("ok\x1b[31mred\x1b]0;title\x07\u009b2J\x00\x7f\u202eend"); got != "okredend" {
		t.Errorf("sanitize: %q", got)
	}
	if got := Sanitize("bad\xffutf8"); got != "bad�utf8" {
		t.Errorf("invalid utf-8: %q", got)
	}
	for s, w := range map[string]int{"abc": 3, "漢字": 4, "e\u0301": 1, "": 0, "🙂": 2, "a\u200bb": 2} {
		if got := Width(s); got != w {
			t.Errorf("Width(%q) = %d, want %d", s, got, w)
		}
	}
	if got := Truncate("漢字漢字", 5); got != "漢字…" || Width(got) > 5 {
		t.Errorf("truncate wide: %q", got)
	}
	if got := Truncate("short", 10); got != "short" {
		t.Errorf("no cut: %q", got)
	}
	if got := Truncate("abcdef", 4); got != "abc…" {
		t.Errorf("cut: %q", got)
	}
	for _, s := range []string{"漢字漢字", "abcdef", "a漢b", ""} {
		for c := 0; c < 8; c++ {
			if got := Pad(s, c); Width(got) != c {
				t.Errorf("Pad(%q, %d) = %q width %d", s, c, got, Width(got))
			}
		}
	}
}

func TestBraille(t *testing.T) {
	got := Braille([]float64{0, 4, 8, 2}, 2, 1, 8)
	if len(got) != 1 || got[0] != "⢠⣇" {
		t.Fatalf("braille %q", got)
	}
	tall := Braille([]float64{8, 8}, 1, 2, 8)
	if tall[0] != "⣿" || tall[1] != "⣿" {
		t.Fatalf("full %q", tall)
	}
	if b := Braille([]float64{0, 0, 0}, 3, 2, 0); b[0] != "   " || b[1] != "   " {
		t.Fatalf("all zero %q", b)
	}
	if b := Braille([]float64{0.001}, 1, 1, 100); b[0] == " " {
		t.Fatal("a non-zero value too small for a dot vanished")
	}
	if b := Braille([]float64{5, 7}, 3, 1, 0); Width(b[0]) != 3 {
		t.Fatalf("auto max / right alignment %q", b)
	}
	if s := Spark([]float64{0, 1, 2, 4, 8}, 5); s != " ▁▂▄█" {
		t.Fatalf("spark %q", s)
	}
	if s, _ := Meter(0.5, 4); s != "■■··" {
		t.Fatalf("meter %q", s)
	}
	if Gradient(0) == Gradient(1) || Gradient(-1) != Gradient(0) || Gradient(2) != Gradient(1) {
		t.Fatal("gradient ends")
	}
}

func TestScreenDiff(t *testing.T) {
	a := NewScreen(20, 3)
	a.Box(0, 0, 20, 3, "title", Style{})
	a.Text(2, 1, "hello 漢字", Style{Bold: true})
	var buf bytes.Buffer
	a.Diff(nil, &buf)
	if !strings.Contains(buf.String(), "hello") || !strings.Contains(buf.String(), "title") {
		t.Fatalf("full frame %q", buf.String())
	}
	b := NewScreen(20, 3)
	b.Box(0, 0, 20, 3, "title", Style{})
	b.Text(2, 1, "hello 漢字", Style{Bold: true})
	buf.Reset()
	b.Diff(a, &buf)
	if buf.Len() != 0 {
		t.Fatalf("identical frame wrote %q", buf.String())
	}
	c := NewScreen(20, 3)
	c.Box(0, 0, 20, 3, "title", Style{})
	c.Text(2, 1, "jello 漢字", Style{Bold: true})
	buf.Reset()
	c.Diff(b, &buf)
	out := buf.String()
	if strings.Count(out, "\x1b[2;3H") != 1 || !strings.Contains(out, "j") || strings.Contains(out, "ello") {
		t.Fatalf("one-cell change wrote %q", out)
	}
	// Clipping: text past the edge and a wide rune on the last column.
	d := NewScreen(4, 1)
	if n := d.Text(2, 0, "漢字", Style{}); n != 2 {
		t.Fatalf("clipped wrote %d cells", n)
	}
	d.Text(3, 0, "漢", Style{})
	if d.cell(3, 0).r != ' ' {
		t.Fatal("half a wide rune on the edge")
	}
}

type fakeTTY struct {
	raw, restored bool
}

func (f *fakeTTY) makeRaw() error { f.raw = true; return nil }
func (f *fakeTTY) restore() error { f.restored = true; return nil }
func (f *fakeTTY) size() (int, int, error) {
	return 100, 40, nil
}

func TestSessionRestoresOnPanic(t *testing.T) {
	tty := &fakeTTY{}
	var out bytes.Buffer
	s, err := start(tty, &out)
	if err != nil || !tty.raw || !strings.Contains(out.String(), "\x1b[?1049h") || !strings.Contains(out.String(), "\x1b[?25l") {
		t.Fatalf("start: err %v raw %v out %q", err, tty.raw, out.String())
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic swallowed")
			}
		}()
		s.Guard(func() { panic("boom") })
	}()
	if !tty.restored || !strings.Contains(out.String(), "\x1b[?1049l") || !strings.Contains(out.String(), "\x1b[?25h") {
		t.Fatalf("not restored: %v %q", tty.restored, out.String())
	}
	n := out.Len()
	s.Restore()
	if out.Len() != n {
		t.Fatal("restore not idempotent")
	}
	if w, h := s.Size(); w != 100 || h != 40 {
		t.Fatalf("size %dx%d", w, h)
	}
}

func TestSessionDrawDiffs(t *testing.T) {
	var out bytes.Buffer
	s, _ := start(&fakeTTY{}, &out)
	a := NewScreen(10, 2)
	a.Text(0, 0, "x", Style{})
	s.Draw(a)
	out.Reset()
	b := NewScreen(10, 2)
	b.Text(0, 0, "x", Style{})
	s.Draw(b)
	if out.Len() != 0 {
		t.Fatalf("unchanged frame wrote %q", out.String())
	}
	s.Redraw()
	s.Draw(b)
	if !strings.Contains(out.String(), "\x1b[2J") {
		t.Fatal("Redraw did not force a full frame")
	}
}
