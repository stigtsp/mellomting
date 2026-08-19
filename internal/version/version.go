package version

import (
	"fmt"
	"runtime"
)

const Name = "mellomting"

var (
	// Version is set at build time (PLAN §89):
	//
	//	-X mellemting/internal/version.Version=...
	Version = "0.0.0-dev"
	// Commit is the git commit the binary was built from.
	Commit = "unknown"
	// Date is the build date (committish ISO-8601). Release builds set
	// it; direct `go build` leaves it empty for reproducible local
	// builds.
	Date = ""
)

// String reports the binary version for `mellomting version`.
func String() string {
	if Date != "" {
		return fmt.Sprintf("%s %s (commit %s, built %s, %s)", Name, Version, Commit, Date, runtime.Version())
	}
	return fmt.Sprintf("%s %s (commit %s, %s)", Name, Version, Commit, runtime.Version())
}
