package config

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"slices"

	"mellomting/internal/landlock"
	"mellomting/internal/redact"
)

// Defaults follow PLAN §76 and the per-section suggested defaults;
// every limit is finite (PLAN §9.1).
const (
	defaultMaxHeaderBytes          = 32768
	defaultMaxBodyBytes            = 16777216
	defaultMaxResponseBytes        = 67108864
	defaultMaxInflightRequests     = 64
	defaultMaxBufferedRequestBytes = 67108864

	defaultReadHeaderTimeout  = 5 * time.Second
	defaultReadBodyTimeout    = 60 * time.Second
	defaultIdleTimeout        = 120 * time.Second
	defaultStreamIdleTimeout  = 120 * time.Second
	defaultStreamWriteTimeout = 30 * time.Second

	defaultConnectTimeout   = 3 * time.Second
	defaultHeaderTimeout    = 30 * time.Second
	defaultRequestTimeout   = 20 * time.Minute
	defaultBackendQueueSize = 8
	defaultQueueTimeout     = 5 * time.Second
	defaultMaxConcurrency   = 4

	// defaultMaxConnections bounds concurrently accepted connections at
	// the listener (T-L15, PLAN §9.1).
	defaultMaxConnections = 1024

	defaultGlobalRPS    = 100.0
	defaultGlobalBurst  = 200
	defaultPreauthRPS   = 100.0
	defaultPreauthBurst = 200
	defaultAuthLogRate  = 1.0
	// DefaultPreauthSources caps the number of per-source pre-auth
	// states (PLAN §33): sources are not bounded by the key store, so
	// the registry must be bounded to keep a flood of distinct
	// addresses from growing memory without limit.
	DefaultPreauthSources  = 4096
	defaultAccountingQueue = 4096
	// defaultAccountingReplayMaxBytes bounds the JSONL tail replayed at
	// startup (PLAN §40).
	defaultAccountingReplayMaxBytes int64 = 1 << 30

	defaultAffinityTTL        = 2 * time.Hour
	defaultMaxAffinityEntries = 10000

	defaultRetryMaxAttempts    = 1
	defaultRetryMaxAttemptsMax = 8
	defaultRetryInitialBackoff = 100 * time.Millisecond
	defaultRetryMaxBackoff     = 1 * time.Second
)

