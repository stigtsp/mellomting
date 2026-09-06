package landlock

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
)

// Policy is the post-startup Landlock policy of the daemon (PLAN
// §58-60): the small set of capabilities the process needs after all
// secrets have been preloaded and their file descriptors closed. It is
// plain data so it can be built and tested on any platform; applying it
// is Linux-only (see Apply).
type Policy struct {
	// ReadFiles are pathnames the daemon may open read-only after
	// confinement. This is the users file: a SIGHUP reload (PLAN §30)
	// re-reads it by pathname.
	ReadFiles []string
	// ReadDirs are directories whose files the daemon may open
	// read-only after confinement. A Landlock rule binds to the inode
	// behind the path at the time the ruleset is built, and every key
	// mutation publishes the users file by renaming a new file over it,
	// so a rule naming the file alone stops matching the moment a key
	// is created, disabled, or revoked — the reload is denied and the
	// change waits for a restart. Granting the directory keeps the
	// reload working across that rename (PLAN §30, §58).
	//
	// The grant is read-only and covers only the directory holding the
	// users file, but that directory normally also holds the pepper and
	// the configuration, so a confined daemon can re-open those too.
	// Both are already read at startup and the pepper stays in memory
	// for the life of the process, so this widens what a compromised
	// daemon can re-read, not what it can reach for the first time.
	ReadDirs []string
	// WriteFiles are pathnames the daemon may open for writing after
	// confinement. This is the accounting log only if it is (re-)opened
	// by pathname (PLAN §58).
	WriteFiles []string
	// ConnectTCP are the TCP destination ports the daemon may connect
	// to. Landlock TCP rules are port-based and carry no destination IP
	// (PLAN §60); external address restriction is the job of the backend
	// network modes (PLAN §16).
	ConnectTCP []uint16
}

// Summarize renders the policy for operational logs (PLAN §43): the
// summary carries no secret.
func (p Policy) Summarize() []string {
	out := make([]string, 0, len(p.ReadFiles)+len(p.ReadDirs)+len(p.WriteFiles)+len(p.ConnectTCP))
	for _, f := range p.ReadFiles {
		out = append(out, "read "+f)
	}
	for _, d := range p.ReadDirs {
		out = append(out, "read files in "+d)
	}
	for _, f := range p.WriteFiles {
		out = append(out, "write "+f)
	}
	for _, port := range p.ConnectTCP {
		out = append(out, fmt.Sprintf("connect tcp %d", port))
	}
	return out
}

// BackendPorts extracts the TCP destination port of each backend base
// URL (PLAN §60). An omitted port uses the scheme default (80 for
// http, 443 for https). The result is sorted and de-duplicated. A
// malformed URL, an unsupported scheme, or an invalid port is an error:
// the policy is fail-closed (PLAN §55).
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
