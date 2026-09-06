// Package config loads, normalizes, and validates Mellomting configuration.
//
// Configuration files are trusted local inputs but are still parsed
// defensively (PLAN §28): size-bounded, alias- and custom-tag-free YAML,
// strict known-field decoding, and fail-closed validation. Every limit
// has a finite default (PLAN §9.1).
package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultConfigPath is the default main configuration path.
	DefaultConfigPath = "/etc/mellomting/config.yaml"
	// DefaultUsersFile is the default API-key store path (PLAN §26).
	DefaultUsersFile = "/etc/mellomting/users.yaml"
	// DefaultPepperFile is the default HMAC pepper path (PLAN §27).
	DefaultPepperFile = "/etc/mellomting/auth.pepper"
	// DefaultAccountingPath is the default usage JSONL path (PLAN §76).
	DefaultAccountingPath = "/var/log/mellomting/usage.jsonl"
	// DefaultUnixSocketMode is the default Unix socket mode (PLAN §8.1);
	// listen.mode may be omitted and defaults to this.
	DefaultUnixSocketMode = "0660"

	// MaxFileBytes bounds the size of a configuration file (PLAN §28).
	MaxFileBytes = 1 << 20
)

// Config is the normalized runtime configuration (PLAN §76). It is not the
// operator-facing source schema (D11): Parse decodes the source document
// (sourceDoc) and deterministically normalizes it (D12) into this value.
// Backends here are the synthetic internal backends derived from the
// operator's servers; the source form is rejected and no source-only state
// is retained.
type Config struct {
	Version    int                `yaml:"version"`
	Server     Server             `yaml:"server"`
	Auth       Auth               `yaml:"auth"`
	Security   Security           `yaml:"security"`
	Logging    Logging            `yaml:"logging"`
	Accounting Accounting         `yaml:"accounting"`
	Limits     Limits             `yaml:"limits"`
	Shutdown   Shutdown           `yaml:"shutdown"`
	Responses  Responses          `yaml:"responses"`
	Retry      Retry              `yaml:"retry"`
	Backends   map[string]Backend `yaml:"backends"`
	Models     map[string]Model   `yaml:"models"`
}

// Responses bounds the in-memory Responses API affinity table (PLAN §21).
// The mapping is cleared on restart; it is not persisted in v1.
type Responses struct {
	AffinityTTL        Duration `yaml:"affinity_ttl"`
	MaxAffinityEntries int      `yaml:"max_affinity_entries"`
}

// Retry bounds conservative pre-stream retries and fallbacks
// (PLAN §23). max_attempts is the total number of attempts a request may
// make across all backends; the default of 1 means "no retry unless
// explicitly enabled".
type Retry struct {
	MaxAttempts    int      `yaml:"max_attempts"`
	InitialBackoff Duration `yaml:"initial_backoff"`
	MaxBackoff     Duration `yaml:"max_backoff"`
	// Jitter enables full jitter on the exponential backoff. A nil
	// pointer means the default (true).
	Jitter *bool `yaml:"jitter,omitempty"`
}

// JitterEnabled reports whether backoff jitter is on (default true).
func (r Retry) JitterEnabled() bool { return r.Jitter == nil || *r.Jitter }

// Shutdown controls graceful shutdown (PLAN §74).
type Shutdown struct {
	GracePeriod Duration `yaml:"grace_period"`
}

// Duration wraps time.Duration for strict YAML parsing (PLAN §28):
// values must carry an explicit unit, e.g. "30s".
type Duration time.Duration

// Duration returns the wrapped time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// MarshalYAML renders a duration with its unit (e.g. "30s") so
// `config show-effective` stays human-readable.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// UnmarshalYAML decodes a strict duration scalar from YAML.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("malformed duration: value must be a string with a unit, e.g. 30s")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("malformed duration %q: must be a number with a unit, e.g. 30s", s)
	}
	*d = Duration(parsed)
	return nil
}

