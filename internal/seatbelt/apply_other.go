//go:build !darwin

package seatbelt

import (
	"errors"
	"runtime"

	"mellomting/internal/sandbox"
)

// Check reports Seatbelt as unavailable off macOS (PLAN §7: a build on a
// platform without the sandbox MUST say so).
func Check() sandbox.Report {
	return sandbox.Report{
		Platform: runtime.GOOS,
		Backend:  Backend,
		Reason:   "Seatbelt requires macOS",
	}
}

// Apply reports that Seatbelt enforcement is impossible on this
// platform. It never silently succeeds: a caller in required mode must
// treat this as a startup failure (PLAN §55, §57).
func Apply(_ sandbox.Policy) error {
	return errors.New("seatbelt requires macOS")
}
