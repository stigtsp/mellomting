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

// DefaultMinimumABI is the default configured minimum (PLAN §55).
const DefaultMinimumABI = 8

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
