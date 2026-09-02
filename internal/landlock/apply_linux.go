//go:build linux

package landlock

import (
	"fmt"

	ll "github.com/landlock-lsm/go-landlock/landlock"
	llsys "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// abiPresets maps an ABI version to the go-landlock configuration that
// restricts the full set of operations available at that version
// (PLAN §54, §55). The table is closed: an ABI above the pinned
// release's maximum is refused rather than silently guessed at.
var abiPresets = map[int]ll.Config{
	1: ll.V1,
	2: ll.V2,
	3: ll.V3,
	4: ll.V4,
	5: ll.V5,
	6: ll.V6,
	7: ll.V7,
	8: ll.V8,
	9: ll.V9,
}

// Apply enforces the policy on every thread of this process
// (PLAN §56). It is strictly fail-closed: when the selected ABI cannot
// express the policy it returns an error instead of degrading
// (PLAN §55 step 5). It never uses the library's BestEffort()
// downgrade path, which can degrade all the way to no protection
// (PLAN §55 step 5, §57 step 17); the serve mode gate decides whether
// to enforce or to abort.
//
// All-thread semantics (PLAN §56): on ABI 8+ the kernel's
// LANDLOCK_FLAG_RESTRICT_SELF_TSYNC restricts every current runtime
// thread atomically, and threads the Go runtime spawns afterwards
// inherit the confined domain at clone time. Below ABI 8 go-landlock
// falls back to its all-thread prctl/restrict sequence.
func Apply(abi int, pol Policy) error {
	cfg, ok := abiPresets[abi]
	if !ok {
		return fmt.Errorf("unsupported Landlock ABI %d (pinned library covers 1..%d)", abi, MaxABI)
	}

	// Minimal post-startup rights (PLAN §58). No execute rights are
	// granted anywhere; the policy denies everything else by
	// construction of the V-n preset. AccessFSMakeReg is deliberately
	// absent: the accounting writer only appends to an existing file
	// (O_APPEND), so the right to *create* files is not needed.
	readFile := ll.AccessFSSet(llsys.AccessFSReadFile)
	writeFile := ll.AccessFSSet(llsys.AccessFSWriteFile)

	var rules []ll.Rule
	for _, f := range pol.ReadFiles {
		rules = append(rules, ll.PathAccess(readFile, f))
	}
	for _, f := range pol.WriteFiles {
		rules = append(rules, ll.PathAccess(writeFile, f))
	}
	for _, p := range pol.ConnectTCP {
		rules = append(rules, ll.ConnectTCP(p))
	}

	// Restrict applies the filesystem, network, and scoped-IPC
	// restrictions of the chosen preset to all threads (PLAN §62:
	// scoped signal/abstract-UDS restrictions take effect on ABI 6+).
	return cfg.Restrict(rules...)
}
