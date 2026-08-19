//go:build linux

package landlock

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// Check queries the kernel for the highest supported Landlock ABI using
// the read-only LANDLOCK_CREATE_RULESET_VERSION flag: no ruleset is
// created and no restriction is applied.
//
// The ABI number comes from the same syscall the pinned go-landlock
// release uses (PLAN §54).
func Check() Report {
	r1, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return Report{
			Platform: runtime.GOOS,
			Reason:   "kernel does not accept Landlock (" + errno.Error() + ")",
		}
	}
	return Report{
		Platform:  runtime.GOOS,
		Supported: true,
		KernelABI: int(r1),
	}
}