// Server is the listen and HTTP limit configuration (PLAN §8, §9).
type Server struct {
	Listen                  Listen `yaml:"listen"`
	TLS                     TLS    `yaml:"tls,omitempty"`
	MaxHeaderBytes          int    `yaml:"max_header_bytes"`
	MaxBodyBytes            int    `yaml:"max_body_bytes"`
	MaxResponseBytes        int    `yaml:"max_response_bytes"`
	MaxInflightRequests     int    `yaml:"max_inflight_requests"`
	MaxBufferedRequestBytes int    `yaml:"max_buffered_request_bytes"`
	// MaxConnections caps concurrently accepted connections at the
	// listener so a flood of idle sockets cannot exhaust fds (T-L15,
	// PLAN §9.1). Excess connections are closed immediately.
	MaxConnections int `yaml:"max_connections"`

	ReadHeaderTimeout  Duration `yaml:"read_header_timeout"`
	ReadBodyTimeout    Duration `yaml:"read_body_timeout"`
	IdleTimeout        Duration `yaml:"idle_timeout"`
	StreamIdleTimeout  Duration `yaml:"stream_idle_timeout"`
	StreamWriteTimeout Duration `yaml:"stream_write_timeout"`

	// AllowPlaintextNonLoopback opts in to a plaintext non-loopback TCP
	// listener when TLS is not configured (PLAN §8.2).
	AllowPlaintextNonLoopback bool `yaml:"allow_plaintext_non_loopback,omitempty"`

	// TrustedProxies lists the peers whose forwarded client address the
	// ingress may believe (PLAN §18.1). Each entry is a CIDR, or the
	// literal "unix" for the peer of a Unix-socket listener, which has
	// no address of its own — the reverse proxy is whoever may open the
	// socket. Empty (the default) means X-Forwarded-For is ignored
	// entirely and the socket peer is the client.
	//
	// This governs per-source pre-auth rate limiting and the address in
	// operator logs. It never affects authentication or authorization,
	// which depend on the API key alone.
	TrustedProxies []string `yaml:"trusted_proxies,omitempty"`
}

// TrustedProxyUnix is the TrustedProxies entry naming the peer of a
// Unix-socket listener.
const TrustedProxyUnix = "unix"

// Listen selects the ingress listener (PLAN §8.1).
type Listen struct {
	Network string `yaml:"network"` // "unix" or "tcp"
	Address string `yaml:"address"`
	// Mode is the Unix socket mode, e.g. "0660". Optional; defaults to
	// DefaultUnixSocketMode for network unix.
	Mode string `yaml:"mode,omitempty"`
}

// TLS is native listener TLS (PLAN §67). Configuring either file field
// enables static TLS loaded from those files before the sandbox is applied
// (PLAN §57 step 9); a restart is required to reload (PLAN §67). Both files
// are required together (D11). Native ACME (PLAN §68) is shelved and its
// source fields (mode, hostname, email) are rejected as unknown.
type TLS struct {
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
}

// Auth selects the key store and pepper files (PLAN §26, §27).
type Auth struct {
	UsersFile  string `yaml:"users_file"`
	PepperFile string `yaml:"pepper_file"`
}

// Security holds backend network and sandbox policy (PLAN §16, §55).
type Security struct {
	BackendNetwork BackendNetwork `yaml:"backend_network"`
	Landlock       Landlock       `yaml:"landlock"`
	Seatbelt       Seatbelt       `yaml:"seatbelt,omitempty"`
}

// BackendNetwork restricts where backend connections may go (PLAN §16).
type BackendNetwork struct {
	Mode string `yaml:"mode"` // "loopback-only", "allowed-cidrs", or "any"
	// CIDRs is the allowed address set for mode "allowed-cidrs".
	CIDRs []string `yaml:"cidrs,omitempty"`
}

// Landlock is the sandbox policy (PLAN §55).
type Landlock struct {
	Mode       string `yaml:"mode"` // "required", "best-effort", or "disabled"
	MinimumABI int    `yaml:"minimum_abi"`
}

// Seatbelt is the sandbox policy on macOS (PLAN §55), the counterpart
// of Landlock on Linux. Only the one for the running platform is
// consulted, so a configuration can carry both and be served anywhere.
//
// It defaults to disabled where Landlock defaults to required: macOS is
// a development platform for this daemon, not a deployment target, and
// its confinement has not been proven on the range of macOS releases
// that Landlock has on Linux kernels. Enable it deliberately, after
// `mellomting sandbox check` reports it available on that host.
type Seatbelt struct {
	Mode string `yaml:"mode"` // "required", "best-effort", or "disabled"
}

