package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mellomting/internal/landlock"
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

	defaultQualifierTimeout     = 3 * time.Second
	defaultQualifierConcurrency = 2
	defaultQualifierQueueSize   = 4

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
	if c.Auth.UsersFile == "" {
		c.Auth.UsersFile = DefaultUsersFile
	}
	if c.Auth.PepperFile == "" {
		c.Auth.PepperFile = DefaultPepperFile
	}

	s := &c.Server
	if s.MaxHeaderBytes == 0 {
		s.MaxHeaderBytes = defaultMaxHeaderBytes
	}
	if s.MaxBodyBytes == 0 {
		s.MaxBodyBytes = defaultMaxBodyBytes
	}
	if s.MaxResponseBytes == 0 {
		s.MaxResponseBytes = defaultMaxResponseBytes
	}
	if s.MaxInflightRequests == 0 {
		s.MaxInflightRequests = defaultMaxInflightRequests
	}
	if s.MaxBufferedRequestBytes == 0 {
		s.MaxBufferedRequestBytes = defaultMaxBufferedRequestBytes
	}
	if s.MaxConnections == 0 {
		s.MaxConnections = defaultMaxConnections
	}
	if s.ReadHeaderTimeout == 0 {
		s.ReadHeaderTimeout = Duration(defaultReadHeaderTimeout)
	}
	if s.ReadBodyTimeout == 0 {
		s.ReadBodyTimeout = Duration(defaultReadBodyTimeout)
	}
	if s.IdleTimeout == 0 {
		s.IdleTimeout = Duration(defaultIdleTimeout)
	}
	if s.StreamIdleTimeout == 0 {
		s.StreamIdleTimeout = Duration(defaultStreamIdleTimeout)
	}
	if s.StreamWriteTimeout == 0 {
		s.StreamWriteTimeout = Duration(defaultStreamWriteTimeout)
	}

	if c.Server.Listen.Network == "unix" && c.Server.Listen.Mode == "" {
		c.Server.Listen.Mode = DefaultUnixSocketMode
	}

	if c.Security.BackendNetwork.Mode == "" {
		c.Security.BackendNetwork.Mode = "loopback-only"
	}
	if c.Security.Landlock.Mode == "" {
		c.Security.Landlock.Mode = landlock.ModeRequired
	}
	if c.Security.Landlock.MinimumABI == 0 {
		c.Security.Landlock.MinimumABI = landlock.DefaultMinimumABI
	}

	if c.Logging.Format == "" {
		c.Logging.Format = "json"
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}

	a := &c.Accounting
	if a.Enabled {
		if a.Path == "" {
			a.Path = DefaultAccountingPath
		}
		if a.EnsureStreamUsage == nil {
			t := true
			a.EnsureStreamUsage = &t
		}
		if a.ReplayOnStart == nil {
			t := true
			a.ReplayOnStart = &t
		}
		if a.ReplayMaxBytes == 0 {
			a.ReplayMaxBytes = defaultAccountingReplayMaxBytes
		}
		if a.QueueSize == 0 {
			a.QueueSize = defaultAccountingQueue
		}
		if a.Overflow == "" {
			a.Overflow = "drop-and-alert"
		}
		if a.FSync == "" {
			a.FSync = "interval"
		}
		if a.FSyncInterval == 0 {
			a.FSyncInterval = Duration(5 * time.Second)
		}
	}

	if c.Limits.GlobalRequestsPerSecond == 0 {
		c.Limits.GlobalRequestsPerSecond = defaultGlobalRPS
	}
	if c.Limits.GlobalBurst == 0 {
		c.Limits.GlobalBurst = defaultGlobalBurst
	}
	if c.Limits.PreauthRequestsPerSecond == 0 {
		c.Limits.PreauthRequestsPerSecond = defaultPreauthRPS
	}
	if c.Limits.PreauthBurst == 0 {
		c.Limits.PreauthBurst = defaultPreauthBurst
	}
	if c.Limits.AuthFailureLogRate == 0 {
		c.Limits.AuthFailureLogRate = defaultAuthLogRate
	}

	if c.Shutdown.GracePeriod == 0 {
		c.Shutdown.GracePeriod = Duration(30 * time.Second)
	}

	if c.Responses.AffinityTTL == 0 {
		c.Responses.AffinityTTL = Duration(defaultAffinityTTL)
	}
	if c.Responses.MaxAffinityEntries == 0 {
		c.Responses.MaxAffinityEntries = defaultMaxAffinityEntries
	}

	if c.Retry.MaxAttempts == 0 {
		c.Retry.MaxAttempts = defaultRetryMaxAttempts
	}
	if c.Retry.InitialBackoff == 0 {
		c.Retry.InitialBackoff = Duration(defaultRetryInitialBackoff)
	}
	if c.Retry.MaxBackoff == 0 {
		c.Retry.MaxBackoff = Duration(defaultRetryMaxBackoff)
	}

	for name := range c.Backends {
		b := c.Backends[name]
		if b.ConnectTimeout == 0 {
			b.ConnectTimeout = Duration(defaultConnectTimeout)
		}
		if b.HeaderTimeout == 0 {
			b.HeaderTimeout = Duration(defaultHeaderTimeout)
		}
		if b.RequestTimeout == 0 {
			b.RequestTimeout = Duration(defaultRequestTimeout)
		}
		if b.StreamIdleTimeout == 0 {
			b.StreamIdleTimeout = Duration(defaultStreamIdleTimeout)
		}
		if b.MaxConcurrency == 0 {
			b.MaxConcurrency = defaultMaxConcurrency
		}
		if b.QueueSize == 0 {
			b.QueueSize = defaultBackendQueueSize
		}
		if b.QueueTimeout == 0 {
			b.QueueTimeout = Duration(defaultQueueTimeout)
		}
		c.Backends[name] = b
	}

	for name := range c.Qualifiers {
		q := c.Qualifiers[name]
		if q.Timeout == 0 {
			q.Timeout = Duration(defaultQualifierTimeout)
		}
		if q.MaxConcurrency == 0 {
			q.MaxConcurrency = defaultQualifierConcurrency
		}
		if q.QueueSize == 0 {
			q.QueueSize = defaultQualifierQueueSize
		}
		c.Qualifiers[name] = q
	}

	for name := range c.Models {
		m := c.Models[name]
		if m.Type == "" {
			m.Type = "generation"
		}
		if m.Strategy == "" {
			if len(m.Backends) <= 1 {
				m.Strategy = "single"
			} else {
				m.Strategy = "least-inflight"
			}
		}
		for i := range m.Backends {
			if m.Backends[i].Weight == 0 {
				m.Backends[i].Weight = 1
			}
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
	errs = append(errs, validateQualifiers(c.Qualifiers, c.Backends)...)
	errs = append(errs, validateModels(c.Models, c.Backends, c.Qualifiers)...)

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func validateServer(s *Server) []string {
	var errs []string

	switch s.Listen.Network {
	case "tcp":
		host, _, err := net.SplitHostPort(s.Listen.Address)
		if err != nil || host == "" {
			errs = append(errs, fmt.Sprintf("server.listen.address: %q must be host:port for network tcp", s.Listen.Address))
		} else if isNonLoopbackHost(host) && !tlsConfigured(s.TLS) && !s.AllowPlaintextNonLoopback {
			errs = append(errs, "server: non-loopback TCP listener requires tls configuration or allow_plaintext_non_loopback: true (PLAN §8.2)")
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

	if s.Listen.Network == "unix" && s.TLS.Mode != "" {
		errs = append(errs, "server.tls: only valid when listen network is tcp")
	}

	switch s.TLS.Mode {
	case "":
	case "files":
		if s.TLS.CertFile == "" || s.TLS.KeyFile == "" {
			errs = append(errs, "server.tls: cert_file and key_file are required when mode is files (PLAN §67)")
		}
	case "acme":
		if s.TLS.Hostname == "" || s.TLS.Email == "" {
			errs = append(errs, "server.tls: hostname and email are required when mode is acme (PLAN §68)")
		}
	default:
		errs = append(errs, fmt.Sprintf("server.tls.mode: %q must be \"files\" or \"acme\"", s.TLS.Mode))
	}

	return errs
}

func tlsConfigured(t TLS) bool { return t.Mode != "" }

func validateAuth(a *Auth) []string {
	var errs []string
	if a.UsersFile == "" {
		errs = append(errs, "auth.users_file: required")
	}
	if a.PepperFile == "" {
		errs = append(errs, "auth.pepper_file: required")
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
		set := a.Path != "" || a.EnsureStreamUsage != nil || a.ReplayOnStart != nil ||
			a.ReplayMaxBytes != 0 || a.QueueSize != 0 || a.Overflow != "" || a.FSync != "" ||
			a.FSyncInterval != 0 || a.UnknownUsageReservation != 0
		if set {
			errs = append(errs, "accounting: remaining fields must be empty when enabled is false")
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
		errs = append(errs, fmt.Sprintf("retry.max_attempts: %d out of range 1..%d (PLAN §23)", r.MaxAttempts, defaultRetryMaxAttemptsMax))
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
			errs = append(errs, fmt.Sprintf("%s.base_url: %q is not a valid URL", prefix, redactURL(b.BaseURL)))
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
			errs = append(errs, fmt.Sprintf("%s.base_url: query strings are not supported (PLAN §15.1)", prefix))
		}
		if u.Fragment != "" {
			errs = append(errs, fmt.Sprintf("%s.base_url: fragments are not supported (PLAN §15.1)", prefix))
		}
		if p := u.Path; p != "" && p != "/" {
			errs = append(errs, fmt.Sprintf("%s.base_url: path %q makes endpoint construction ambiguous (PLAN §15.1)", prefix, p))
		}

		host, _, err := net.SplitHostPort(u.Host)
		if err != nil || host == "" {
			errs = append(errs, fmt.Sprintf("%s.base_url: host %q is malformed or missing a port", prefix, u.Host))
		} else {
			if bn.Mode == "loopback-only" && !isLoopbackHost(host) {
				errs = append(errs, fmt.Sprintf("%s.base_url: %q is not a loopback IP literal; loopback-only mode requires a literal loopback destination (PLAN §16.1)", prefix, host))
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

func validateQualifiers(qualifiers map[string]Qualifier, backends map[string]Backend) []string {
	var errs []string

	for name, q := range qualifiers {
		prefix := fmt.Sprintf("qualifiers.%s", name)
		if q.Backend == "" {
			errs = append(errs, prefix+".backend: required")
		} else if _, ok := backends[q.Backend]; !ok {
			errs = append(errs, fmt.Sprintf("%s.backend: unknown backend %q (PLAN §28)", prefix, q.Backend))
		}
		if q.Model == "" {
			errs = append(errs, prefix+".model: required")
		}
		if q.Timeout.Duration() <= 0 {
			errs = append(errs, prefix+".timeout: must be > 0")
		}
		switch q.FailurePolicy {
		case "allow", "audit", "block", "": // "" reported below as required
		default:
			errs = append(errs, fmt.Sprintf("%s.failure_policy: %q must be allow, audit, or block", prefix, q.FailurePolicy))
		}
		if q.FailurePolicy == "" {
			errs = append(errs, prefix+".failure_policy: required, never implicit (PLAN §46)")
		}
		if err := qualifierMode(q.Input.Mode, prefix+".input.mode"); err != "" {
			errs = append(errs, err)
		}
		if err := qualifierMode(q.Output.Mode, prefix+".output.mode"); err != "" {
			errs = append(errs, err)
		}
		if q.MaxConcurrency < 0 {
			errs = append(errs, prefix+".max_concurrency: must be >= 0")
		}
		if q.QueueSize < 0 {
			errs = append(errs, prefix+".queue_size: must be >= 0")
		}

		if b, ok := backends[q.Backend]; ok {
			if host := urlHost(b.BaseURL); host != "" && isNonLoopbackHost(host) && !q.AllowRemoteContent {
				errs = append(errs, fmt.Sprintf("%s: backend %q is not loopback; allow_remote_content: true is required (PLAN §47)", prefix, q.Backend))
			}
		}
	}

	return errs
}

func qualifierMode(mode, field string) string {
	switch mode {
	case "disabled", "audit", "block":
		return ""
	case "":
		return fmt.Sprintf("%s: required, one of disabled, audit, or block", field)
	default:
		return fmt.Sprintf("%s: %q must be disabled, audit, or block", field, mode)
	}
}

func validateModels(models map[string]Model, backends map[string]Backend, qualifiers map[string]Qualifier) []string {
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

		switch m.Strategy {
		case "single", "round-robin", "weighted-round-robin", "least-inflight", "weighted-least-inflight":
		default:
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
				errs = append(errs, fmt.Sprintf("%s.backends: unknown backend %q (PLAN §28)", prefix, ref.Name))
			}
			if seen[ref.Name] {
				errs = append(errs, fmt.Sprintf("%s.backends: duplicate backend %q", prefix, ref.Name))
			}
			seen[ref.Name] = true
			if ref.Weight < 0 {
				errs = append(errs, fmt.Sprintf("%s.backends: %q has a negative weight", prefix, ref.Name))
			}
		}

		if m.Qualifier != "" {
			if _, ok := qualifiers[m.Qualifier]; !ok {
				errs = append(errs, fmt.Sprintf("%s.qualifier: unknown qualifier %q (PLAN §28)", prefix, m.Qualifier))
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
func isNonLoopbackHost(host string) bool {
	if addr, err := netip.ParseAddr(host); err == nil {
		return !addr.IsLoopback()
	}
	return true
}

func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		return u.Hostname()
	}
	return host
}

// redactURL removes any userinfo (credentials) from a URL string for use
// in error messages, so a malformed base_url with inlined credentials
// never leaks them to the operator or logs (T-M13). Best-effort: it
// works even when the URL failed to parse.
func redactURL(raw string) string {
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 0 {
		return raw
	}
	rest := raw[schemeEnd+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return raw
	}
	// Only redact when the '@' is part of the authority (before any
	// path/query/fragment separator), not an email-like string in a
	// path.
	slash := strings.IndexAny(rest, "/?#")
	if slash >= 0 && at > slash {
		return raw
	}
	return raw[:schemeEnd+3] + "<redacted>@" + rest[at+1:]
}

func inAnyPrefix(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
