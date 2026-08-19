package version

import "runtime"

const Name = "mellomting"

var (
	Version = "0.0.0-dev"
	Commit  = "unknown"
)

func String() string {
	return Name + " " + Version + " (commit " + Commit + ", " + runtime.Version() + ")"
}
