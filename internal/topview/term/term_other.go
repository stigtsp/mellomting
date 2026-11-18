//go:build !linux && !darwin

package term

import (
	"errors"
	"os"
)

func newTTY(in, out *os.File) (tty, error) {
	return nil, errors.New("the interactive view needs Linux or macOS; use --once")
}

// NotifyResize does nothing where resizes are not signalled.
func NotifyResize(c chan<- os.Signal) {}

// IsTerminal is false where raw mode is unsupported.
func IsTerminal(f *os.File) bool { return false }
