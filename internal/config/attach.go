package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"reflect"
)

// maxAttachServers bounds attach.servers: each one holds a connection to
// the edge.
const maxAttachServers = 16

// Attach makes the daemon an agent (PLAN §104): it dials an edge with an
// agent key and serves the edge's requests from local inference servers,
// one attachment per server, and does nothing else.
type Attach struct {
	// Edge is the proxy's base URL.
	Edge string `yaml:"edge"`
	// KeyFile holds the agent key.
	KeyFile string `yaml:"key_file"`
	// Servers are the local inference servers' base URLs.
	Servers []string `yaml:"servers"`
	// BackendKeyFile holds a bearer token for the local servers.
	BackendKeyFile string `yaml:"backend_key_file,omitempty"`
	// CAFile holds the PEM CAs the edge is verified with, instead of the
	// system roots.
	CAFile string `yaml:"ca_file,omitempty"`
	// InsecureEdge allows a plain http edge off loopback, which sends the
	// agent key unencrypted.
	InsecureEdge bool `yaml:"insecure_edge,omitempty"`
	// Metrics false refuses the edge's requests for the local servers'
	// load reports (PLAN §104): the agent's operator may not want the
	// edge's to see them. Absent means on.
	Metrics *bool `yaml:"metrics,omitempty"`
}

// MetricsEnabled reports whether the edge may read the local servers'
// load reports.
func (a *Attach) MetricsEnabled() bool { return a.Metrics == nil || *a.Metrics }

// ServesClients reports whether the daemon runs the proxy: it does
// unless it attaches, and a daemon that attaches does nothing else.
func (c *Config) ServesClients() bool { return c.Attach == nil }

// attachExcludes rejects every section a daemon that attaches would
// ignore, so none is configured in the belief that it applies. Logging,
// the sandbox modes and the shutdown grace period apply to both roles.
func attachExcludes(doc sourceDoc) []string {
	if doc.Attach == nil {
		return nil
	}
	sections := []struct {
		name string
		v    any
	}{
		{"server", doc.Server}, {"auth", doc.Auth}, {"accounting", doc.Accounting},
		{"limits", doc.Limits}, {"responses", doc.Responses}, {"retry", doc.Retry},
		{"agents", doc.Agents}, {"servers", doc.Servers}, {"models", doc.Models},
		{"security.backend_network", doc.Security.BackendNetwork},
	}
	var errs []string
	for _, sec := range sections {
		if !reflect.ValueOf(sec.v).IsZero() {
			errs = append(errs, sec.name+": does not apply with attach, which serves no clients")
		}
	}
	return errs
}

// ParseEdgeURL accepts an absolute http or https URL without credentials,
// query or fragment.
func ParseEdgeURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("scheme must be http or https")
	case u.Host == "":
		return nil, fmt.Errorf("host is missing")
	case u.User != nil:
		return nil, fmt.Errorf("credentials in the URL are not supported")
	case u.RawQuery != "" || u.Fragment != "":
		return nil, fmt.Errorf("query and fragment are not supported")
	}
	return u, nil
}

func validateAttach(a *Attach) []string {
	if a == nil {
		return nil
	}
	var errs []string
	if a.Edge == "" {
		errs = append(errs, "attach.edge: required")
	} else if u, err := ParseEdgeURL(a.Edge); err != nil {
		errs = append(errs, fmt.Sprintf("attach.edge: %v", err))
	} else if u.Scheme == "http" && IsNonLoopbackHost(u.Hostname()) && !a.InsecureEdge {
		errs = append(errs, "attach.edge: must be https, or the agent key travels unencrypted; set attach.insecure_edge to allow it")
	}
	for _, f := range []struct {
		name, path string
		required   bool
	}{
		{"key_file", a.KeyFile, true},
		{"backend_key_file", a.BackendKeyFile, false},
		{"ca_file", a.CAFile, false},
	} {
		switch {
		case f.path == "" && f.required:
			errs = append(errs, "attach."+f.name+": required")
		case f.path != "" && !filepath.IsAbs(f.path):
			errs = append(errs, "attach."+f.name+": must be an absolute path")
		}
	}
	switch n := len(a.Servers); {
	case n == 0:
		errs = append(errs, "attach.servers: at least one server is required")
	case n > maxAttachServers:
		errs = append(errs, fmt.Sprintf("attach.servers: at most %d servers, got %d", maxAttachServers, n))
	}
	seen := make(map[string]bool, len(a.Servers))
	for i, s := range a.Servers {
		if err := checkServerURL(s); err != nil {
			errs = append(errs, fmt.Sprintf("attach.servers[%d]: %v", i, err))
		}
		if seen[s] {
			errs = append(errs, fmt.Sprintf("attach.servers[%d]: %q is listed twice", i, s))
		}
		seen[s] = true
	}
	return errs
}

// checkServerURL applies the base-URL rules of a configured server
// (PLAN §15.1). The backend network mode does not apply: the agent is
// placed next to its servers by whoever installs it.
func checkServerURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || raw == "":
		return fmt.Errorf("%q is not a valid URL", raw)
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("unsupported scheme %q (want http or https)", u.Scheme)
	case u.User != nil, u.RawQuery != "", u.Fragment != "":
		return fmt.Errorf("userinfo, query strings and fragments are not supported")
	case u.Path != "" && u.Path != "/":
		return fmt.Errorf("path %q must be empty or /", u.Path)
	}
	// A trailing colon splits into a host and an empty port.
	if host, port, err := net.SplitHostPort(u.Host); err == nil && (host == "" || port == "") {
		return fmt.Errorf("host %q is malformed", u.Host)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("host is missing")
	}
	return nil
}
