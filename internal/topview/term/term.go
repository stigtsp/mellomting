// Package term is the terminal layer under `mellomting top`: raw mode,
// key decoding, cell-width text, a diffing frame buffer and braille
// graphs. It is small on purpose; PLAN §88 rules out a TUI framework.
package term

import (
	"io"
	"os"
	"sync"
)

// tty is the terminal's mode and size; a fake stands in for tests.
type tty interface {
	makeRaw() error
	restore() error
	size() (w, h int, err error)
}

// Session holds the terminal in raw mode on the alternate screen.
type Session struct {
	tty  tty
	out  io.Writer
	once sync.Once
}

// Start puts the terminal on in into raw mode and switches out to the
// alternate screen with the cursor hidden. Restore undoes both; call
// it on every exit path, including a panic (Guard).
func Start(in, out *os.File) (*Session, error) {
	t, err := newTTY(in, out)
	if err != nil {
		return nil, err
	}
	return start(t, out)
}

func start(t tty, out io.Writer) (*Session, error) {
	if err := t.makeRaw(); err != nil {
		return nil, err
	}
	s := &Session{tty: t, out: out}
	if _, err := io.WriteString(out, "\x1b[?1049h\x1b[?25l\x1b[2J"); err != nil {
		s.Restore()
		return nil, err
	}
	return s, nil
}

// Restore leaves the alternate screen, shows the cursor and restores the
// terminal mode. Only the first call does anything.
func (s *Session) Restore() {
	s.once.Do(func() {
		_, _ = io.WriteString(s.out, "\x1b[?2026l\x1b[0m\x1b[?25h\x1b[?1049l")
		_ = s.tty.restore()
	})
}

// Guard runs fn, and on a panic restores the terminal before letting
// the panic continue, so its trace prints on a usable terminal.
func (s *Session) Guard(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.Restore()
			panic(r)
		}
	}()
	fn()
}

// Size is the terminal's size in cells, 80×24 when unknown.
func (s *Session) Size() (w, h int) {
	w, h, err := s.tty.size()
	if err != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return w, h
}