// Logging configures operational logs (PLAN §43).
type Logging struct {
	Format string `yaml:"format"`
	Level  string `yaml:"level"`
}

// Accounting configures token-usage JSONL recording (PLAN §38-42).
type Accounting struct {
	Enabled           bool     `yaml:"enabled"`
	Path              string   `yaml:"path,omitempty"`
	EnsureStreamUsage *bool    `yaml:"ensure_stream_usage,omitempty"`
	ReplayOnStart     *bool    `yaml:"replay_on_start,omitempty"`
	ReplayMaxBytes    int64    `yaml:"replay_max_bytes,omitempty"`
	QueueSize         int      `yaml:"queue_size,omitempty"`
	Overflow          string   `yaml:"overflow,omitempty"`
	FSync             string   `yaml:"fsync,omitempty"`
	FSyncInterval     Duration `yaml:"fsync_interval,omitempty"`
	// UnknownUsageReservation is the fixed token charge applied to a
	// successful request whose usage is unknown (PLAN §39). It is a
	// conservative total (covers input and output). 0 falls back to the
	// model's configured output cap.
	UnknownUsageReservation int64 `yaml:"unknown_usage_reservation,omitempty"`
}

// Limits holds the global admission limits (PLAN §34).
type Limits struct {
	GlobalRequestsPerSecond float64 `yaml:"global_requests_per_second"`
	GlobalBurst             int     `yaml:"global_burst"`
	// PreauthRequestsPerSecond bounds pre-auth requests per source IP
	// (PLAN §33): a bogus-token flood from one host is throttled before
	// authentication and before it can drain the shared authenticated
	// bucket.
	PreauthRequestsPerSecond float64 `yaml:"preauth_requests_per_second"`
	PreauthBurst             int     `yaml:"preauth_burst"`
	// AuthFailureLogRate bounds invalid-auth warn-line output during a
	// flood (PLAN §33): at most this many lines per second, each
	// carrying the count of suppressed attempts.
	AuthFailureLogRate float64 `yaml:"auth_failure_log_rate"`
}

// Backend is one OpenAI-compatible inference server (PLAN §15, §17).
type Backend struct {
	BaseURL       string `yaml:"base_url"`
	UpstreamModel string `yaml:"upstream_model"`
	// APIKeyFile holds the backend credential, if any (PLAN §17). The
	// file value itself is never logged; only the path is configuration.
	APIKeyFile        string   `yaml:"api_key_file"`
	ConnectTimeout    Duration `yaml:"connect_timeout"`
	HeaderTimeout     Duration `yaml:"header_timeout"`
	RequestTimeout    Duration `yaml:"request_timeout"`
	StreamIdleTimeout Duration `yaml:"stream_idle_timeout"`
	MaxConcurrency    int      `yaml:"max_concurrency"`
	QueueSize         int      `yaml:"queue_size"`
	QueueTimeout      Duration `yaml:"queue_timeout"`
}

// Model is one public model alias (PLAN §13, §19).
type Model struct {
	Type     string       `yaml:"type"` // "generation" or "embedding"
	Strategy string       `yaml:"strategy"`
	Policy   ModelPolicy  `yaml:"policy"`
	Backends []BackendRef `yaml:"backends"`
}

// ModelPolicy is per-model request policy.
type ModelPolicy struct {
	MaxOutputTokens int `yaml:"max_output_tokens,omitempty"`
}

// BackendRef names a backend plus an optional routing weight. It accepts
// either a plain scalar ("qwen-a") or a mapping ({name, weight}).
type BackendRef struct {
	Name   string `yaml:"name"`
	Weight int    `yaml:"weight"`
}

// UnmarshalYAML accepts a scalar or mapping for a backend reference.
func (r *BackendRef) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var s string
		if err := node.Decode(&s); err != nil {
			return fmt.Errorf("malformed backend reference: %w", err)
		}
		r.Name = s
		return nil
	}
	var m struct {
		Name   string `yaml:"name"`
		Weight int    `yaml:"weight"`
	}
	if err := node.Decode(&m); err != nil {
		return fmt.Errorf("malformed backend reference: %w", err)
	}
	r.Name = m.Name
	r.Weight = m.Weight
	return nil
}