// applyDefaults fills zero-valued fields with the PLAN defaults. Zero
// values that an operator would want explicitly (e.g. queue_size 0) are
// not distinguishable from omitted fields in YAML and take the default.
func applyDefaults(c *Config) {
	c.Auth.UsersFile = cmp.Or(c.Auth.UsersFile, DefaultUsersFile)
	c.Auth.PepperFile = cmp.Or(c.Auth.PepperFile, DefaultPepperFile)

	s := &c.Server
	s.MaxHeaderBytes = cmp.Or(s.MaxHeaderBytes, defaultMaxHeaderBytes)
	s.MaxBodyBytes = cmp.Or(s.MaxBodyBytes, defaultMaxBodyBytes)
	s.MaxResponseBytes = cmp.Or(s.MaxResponseBytes, defaultMaxResponseBytes)
	s.MaxInflightRequests = cmp.Or(s.MaxInflightRequests, defaultMaxInflightRequests)
	s.MaxBufferedRequestBytes = cmp.Or(s.MaxBufferedRequestBytes, defaultMaxBufferedRequestBytes)
	s.MaxConnections = cmp.Or(s.MaxConnections, defaultMaxConnections)
	s.ReadHeaderTimeout = cmp.Or(s.ReadHeaderTimeout, Duration(defaultReadHeaderTimeout))
	s.ReadBodyTimeout = cmp.Or(s.ReadBodyTimeout, Duration(defaultReadBodyTimeout))
	s.IdleTimeout = cmp.Or(s.IdleTimeout, Duration(defaultIdleTimeout))
	s.StreamIdleTimeout = cmp.Or(s.StreamIdleTimeout, Duration(defaultStreamIdleTimeout))
	s.StreamWriteTimeout = cmp.Or(s.StreamWriteTimeout, Duration(defaultStreamWriteTimeout))

	// Only a unix listener has a file mode to default.
	if s.Listen.Network == "unix" {
		s.Listen.Mode = cmp.Or(s.Listen.Mode, DefaultUnixSocketMode)
	}

	sec := &c.Security
	sec.BackendNetwork.Mode = cmp.Or(sec.BackendNetwork.Mode, "loopback-only")
	sec.Landlock.Mode = cmp.Or(sec.Landlock.Mode, landlock.ModeRequired)
	sec.Landlock.MinimumABI = cmp.Or(sec.Landlock.MinimumABI, landlock.DefaultMinimumABI)

	c.Logging.Format = cmp.Or(c.Logging.Format, "json")
	c.Logging.Level = cmp.Or(c.Logging.Level, "info")

	// Accounting fields default only when accounting is enabled: validate
	// rejects them when it is off, and defaulting them here would mask
	// that rejection.
	if a := &c.Accounting; a.Enabled {
		a.Path = cmp.Or(a.Path, DefaultAccountingPath)
		a.EnsureStreamUsage = cmp.Or(a.EnsureStreamUsage, new(true))
		a.ReplayOnStart = cmp.Or(a.ReplayOnStart, new(true))
		a.ReplayMaxBytes = cmp.Or(a.ReplayMaxBytes, defaultAccountingReplayMaxBytes)
		a.QueueSize = cmp.Or(a.QueueSize, defaultAccountingQueue)
		a.Overflow = cmp.Or(a.Overflow, "drop-and-alert")
		a.FSync = cmp.Or(a.FSync, "interval")
		if a.FSync == "interval" {
			a.FSyncInterval = cmp.Or(a.FSyncInterval, Duration(5*time.Second))
		}
	}

	l := &c.Limits
	l.GlobalRequestsPerSecond = cmp.Or(l.GlobalRequestsPerSecond, defaultGlobalRPS)
	l.GlobalBurst = cmp.Or(l.GlobalBurst, defaultGlobalBurst)
	l.PreauthRequestsPerSecond = cmp.Or(l.PreauthRequestsPerSecond, defaultPreauthRPS)
	l.PreauthBurst = cmp.Or(l.PreauthBurst, defaultPreauthBurst)
	l.AuthFailureLogRate = cmp.Or(l.AuthFailureLogRate, defaultAuthLogRate)

	c.Shutdown.GracePeriod = cmp.Or(c.Shutdown.GracePeriod, Duration(30*time.Second))

	c.Responses.AffinityTTL = cmp.Or(c.Responses.AffinityTTL, Duration(defaultAffinityTTL))
	c.Responses.MaxAffinityEntries = cmp.Or(c.Responses.MaxAffinityEntries, defaultMaxAffinityEntries)

	c.Retry.MaxAttempts = cmp.Or(c.Retry.MaxAttempts, defaultRetryMaxAttempts)
	c.Retry.InitialBackoff = cmp.Or(c.Retry.InitialBackoff, Duration(defaultRetryInitialBackoff))
	c.Retry.MaxBackoff = cmp.Or(c.Retry.MaxBackoff, Duration(defaultRetryMaxBackoff))

	for name, b := range c.Backends {
		b.ConnectTimeout = cmp.Or(b.ConnectTimeout, Duration(defaultConnectTimeout))
		b.HeaderTimeout = cmp.Or(b.HeaderTimeout, Duration(defaultHeaderTimeout))
		b.RequestTimeout = cmp.Or(b.RequestTimeout, Duration(defaultRequestTimeout))
		b.StreamIdleTimeout = cmp.Or(b.StreamIdleTimeout, Duration(defaultStreamIdleTimeout))
		b.MaxConcurrency = cmp.Or(b.MaxConcurrency, defaultMaxConcurrency)
		b.QueueSize = cmp.Or(b.QueueSize, defaultBackendQueueSize)
		b.QueueTimeout = cmp.Or(b.QueueTimeout, Duration(defaultQueueTimeout))
		c.Backends[name] = b
	}

	for name, m := range c.Models {
		m.Type = cmp.Or(m.Type, "generation")
		// Strategy is inferred from the replica count rather than being a
		// fixed default, so it is not a cmp.Or case.
		if m.Strategy == "" {
			m.Strategy = "single"
			if len(m.Backends) > 1 {
				m.Strategy = "least-inflight"
			}
		}
		for i := range m.Backends {
			m.Backends[i].Weight = cmp.Or(m.Backends[i].Weight, 1)
		}
		c.Models[name] = m
	}
}

