//go:build linux || darwin

package term

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

type unixTTY struct {
	in, out int
	saved   *unix.Termios
}

func newTTY(in, out *os.File) (tty, error) {
	return &unixTTY{in: int(in.Fd()), out: int(out.Fd())}, nil
}

func (t *unixTTY) makeRaw() error {
	st, err := unix.IoctlGetTermios(t.in, ioctlGet)
	if err != nil {
		return err
	}
	saved := *st
	t.saved = &saved
	raw := *st
	// cfmakeraw, keeping output processing off too: every draw positions
	// the cursor itself. ISIG is off so Ctrl-C arrives as a key and the
	// terminal is restored by the normal exit path.
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	return unix.IoctlSetTermios(t.in, ioctlSet, &raw)
}

func (t *unixTTY) restore() error {
	if t.saved == nil {
		return nil
	}
	return unix.IoctlSetTermios(t.in, ioctlSet, t.saved)
}

func (t *unixTTY) size() (int, int, error) {
	ws, err := unix.IoctlGetWinsize(t.out, unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, err
	}
	return int(ws.Col), int(ws.Row), nil
}

// NotifyResize delivers a value on c each time the terminal is resized.
func NotifyResize(c chan<- os.Signal) { signal.Notify(c, syscall.SIGWINCH) }

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGet)
	return err == nil
}
