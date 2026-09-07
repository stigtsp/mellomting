// Package sandbox describes what the daemon may still do after startup,
// independent of the kernel that enforces it (PLAN §53-63).
//
// The policy is plain data so it can be built and tested on any
// platform. Enforcing it is the job of a platform backend:
// internal/landlock on Linux, internal/seatbelt on macOS. Neither is
// consulted on a platform it does not cover, and neither silently
// degrades a required sandbox to none (PLAN §7, §55).
package sandbox

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
)

// Sandbox modes (PLAN §55).
const (
	ModeRequired   = "required"
	ModeBestEffort = "best-effort"
	ModeDisabled   = "disabled"
)

// Listener is the ingress listener the daemon keeps serving on after it
// is confined. It is already bound and listening when the policy is
// applied, so no backend needs the right to create it — but a backend
// that filters accepts (Seatbelt does; Landlock does not) has to be told
// which one to keep open.
type Listener struct {
	// UnixPath is the socket pathname of a unix listener.
	UnixPath string
	// TCPPort is the port of a TCP listener.
	TCPPort uint16
}

// Policy is the set of capabilities the daemon needs after all secrets
// have been preloaded and their file descriptors closed (PLAN §58-60).
// Everything outside it is denied.
type Policy struct {
	// ReadPaths are pathnames the daemon may open read-only; a
	// directory covers the files beneath it. This is the directory
	// holding the users file, so a SIGHUP reload (PLAN §30) can re-read
	// it by pathname across the rename every key mutation publishes it
	// through (PLAN §58).
	ReadPaths []string
	// WriteFiles are pathnames the daemon may open for writing. This is
	// the accounting log only if it is (re-)opened by pathname
	// (PLAN §58).
	WriteFiles []string
	// ConnectTCP are the TCP destination ports the daemon may connect
	// to. Both backends filter by port and carry no destination IP
	// (PLAN §60); restricting the address is the job of the backend
	// network modes (PLAN §16).
	ConnectTCP []uint16
	// Listeners are the sockets the daemon keeps serving on: the
	// ingress, and the admin socket when one is configured.
	Listeners []Listener
}

// Summarize renders the policy for operational logs (PLAN §43): the
// summary carries no secret.
func (p Policy) Summarize() []string {
	out := make([]string, 0, len(p.ReadPaths)+len(p.WriteFiles)+len(p.ConnectTCP)+len(p.Listeners))
	for _, f := range p.ReadPaths {
		out = append(out, "read "+f)
	}
	for _, f := range p.WriteFiles {
		out = append(out, "write "+f)
	}
	for _, l := range p.Listeners {
		switch {
		case l.UnixPath != "":
			out = append(out, "accept "+l.UnixPath)
		case l.TCPPort != 0:
			out = append(out, fmt.Sprintf("accept tcp %d", l.TCPPort))
		}
	}
	for _, port := range p.ConnectTCP {
		out = append(out, fmt.Sprintf("connect tcp %d", port))
	}
	return out
}

// Report is the result of a read-only capability probe. It applies no
// policy.
type Report struct {
	// Platform is runtime.GOOS.
	Platform string
	// Backend names the enforcement mechanism this platform uses.
	Backend string
	// Supported is true when the platform accepts the sandbox.
	Supported bool
	// KernelABI is the highest Landlock ABI the kernel supports; 0 on
	// platforms whose backend has no ABI to report.
	KernelABI int
	// Reason explains why the sandbox is unavailable when !Supported.
	Reason string
}

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

// BackendPorts extracts the TCP destination port of each backend base
// URL (PLAN §60). An omitted port uses the scheme default. The result is
// sorted and de-duplicated. A malformed URL, an unsupported scheme, or
// an invalid port is an error: the policy is fail-closed (PLAN §55).
//
// Errors name what is wrong and never echo the URL, which the operator
// already identifies by the field it came from.
func BackendPorts(baseURLs ...string) ([]uint16, error) {
	seen := make(map[uint16]bool, len(baseURLs))
	var ports []uint16
	for _, raw := range baseURLs {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return nil, errors.New("backend base URL is not valid")
		}
		switch u.Scheme {
		case "http", "https":
		default:
			return nil, fmt.Errorf("backend base URL: unsupported scheme %q (want http or https)", u.Scheme)
		}
		port := u.Port()
		if port == "" {
			var ok bool
			port, ok = DefaultPortForScheme(u.Scheme)
			if !ok {
				return nil, fmt.Errorf("backend base URL: unsupported scheme %q (want http or https)", u.Scheme)
			}
		}
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("backend base URL carries an invalid TCP port %q", port)
		}
		if !seen[uint16(n)] {
			seen[uint16(n)] = true
			ports = append(ports, uint16(n))
		}
	}
	slices.Sort(ports)
	return ports, nil
}
