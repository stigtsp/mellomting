//go:build !linux

package landlock

import (
	"runtime"

	"mellomting/internal/sandbox"
)

// Check reports Landlock as unavailable on non-Linux platforms (PLAN §7:
// non-Linux builds MUST clearly report that Landlock is unavailable).
func Check() sandbox.Report {
	return sandbox.Report{
		Platform: runtime.GOOS,
		Backend:  Backend,
		Reason:   "Landlock is a Linux-only kernel feature",
	}
}