// validate enforces every cross-field constraint (PLAN §28) and returns
// all violations at once.
func validate(c *Config) error {
	var errs []string
	if c.Version != 1 {
		errs = append(errs, fmt.Sprintf("version: %d is not supported (want 1)", c.Version))
	}
	errs = append(errs, validateServer(&c.Server)...)
	errs = append(errs, validateAuth(&c.Auth)...)
	errs = append(errs, validateSecurity(&c.Security)...)
	errs = append(errs, validateLogging(&c.Logging)...)
	errs = append(errs, validateAccounting(&c.Accounting)...)
	errs = append(errs, validateLimits(&c.Limits)...)
	errs = append(errs, validateShutdown(&c.Shutdown)...)
	errs = append(errs, validateResponses(&c.Responses)...)
	errs = append(errs, validateRetry(&c.Retry)...)
	errs = append(errs, validateBackends(c.Backends, c.Security.BackendNetwork)...)
	errs = append(errs, validateModels(c.Models, c.Backends)...)

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func validateServer(s *Server) []string {
	var errs []string

	switch s.Listen.Network {
	case "tcp":
		if s.Listen.Mode != "" {
			errs = append(errs, "server.listen.mode: only valid for network unix")
		}
		host, _, err := net.SplitHostPort(s.Listen.Address)
		if err != nil {
			errs = append(errs, fmt.Sprintf("server.listen.address: %q must be host:port for network tcp", s.Listen.Address))
		} else if IsNonLoopbackHost(host) && !TLSConfigured(s.TLS) && !s.AllowPlaintextNonLoopback {
			errs = append(errs, "server: non-loopback TCP listener requires tls configuration or allow_plaintext_non_loopback: true")
		}
	case "unix":
		if s.Listen.Address == "" {
			errs = append(errs, "server.listen.address: required for network unix")
		}
		if mode, err := strconv.ParseUint(s.Listen.Mode, 8, 16); err != nil || mode == 0 || mode > 0o777 {
			errs = append(errs, fmt.Sprintf("server.listen.mode: %q must be an octal mode like 0660", s.Listen.Mode))
		}
	default:
		errs = append(errs, `server.listen.network: required, one of "unix" or "tcp"`)
		if s.Listen.Mode != "" {
			errs = append(errs, "server.listen.mode: only valid for network unix")
		}
	}

	if s.MaxHeaderBytes <= 0 {
		errs = append(errs, "server.max_header_bytes: must be > 0")
	}
	if s.MaxBodyBytes <= 0 {
		errs = append(errs, "server.max_body_bytes: must be > 0")
	}
	if s.MaxResponseBytes <= 0 {
		errs = append(errs, "server.max_response_bytes: must be > 0")
	}
	if s.MaxInflightRequests <= 0 {
		errs = append(errs, "server.max_inflight_requests: must be > 0")
	}
	if s.MaxBufferedRequestBytes <= 0 {
		errs = append(errs, "server.max_buffered_request_bytes: must be > 0")
	}
	if s.MaxConnections <= 0 {
		errs = append(errs, "server.max_connections: must be > 0")
	}
	if s.ReadHeaderTimeout.Duration() <= 0 {
		errs = append(errs, "server.read_header_timeout: must be > 0")
	}
	if s.ReadBodyTimeout.Duration() <= 0 {
		errs = append(errs, "server.read_body_timeout: must be > 0")
	}
	if s.IdleTimeout.Duration() <= 0 {
		errs = append(errs, "server.idle_timeout: must be > 0")
	}
	if s.StreamIdleTimeout.Duration() <= 0 {
		errs = append(errs, "server.stream_idle_timeout: must be > 0")
	}
	if s.StreamWriteTimeout.Duration() <= 0 {
		errs = append(errs, "server.stream_write_timeout: must be > 0")
	}

	if s.Listen.Network == "unix" && TLSConfigured(s.TLS) {
		errs = append(errs, "server.tls: only valid when listen network is tcp")
	}

	if TLSConfigured(s.TLS) && (s.TLS.CertFile == "" || s.TLS.KeyFile == "") {
		errs = append(errs, "server.tls: cert_file and key_file are required together")
	}

	return errs
}

// TLSConfigured reports whether a static TLS keypair is configured at
// all; validate requires both halves together.
func TLSConfigured(t TLS) bool { return t.CertFile != "" || t.KeyFile != "" }

// validateAuth requires both auth paths to be absolute. They select what a
// privileged `key` command reads and rewrites, so resolving them against
// the working directory would make the files used depend on where the
// command was run from.
func validateAuth(a *Auth) []string {
	var errs []string
	switch {
	case a.UsersFile == "":
		errs = append(errs, "auth.users_file: required")
	case !filepath.IsAbs(a.UsersFile):
		errs = append(errs, "auth.users_file: must be an absolute path")
	}
	switch {
	case a.PepperFile == "":
		errs = append(errs, "auth.pepper_file: required")
	case !filepath.IsAbs(a.PepperFile):
		errs = append(errs, "auth.pepper_file: must be an absolute path")
	}
	return errs
}

func validateSecurity(s *Security) []string {
	var errs []string

	bn := s.BackendNetwork
	switch bn.Mode {
	case "loopback-only", "any":
		if len(bn.CIDRs) > 0 {
			errs = append(errs, "security.backend_network.cidrs: only valid when mode is allowed-cidrs")
		}
	case "allowed-cidrs":
		if len(bn.CIDRs) == 0 {
			errs = append(errs, "security.backend_network.cidrs: required when mode is allowed-cidrs")
		}
		for _, cidr := range bn.CIDRs {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				errs = append(errs, fmt.Sprintf("security.backend_network.cidrs: %q is not a valid CIDR", cidr))
			}
		}
	default:
		errs = append(errs, fmt.Sprintf("security.backend_network.mode: %q must be loopback-only, allowed-cidrs, or any", bn.Mode))
	}

	l := s.Landlock
	switch l.Mode {
	case landlock.ModeRequired, landlock.ModeBestEffort, landlock.ModeDisabled:
	default:
		errs = append(errs, fmt.Sprintf("security.landlock.mode: %q must be required, best-effort, or disabled", l.Mode))
	}
	if l.MinimumABI < 1 || l.MinimumABI > landlock.MaxABI {
		errs = append(errs, fmt.Sprintf("security.landlock.minimum_abi: %d out of range 1..%d", l.MinimumABI, landlock.MaxABI))
	}

	return errs
}

