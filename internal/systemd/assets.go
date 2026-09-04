// Package systemd provisions the Mellomting daemon as a systemd service
// (PLAN §64) from `mellomting install --systemd`.
//
// The unit and logrotate assets are embedded into the binary so a
// single release artifact can install itself end-to-end. The canonical
// operator-facing copies remain in repo deploy/; a sync test
// (systemd_test.go) proves the embedded copies never drift from them.
package systemd

import (
	_ "embed"
	"strings"
)

// DefaultServiceUser is the unprivileged system account the daemon runs
// under (PLAN §64) and the owner of its log/state/runtime directories.
const DefaultServiceUser = "mellomting"

// binaryPathPlaceholder is substituted by RenderService. It is never a
// path that could legitimately be installed, so a stale template is
// caught by tests and by a unit validation.
const binaryPathPlaceholder = "@BINARY_PATH@"

//go:embed mellomting.service.tmpl
var serviceTemplate string

//go:embed mellomting.logrotate
var logrotateAsset []byte

//go:embed mellomting-config.yaml.example
var configTemplateAsset []byte

// RenderService renders the hardened systemd unit with the given binary
// path substituted into ExecStart. It matches deploy/mellomting.service
// byte-for-byte when binaryPath is /usr/local/bin/mellomting (asserted in
// systemd_test.go).
func RenderService(binaryPath string) string {
	return strings.ReplaceAll(serviceTemplate, binaryPathPlaceholder, binaryPath)
}

// Logrotate returns the accounting-log rotation policy (PLAN §42).
func Logrotate() []byte {
	return logrotateAsset
}

// ConfigTemplate returns the scaffold written into the config directory
// by install --systemd when no config file exists. It is deliberately
// incomplete — no backends and no models — so it does not pass validation
// and the daemon stays fail-closed until the operator fills those in; the
// commented stubs document every field. It is byte-identical to
// deploy/mellomting-config.yaml.example (asserted by the sync test).
func ConfigTemplate() []byte {
	return configTemplateAsset
}
