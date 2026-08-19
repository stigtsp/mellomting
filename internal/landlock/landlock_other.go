//go:build !linux

package landlock

import (
	"runtime"
)

// Check reports Landlock as unavailable on non-Linux platforms (PLAN §7:
// non-Linux builds MUST clearly report that Landlock is unavailable).
func Check() Report {
	return Report{
		Platform: runtime.GOOS,
		Reason:   "Landlock is a Linux-only kernel feature",
	}
}