func validateLogging(l *Logging) []string {
	var errs []string
	switch l.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Sprintf("logging.format: %q must be json or text", l.Format))
	}
	switch l.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Sprintf("logging.level: %q must be debug, info, warn, or error", l.Level))
	}
	return errs
}

func validateAccounting(a *Accounting) []string {
	var errs []string

	if !a.Enabled {
		// FIX-04/N10: ensure_stream_usage and unknown_usage_reservation
		// govern token-quota charging (PLAN §38-39), not the JSONL, so
		// they stay usable when accounting is disabled but per-key token
		// quotas are configured. serve rejects them at startup when no
		// quota exists; every other field still requires accounting to
		// be enabled.
		set := a.Path != "" || a.ReplayOnStart != nil ||
			a.ReplayMaxBytes != 0 || a.QueueSize != 0 || a.Overflow != "" || a.FSync != "" ||
			a.FSyncInterval != 0
		if set {
			errs = append(errs, "accounting: remaining fields must be empty when enabled is false")
		}
		if a.UnknownUsageReservation < 0 {
			errs = append(errs, "accounting.unknown_usage_reservation: must be >= 0")
		}
		return errs
	}

	if a.Path == "" {
		errs = append(errs, "accounting.path: required when accounting is enabled")
	}
	if a.QueueSize <= 0 {
		errs = append(errs, "accounting.queue_size: must be > 0 when accounting is enabled")
	}
	if a.ReplayMaxBytes < 0 {
		errs = append(errs, "accounting.replay_max_bytes: must be >= 0")
	}
	if a.UnknownUsageReservation < 0 {
		errs = append(errs, "accounting.unknown_usage_reservation: must be >= 0")
	}
	switch a.Overflow {
	case "drop-and-alert":
	default:
		errs = append(errs, fmt.Sprintf("accounting.overflow: %q must be drop-and-alert", a.Overflow))
	}
	switch a.FSync {
	case "interval":
		if a.FSyncInterval.Duration() <= 0 {
			errs = append(errs, "accounting.fsync_interval: must be > 0 when fsync is interval")
		}
	case "every", "never":
		if a.FSyncInterval != 0 {
			errs = append(errs, "accounting.fsync_interval: only valid when fsync is interval")
		}
	default:
		errs = append(errs, fmt.Sprintf("accounting.fsync: %q must be interval, every, or never", a.FSync))
	}

	return errs
}

