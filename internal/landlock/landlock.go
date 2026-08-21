// Package landlock reports the Landlock sandbox capability of this host
// (PLAN §53-63).
//
// Landlock is a Linux-only kernel feature. On other platforms Check
// reports it as unavailable; Mellomting never silently degrades a
// required sandbox to none (PLAN §7, §55).
package landlock

import "github.com/landlock-lsm/go-landlock/landlock"

// The pinned go-landlock release is referenced so the dependency is kept
// in the module graph (PLAN §54: "pin the dependency to a reviewed
// release"). If a future pinned release dropped the top ABI, this blank
// use would fail the build and force the re-review that PLAN §55 asks
// for. Bump MaxABI only together with a reviewed release upgrade.
var _ = landlock.V9

// MaxABI is the highest Landlock ABI supported by the pinned go-landlock
// v0.9.0 release (PLAN §54). Bump only when the pinned release is
// reviewed and upgraded.
const MaxABI = 9

// DefaultMinimumABI is the default configured minimum (PLAN §55). ABI 6
// is the floor this program needs: TCP connect (ABI 4) for backend
// dialing and scoped IPC (ABI 6) for the PLAN §62 signal/abstract-UDS
// confinement. ABI 8's TSYNC all-thread path is preferred when the
// kernel offers it, but ABI 6 kernels are still fully confined via
// go-landlock's all-thread prctl/restrict-self fallback.
const DefaultMinimumABI = 6

// DefaultPortForScheme returns the default TCP port for a URL scheme
// (PLAN §60), or ok=false for an unsupported scheme. It is the single
// source of truth for the backend dialer and the sandbox alike, so a
// scheme-to-port drift cannot let the dialer use a port the sandbox
// denies (T-Q6).
func DefaultPortForScheme(scheme string) (port string, ok bool) {
	switch scheme {
	case "http":
		return "80", true
	case "https":
		return "443", true
	}
	return "", false
}

// Sandbox modes (PLAN §55).
const (
	ModeRequired   = "required"
	ModeBestEffort = "best-effort"
	ModeDisabled   = "disabled"
)

// Report is the result of a read-only capability probe. It applies no
// policy.
type Report struct {
	// Platform is runtime.GOOS.
	Platform string
	// Supported is true when the kernel accepts Landlock.
	Supported bool
	// KernelABI is the highest Landlock ABI the kernel supports; 0 when
	// unsupported.
	KernelABI int
	// Reason explains why Landlock is unavailable when !Supported.
	Reason string
}

// Check probes the kernel for Landlock support without applying any
// restriction. It is platform-specific (see the _linux/_other files).
