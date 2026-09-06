//go:build !linux

package landlock

import (
	"fmt"

	"mellomting/internal/sandbox"
)

// Apply reports that Landlock enforcement is impossible on this
// platform (PLAN §7: non-Linux builds MUST clearly report that
// Landlock is unavailable). It never silently succeeds: a caller in
// required mode must treat this as a startup failure
// (PLAN §55, §57).
func Apply(_ int, _ sandbox.Policy) error {
	return fmt.Errorf("landlock is a Linux-only kernel feature; it is not available on this platform")
}