func validateLimits(l *Limits) []string {
	var errs []string
	if l.GlobalRequestsPerSecond <= 0 {
		errs = append(errs, "limits.global_requests_per_second: must be > 0")
	}
	if l.GlobalBurst <= 0 {
		errs = append(errs, "limits.global_burst: must be > 0")
	}
	if l.PreauthRequestsPerSecond <= 0 {
		errs = append(errs, "limits.preauth_requests_per_second: must be > 0")
	}
	if l.PreauthBurst <= 0 {
		errs = append(errs, "limits.preauth_burst: must be > 0")
	}
	if l.AuthFailureLogRate <= 0 {
		errs = append(errs, "limits.auth_failure_log_rate: must be > 0")
	}
	return errs
}

func validateShutdown(s *Shutdown) []string {
	var errs []string
	if s.GracePeriod.Duration() <= 0 {
		errs = append(errs, "shutdown.grace_period: must be > 0")
	}
	return errs
}

func validateResponses(r *Responses) []string {
	var errs []string
	if r.AffinityTTL.Duration() <= 0 {
		errs = append(errs, "responses.affinity_ttl: must be > 0")
	}
	if r.MaxAffinityEntries <= 0 {
		errs = append(errs, "responses.max_affinity_entries: must be > 0")
	}
	return errs
}

func validateRetry(r *Retry) []string {
	var errs []string
	if r.MaxAttempts < 1 || r.MaxAttempts > defaultRetryMaxAttemptsMax {
		errs = append(errs, fmt.Sprintf("retry.max_attempts: %d out of range 1..%d", r.MaxAttempts, defaultRetryMaxAttemptsMax))
	}
	if r.InitialBackoff.Duration() <= 0 {
		errs = append(errs, "retry.initial_backoff: must be > 0")
	}
	if r.MaxBackoff.Duration() <= 0 {
		errs = append(errs, "retry.max_backoff: must be > 0")
	}
	if r.InitialBackoff.Duration() > r.MaxBackoff.Duration() {
		errs = append(errs, "retry.initial_backoff: must not exceed retry.max_backoff")
	}
	return errs
}

func validateBackends(backends map[string]Backend, bn BackendNetwork) []string {
	var errs []string
	if len(backends) == 0 {
		return []string{"backends: at least one backend is required"}
	}

	allowed := make([]netip.Prefix, 0, len(bn.CIDRs))
	for _, cidr := range bn.CIDRs {
		if p, err := netip.ParsePrefix(cidr); err == nil {
			allowed = append(allowed, p)
		}
	}

	for name, b := range backends {
		prefix := fmt.Sprintf("backends.%s", name)

		u, err := url.Parse(b.BaseURL)
		if err != nil || b.BaseURL == "" {
			errs = append(errs, fmt.Sprintf("%s.base_url: %q is not a valid URL", prefix, redact.URL(b.BaseURL)))
			continue
		}
		switch u.Scheme {
		case "http", "https":
		default:
			errs = append(errs, fmt.Sprintf("%s.base_url: unsupported scheme %q (want http or https)", prefix, u.Scheme))
		}
		if u.User != nil {
			errs = append(errs, fmt.Sprintf("%s.base_url: userinfo is not supported", prefix))
		}
		if u.RawQuery != "" {
			errs = append(errs, fmt.Sprintf("%s.base_url: query strings are not supported", prefix))
		}
		if u.Fragment != "" {
			errs = append(errs, fmt.Sprintf("%s.base_url: fragments are not supported", prefix))
		}
		if p := u.Path; p != "" && p != "/" {
			errs = append(errs, fmt.Sprintf("%s.base_url: path %q makes endpoint construction ambiguous", prefix, p))
		}

		host, _, err := net.SplitHostPort(u.Host)
		if err != nil || host == "" {
			errs = append(errs, fmt.Sprintf("%s.base_url: host %q is malformed or missing a port", prefix, u.Host))
		} else {
			if bn.Mode == "loopback-only" && !isLoopbackHost(host) {
				errs = append(errs, fmt.Sprintf("%s.base_url: %q is not a loopback IP literal; loopback-only mode requires a literal loopback destination", prefix, host))
			}
			if bn.Mode == "allowed-cidrs" {
				if addr, err := netip.ParseAddr(host); err == nil && !inAnyPrefix(addr, allowed) {
					errs = append(errs, fmt.Sprintf("%s.base_url: %q is not within security.backend_network.cidrs", prefix, host))
				}
			}
		}

		if b.UpstreamModel == "" {
			errs = append(errs, prefix+".upstream_model: required")
		}
		if b.ConnectTimeout.Duration() <= 0 {
			errs = append(errs, prefix+".connect_timeout: must be > 0")
		}
		if b.HeaderTimeout.Duration() <= 0 {
			errs = append(errs, prefix+".header_timeout: must be > 0")
		}
		if b.RequestTimeout.Duration() <= 0 {
			errs = append(errs, prefix+".request_timeout: must be > 0")
		}
		if b.StreamIdleTimeout.Duration() <= 0 {
			errs = append(errs, prefix+".stream_idle_timeout: must be > 0")
		}
		if b.MaxConcurrency < 1 {
			errs = append(errs, prefix+".max_concurrency: must be >= 1")
		}
		if b.QueueSize < 0 {
			errs = append(errs, prefix+".queue_size: must be >= 0")
		}
		if b.QueueTimeout.Duration() <= 0 {
			errs = append(errs, prefix+".queue_timeout: must be > 0")
		}
	}

	return errs
}

