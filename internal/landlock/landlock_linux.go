//go:build linux

package landlock

import (
	"runtime"

	"golang.org/x/sys/unix"

	"mellomting/internal/sandbox"
)

// Check queries the kernel for the highest supported Landlock ABI using
// the read-only LANDLOCK_CREATE_RULESET_VERSION flag: no ruleset is
// created and no restriction is applied.
//
// The ABI number comes from the same syscall the pinned go-landlock
// release uses (PLAN §54).
func Check() sandbox.Report {
	r1, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return sandbox.Report{
			Platform: runtime.GOOS,
			Backend:  Backend,
			Reason:   "kernel does not accept Landlock (" + errno.Error() + ")",
		}
	}
	return sandbox.Report{
		Platform:  runtime.GOOS,
		Backend:   Backend,
		Supported: true,
		KernelABI: int(r1),
	}
}
