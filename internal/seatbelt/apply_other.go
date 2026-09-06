//go:build !darwin

package seatbelt

import (
	"fmt"
	"runtime"

	"mellomting/internal/sandbox"
)

// Check reports Seatbelt as unavailable off macOS (PLAN §7: a build on a
// platform without the sandbox MUST say so).
func Check() sandbox.Report {
	return sandbox.Report{
		Platform: runtime.GOOS,
		Backend:  Backend,
		Reason:   "Seatbelt is a macOS-only facility",
	}
}

// Apply reports that Seatbelt enforcement is impossible on this
// platform. It never silently succeeds: a caller in required mode must
// treat this as a startup failure (PLAN §55, §57).
func Apply(_ sandbox.Policy) error {
	return fmt.Errorf("seatbelt is a macOS-only facility; it is not available on this platform")
}