// SupportedStrategies is the canonical routing strategy set (PLAN §19).
// routing derives its own acceptance set from this list, so the config
// validator and the router can never drift (FIX-24b; routing imports
// config, so the list cannot live in routing without an import cycle).
var SupportedStrategies = []string{
	"single",
	"round-robin",
	"weighted-round-robin",
	"least-inflight",
	"weighted-least-inflight",
}

func validateModels(models map[string]Model, backends map[string]Backend) []string {
	var errs []string
	if len(models) == 0 {
		return []string{"models: at least one public model is required"}
	}

	for name, m := range models {
		prefix := fmt.Sprintf("models.%s", name)

		switch m.Type {
		case "generation", "embedding":
		default:
			errs = append(errs, fmt.Sprintf("%s.type: %q must be generation or embedding", prefix, m.Type))
		}

		if !slices.Contains(SupportedStrategies, m.Strategy) {
			errs = append(errs, fmt.Sprintf("%s.strategy: %q is not a supported routing strategy", prefix, m.Strategy))
		}
		if m.Strategy == "single" && len(m.Backends) != 1 {
			errs = append(errs, fmt.Sprintf("%s: strategy single requires exactly one backend", prefix))
		}

		if len(m.Backends) == 0 {
			errs = append(errs, prefix+".backends: at least one backend reference is required")
		}
		seen := make(map[string]bool, len(m.Backends))
		for _, ref := range m.Backends {
			if ref.Name == "" {
				errs = append(errs, prefix+".backends: empty backend reference")
				continue
			}
			if _, ok := backends[ref.Name]; !ok {
				errs = append(errs, fmt.Sprintf("%s.backends: unknown backend %q", prefix, ref.Name))
			}
			if seen[ref.Name] {
				errs = append(errs, fmt.Sprintf("%s.backends: duplicate backend %q", prefix, ref.Name))
			}
			seen[ref.Name] = true
			if ref.Weight < 0 {
				errs = append(errs, fmt.Sprintf("%s.backends: %q has a negative weight", prefix, ref.Name))
			}
		}

		if m.Policy.MaxOutputTokens < 0 {
			errs = append(errs, prefix+".policy.max_output_tokens: must be >= 0")
		}
	}

	return errs
}

func isLoopbackHost(host string) bool {
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// isNonLoopbackHost fails closed: unresolvable-or-DNS hosts count as
// non-loopback.
// IsNonLoopbackHost reports whether host is anything other than a
// literal loopback address. It fails closed: any hostname — localhost
// included — counts as non-loopback, because a name can resolve
// anywhere (T-Q11, PLAN §8.2).
func IsNonLoopbackHost(host string) bool {
	if addr, err := netip.ParseAddr(host); err == nil {
		return !addr.IsLoopback()
	}
	return true
}

// IsNonLoopbackListenAddress reports whether a tcp listen address is
// non-loopback, failing closed on a malformed address. validate reports
// a malformed address separately with its own message, so it splits the
// address itself rather than calling this.
func IsNonLoopbackListenAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true
	}
	return IsNonLoopbackHost(host)
}

func inAnyPrefix(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
