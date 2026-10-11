package config

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"slices"

	"mellomting/internal/landlock"
	"mellomting/internal/sandbox"
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

	// Backend defaults are sized for a local inference server, not for
	// an ordinary HTTP service. A healthy single-GPU model spends tens
	// of seconds on admission and long-context prefill before it can
	// emit a response header, so the header bound has to cover queueing
	// plus prefill; a 30s bound turned healthy 40-160 KB agent requests
	// into 504s at exactly 30.0s while the backend was still making
	// progress, and the retries that provoked deepened the overload.
	//
	// The header bound is what those 504s were about, not the
	// concurrency. A continuous-batching server runs many sequences at
	// once, so max_concurrency stays high enough to let it batch;
	// whatever it cannot take waits in the local queue, which
	// queue_size bounds and a client disconnect drains, rather than in
	// the backend's private queue where neither is true.
	defaultConnectTimeout   = 3 * time.Second
	defaultHeaderTimeout    = 300 * time.Second
	defaultRequestTimeout   = 30 * time.Minute
	defaultBackendQueueSize = 32
	// No agent client waits ten minutes to be forwarded, so the back
	// half of the wait the incident report proposed would hold global
	// and per-key in-flight slots for requests nobody will read. Two
	// minutes still absorbs bursts far beyond the 5s that caused the
	// 504 wave, while bounding what one wedged backend can hold.
	defaultQueueTimeout   = 120 * time.Second
	defaultMaxConcurrency = 8

	// A decision server answers in milliseconds, not minutes
	// (SYSTEMONE_PLAN §D11): the bounds a non-streaming call waits on
	// are 30s — 200 times the observed latency — so a wedged decision
	// server is refused in seconds rather than holding a slot for half
	// an hour. The header bound is kept no larger than the request
	// bound.
	decisionRequestTimeout = 30 * time.Second
	decisionHeaderTimeout  = 30 * time.Second

	// Ceilings on a server's admission settings. Each in-flight slot
	// may hold a backend connection, so the cap stays within the
	// listener's connection bound; each queued request holds global and
	// per-key in-flight slots while it waits, so neither the queue nor
	// its wait may be unbounded. A wait longer than request_timeout
	// would outlive the request it is waiting to send.
	maxServerConcurrency = 1024
	maxServerQueueSize   = 4096

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

	defaultAgentMaxAttachments       = 16
	defaultAgentMaxAttachmentsPerKey = 8
	defaultAgentMaxModels            = 16
	maxAgentAttachments              = 256
	maxAgentModels                   = 256

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
	sec.Landlock.Mode = cmp.Or(sec.Landlock.Mode, sandbox.ModeBestEffort)
	sec.Seatbelt.Mode = cmp.Or(sec.Seatbelt.Mode, sandbox.ModeBestEffort)
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

	// Agent limits default only when agents are enabled, like
	// accounting: validate rejects them when agents are off.
	if a := &c.Agents; a.Enabled {
		a.MaxAttachments = cmp.Or(a.MaxAttachments, defaultAgentMaxAttachments)
		a.MaxAttachmentsPerKey = cmp.Or(a.MaxAttachmentsPerKey, min(defaultAgentMaxAttachmentsPerKey, a.MaxAttachments))
		a.MaxConcurrency = cmp.Or(a.MaxConcurrency, defaultMaxConcurrency)
		a.MaxModels = cmp.Or(a.MaxModels, defaultAgentMaxModels)
		a.QueueSize = cmp.Or(a.QueueSize, defaultBackendQueueSize)
		a.QueueTimeout = cmp.Or(a.QueueTimeout, Duration(defaultQueueTimeout))
	}

	for name, b := range c.Backends {
		b.ConnectTimeout = cmp.Or(b.ConnectTimeout, Duration(defaultConnectTimeout))
		b.HeaderTimeout = cmp.Or(b.HeaderTimeout, Duration(defaultHeaderTimeout))
		b.RequestTimeout = cmp.Or(b.RequestTimeout, Duration(defaultRequestTimeout))
		b.StreamIdleTimeout = cmp.Or(b.StreamIdleTimeout, Duration(defaultStreamIdleTimeout))
		// Admission limits are resolved per server in normalization,
		// where an explicit queue_size: 0 can still be told from unset.
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
	if c.ServesClients() {
		errs = append(errs, validateServer(&c.Server)...)
		errs = append(errs, validateAuth(&c.Auth)...)
	}
	errs = append(errs, validateAttach(c.Attach)...)
	errs = append(errs, validateSecurity(&c.Security)...)
	errs = append(errs, validateLogging(&c.Logging)...)
	errs = append(errs, validateAccounting(&c.Accounting)...)
	errs = append(errs, validateLimits(&c.Limits)...)
	errs = append(errs, validateShutdown(&c.Shutdown)...)
	errs = append(errs, validateResponses(&c.Responses)...)
	errs = append(errs, validateRetry(&c.Retry)...)
	errs = append(errs, validateAgents(&c.Agents)...)
	// Nothing to serve is fine for an edge of attached models only, and
	// for a daemon that serves no clients.
	optional := c.Agents.Enabled || !c.ServesClients()
	errs = append(errs, validateBackends(c.Backends, c.Security.BackendNetwork, optional)...)
	errs = append(errs, validateModels(c.Models, c.Backends, optional)...)

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// validateTrustedProxies checks server.trusted_proxies (PLAN §18.1).
// Every entry must be able to match something: an unparseable CIDR, or
// an entry naming a listener this server does not have, is a silent
// no-op that leaves the operator believing a forwarded client address
// is honoured when it is not.
func validateTrustedProxies(s *Server) []string {
	var errs []string
	seen := make(map[string]bool, len(s.TrustedProxies))
	for _, entry := range s.TrustedProxies {
		switch {
		case seen[entry]:
			errs = append(errs, fmt.Sprintf("server.trusted_proxies: duplicate entry %q", entry))
		case entry == TrustedProxyUnix:
			if s.Listen.Network != "unix" {
				errs = append(errs, `server.trusted_proxies: "unix" requires server.listen.network: unix`)
			}
		default:
			p, err := netip.ParsePrefix(entry)
			switch {
			case err != nil:
				errs = append(errs, fmt.Sprintf("server.trusted_proxies: %q is not a CIDR like 127.0.0.1/32 (or the literal %q)", entry, TrustedProxyUnix))
			case p.Addr() != p.Masked().Addr():
				errs = append(errs, fmt.Sprintf("server.trusted_proxies: %q has bits set below the prefix length; write %q", entry, p.Masked().String()))
			case s.Listen.Network == "unix":
				errs = append(errs, fmt.Sprintf("server.trusted_proxies: %q cannot match a Unix listener; use %q", entry, TrustedProxyUnix))
			}
		}
		seen[entry] = true
	}
	return errs
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
		// Absolute, like every other path the daemon is handed: the
		// socket is resolved against the working directory otherwise,
		// and a sandbox policy that names it by the configured spelling
		// would grant a path the kernel never evaluates (PLAN §53.2).
		switch {
		case s.Listen.Address == "":
			errs = append(errs, "server.listen.address: required for network unix")
		case !filepath.IsAbs(s.Listen.Address):
			errs = append(errs, fmt.Sprintf("server.listen.address: %q must be an absolute path", s.Listen.Address))
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

	if s.AdminSocket != "" && !filepath.IsAbs(s.AdminSocket) {
		errs = append(errs, fmt.Sprintf("server.admin_socket: %q must be an absolute path", s.AdminSocket))
	}
	if s.AdminSocket != "" && s.AdminSocket == s.Listen.Address {
		errs = append(errs, "server.admin_socket: must differ from server.listen.address")
	}

	errs = append(errs, validateTrustedProxies(s)...)

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
	case sandbox.ModeRequired, sandbox.ModeBestEffort, sandbox.ModeDisabled:
	default:
		errs = append(errs, fmt.Sprintf("security.landlock.mode: %q must be required, best-effort, or disabled", l.Mode))
	}
	if l.MinimumABI < 1 || l.MinimumABI > landlock.MaxABI {
		errs = append(errs, fmt.Sprintf("security.landlock.minimum_abi: %d out of range 1..%d", l.MinimumABI, landlock.MaxABI))
	}

	switch s.Seatbelt.Mode {
	case sandbox.ModeRequired, sandbox.ModeBestEffort, sandbox.ModeDisabled:
	default:
		errs = append(errs, fmt.Sprintf("security.seatbelt.mode: %q must be required, best-effort, or disabled", s.Seatbelt.Mode))
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

func validateAgents(a *Agents) []string {
	if !a.Enabled {
		if a.MaxAttachments != 0 || a.MaxAttachmentsPerKey != 0 || a.MaxConcurrency != 0 || a.MaxModels != 0 ||
			a.QueueSize != 0 || a.QueueTimeout != 0 {
			return []string{"agents: limits are set but agents.enabled is false"}
		}
		return nil
	}
	var errs []string
	if a.MaxAttachments < 1 || a.MaxAttachments > maxAgentAttachments {
		errs = append(errs, fmt.Sprintf("agents.max_attachments: must be between 1 and %d, got %d", maxAgentAttachments, a.MaxAttachments))
	}
	if a.MaxAttachmentsPerKey < 1 || a.MaxAttachmentsPerKey > a.MaxAttachments {
		errs = append(errs, fmt.Sprintf("agents.max_attachments_per_key: must be between 1 and max_attachments (%d), got %d", a.MaxAttachments, a.MaxAttachmentsPerKey))
	}
	if a.MaxConcurrency < 1 || a.MaxConcurrency > maxServerConcurrency {
		errs = append(errs, fmt.Sprintf("agents.max_concurrency: must be between 1 and %d, got %d", maxServerConcurrency, a.MaxConcurrency))
	}
	if a.QueueSize < 1 || a.QueueSize > maxServerQueueSize {
		errs = append(errs, fmt.Sprintf("agents.queue_size: must be between 1 and %d, got %d", maxServerQueueSize, a.QueueSize))
	}
	if d := a.QueueTimeout.Duration(); d <= 0 || d > defaultRequestTimeout {
		errs = append(errs, fmt.Sprintf("agents.queue_timeout: must be > 0 and at most request_timeout (%s), got %s", defaultRequestTimeout, d))
	}
	if a.MaxModels < 1 || a.MaxModels > maxAgentModels {
		errs = append(errs, fmt.Sprintf("agents.max_models: must be between 1 and %d, got %d", maxAgentModels, a.MaxModels))
	}
	return errs
}

// AgentBackend is the backend configuration an attached agent is served
// under: the agents block's admission cap, the default queue, and the
// static timeouts every configured backend gets. Only those fields
// matter: an agent is reached over its own connection, so nothing is
// dialed, and the base URL only supplies the scheme the transport needs.
func AgentBackend(a Agents) Backend {
	return Backend{
		BaseURL:           "http://agent.invalid",
		HeaderTimeout:     Duration(defaultHeaderTimeout),
		RequestTimeout:    Duration(defaultRequestTimeout),
		StreamIdleTimeout: Duration(defaultStreamIdleTimeout),
		MaxConcurrency:    a.MaxConcurrency,
		QueueSize:         a.QueueSize,
		QueueTimeout:      a.QueueTimeout,
	}
}

// AgentNamespaceWildcard lets an agent key register models under the
// names its server advertises, without a namespace prefix.
const AgentNamespaceWildcard = "*"

// agentNamespacePattern is the grammar of an agent key's namespace: the
// first segment of every public name the key's agents register.
var agentNamespacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// ValidateAgentNamespace checks an agent key's namespace.
func ValidateAgentNamespace(ns string) error {
	if ns == AgentNamespaceWildcard || agentNamespacePattern.MatchString(ns) {
		return nil
	}
	return errors.New("invalid agent namespace: use 1-64 lowercase letters, digits, '.', '_' or '-', starting with a letter or digit, or * for no prefix")
}

// AgentModelName is the public name an attached model is served under:
// "<namespace>/<advertised id>", or the advertised id itself for the
// wildcard namespace.
func AgentModelName(ns, id string) string {
	if ns == AgentNamespaceWildcard {
		return id
	}
	return ns + "/" + id
}

// ValidAgentModelID narrows discovery's ID rules for attached models to
// the ASCII characters ids use in practice (vLLM, NIM, llama.cpp,
// Ollama): an agent's operator is not the edge's, and Unicode would let
// it register lookalikes of other models' names (PLAN §104.6). Aliases
// in an agent key's register list follow the same rule.
func ValidAgentModelID(id string) bool {
	if id == "" || len(id) > maxAgentModelIDBytes {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("._:@+/-", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// maxAgentModelIDBytes bounds an advertised id or alias (PLAN §104.4).
const maxAgentModelIDBytes = 256

// MaxAgentRegister bounds an agent key's register list.
const MaxAgentRegister = maxAgentModels

// AgentRegisteredName is the public name for one entry of an agent key's
// register list (PLAN §104.4): the alias when set, else the advertised
// id, under the key's namespace. The alias is a bare name (no "/"), and
// under the wildcard namespace the result may not contain "/" either,
// so a list cannot reach into another namespace.
func AgentRegisteredName(ns, id, alias string) (string, error) {
	if !ValidAgentModelID(id) {
		return "", fmt.Errorf("register id %q: use 1-256 ASCII letters, digits or ._:@+/-", id)
	}
	base := id
	if alias != "" {
		if !ValidAgentModelID(alias) || strings.Contains(alias, "/") {
			return "", fmt.Errorf("register alias %q: use 1-256 ASCII letters, digits or ._:@+- (no /)", alias)
		}
		base = alias
	}
	if ns == AgentNamespaceWildcard && strings.Contains(base, "/") {
		return "", fmt.Errorf("register id %q: a * agent key cannot register names containing /; give it an alias", id)
	}
	public := AgentModelName(ns, base)
	if err := ValidateModelName(public); err != nil {
		return "", fmt.Errorf("register %q: %v", base, err)
	}
	return public, nil
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

// validateBackends checks the configured backends. With agents enabled
// there may be none: an edge can serve attached models only.
func validateBackends(backends map[string]Backend, bn BackendNetwork, optional bool) []string {
	var errs []string
	if len(backends) == 0 && !optional {
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
			errs = append(errs, prefix+".base_url: is not a valid URL")
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
			errs = append(errs, fmt.Sprintf("%s.base_url: path %q must be empty or /", prefix, p))
		}

		// A trailing colon splits cleanly into a host and an empty port,
		// so it must be rejected explicitly. backend.parseBaseURL
		// refuses it too; catching it here keeps `config check` from
		// blessing a configuration `serve` then refuses.
		host, port, err := net.SplitHostPort(u.Host)
		if err != nil || host == "" || port == "" {
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

// ModelWildcard is the key ACL entry that grants every public model
// (PLAN §31).
const ModelWildcard = "*"

// ModelPrefixWildcardSuffix ends a key ACL entry that grants every model
// under a namespace: "exp1/*" grants "exp1/<anything>" (PLAN §104).
const ModelPrefixWildcardSuffix = "/*"

// ValidateModelName rejects a public model name that would mean
// something else once written into a key's model ACL: the wildcard
// itself, and a name holding the separator `key create --models` splits
// on. Both discovery and configuration validation apply it, so a name an
// inference server advertises can never reach an ACL as a sentinel.
func ValidateModelName(name string) error {
	switch {
	case name == ModelWildcard:
		return fmt.Errorf("%q is the ACL wildcard and cannot name a model", ModelWildcard)
	case strings.Contains(name, ","):
		return errors.New("a model name cannot contain a comma, which separates entries in a key's model list")
	case strings.HasSuffix(name, ModelPrefixWildcardSuffix):
		return fmt.Errorf("a model name cannot end in %q, which grants a namespace in a key's model list", ModelPrefixWildcardSuffix)
	}
	return nil
}

func validateModels(models map[string]Model, backends map[string]Backend, optional bool) []string {
	var errs []string
	if len(models) == 0 && !optional {
		return []string{"models: at least one public model is required"}
	}

	for name, m := range models {
		prefix := fmt.Sprintf("models.%s", name)

		if err := ValidateModelName(name); err != nil {
			errs = append(errs, prefix+": "+err.Error())
		}

		switch m.Type {
		case "generation", "embedding", "decision":
		default:
			errs = append(errs, fmt.Sprintf("%s.type: %q must be generation, embedding, or decision", prefix, m.Type))
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
		// A decision model emits no tokens, so an output cap is
		// meaningless on it (SYSTEMONE_PLAN §D2); refusing it keeps a
		// generation policy copy-pasted onto a decision model from
		// reading as a limit.
		if m.Type == "decision" && m.Policy.MaxOutputTokens != 0 {
			errs = append(errs, prefix+".policy.max_output_tokens: not meaningful on a decision model")
		}
	}

	return errs
}

func isLoopbackHost(host string) bool {
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

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
