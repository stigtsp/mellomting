package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
servers:
  qwen-a:
    url: http://127.0.0.1:8001
models:
  qwen-coder:
    upstream_model: Qwen/Qwen3-Coder-Next
    servers:
    - qwen-a
`

func TestParsePlan76SampleConfig(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "plan76.yaml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse plan §76 sample: %v", err)
	}
	if len(cfg.Backends) != 2 || len(cfg.Models) != 1 {
		t.Fatalf("parsed %d backends, %d models; want 2/1",
			len(cfg.Backends), len(cfg.Models))
	}
	if cfg.Server.MaxInflightRequests != 32 {
		t.Fatalf("explicit limit lost: max_inflight = %d, want 32", cfg.Server.MaxInflightRequests)
	}
	if cfg.Models["qwen-coder"].Strategy != "least-inflight" {
		t.Fatalf("model strategy = %q, want least-inflight", cfg.Models["qwen-coder"].Strategy)
	}
}

func TestParseMinimalConfigAppliesDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("Parse minimal: %v", err)
	}

	if cfg.Server.MaxHeaderBytes != 32768 || cfg.Server.MaxBodyBytes != 16777216 {
		t.Fatalf("server byte limits not defaulted: %+v", cfg.Server)
	}
	if cfg.Server.MaxInflightRequests != 64 || cfg.Server.MaxBufferedRequestBytes != 67108864 {
		t.Fatalf("server request limits not defaulted: %+v", cfg.Server)
	}
	if cfg.Server.MaxConnections != 1024 {
		t.Fatalf("server.max_connections not defaulted: %+v", cfg.Server)
	}
	if cfg.Server.ReadHeaderTimeout.Duration() != 5*time.Second ||
		cfg.Server.StreamIdleTimeout.Duration() != 120*time.Second {
		t.Fatalf("server timeouts not defaulted: %+v", cfg.Server)
	}
	if cfg.Auth.UsersFile != DefaultUsersFile || cfg.Auth.PepperFile != DefaultPepperFile {
		t.Fatalf("auth paths not defaulted: %+v", cfg.Auth)
	}
	if cfg.Security.BackendNetwork.Mode != "loopback-only" ||
		cfg.Security.Landlock.Mode != "best-effort" || cfg.Security.Landlock.MinimumABI != 6 ||
		cfg.Security.Seatbelt.Mode != "best-effort" {
		t.Fatalf("security defaults wrong: %+v", cfg.Security)
	}
	if cfg.Logging.Format != "json" || cfg.Logging.Level != "info" {
		t.Fatalf("logging defaults wrong: %+v", cfg.Logging)
	}
	if cfg.Limits.GlobalRequestsPerSecond != 100 || cfg.Limits.GlobalBurst != 200 {
		t.Fatalf("limits defaults wrong: %+v", cfg.Limits)
	}
	if cfg.Limits.PreauthRequestsPerSecond != 100 || cfg.Limits.PreauthBurst != 200 ||
		cfg.Limits.AuthFailureLogRate != 1.0 {
		t.Fatalf("preauth/limits defaults wrong: %+v", cfg.Limits)
	}
	if cfg.Retry.MaxAttempts != 1 ||
		cfg.Retry.InitialBackoff.Duration() != 100*time.Millisecond ||
		cfg.Retry.MaxBackoff.Duration() != time.Second ||
		!cfg.Retry.JitterEnabled() {
		t.Fatalf("retry defaults wrong: %+v", cfg.Retry)
	}

	b := cfg.Backends[defaultBackendName("qwen-coder", "qwen-a")]
	if b.ConnectTimeout.Duration() != 3*time.Second ||
		b.HeaderTimeout.Duration() != 30*time.Second ||
		b.RequestTimeout.Duration() != 20*time.Minute ||
		b.StreamIdleTimeout.Duration() != 120*time.Second ||
		b.MaxConcurrency != 4 || b.QueueSize != 8 || b.QueueTimeout.Duration() != 5*time.Second {
		t.Fatalf("backend defaults wrong: %+v", b)
	}

	m := cfg.Models["qwen-coder"]
	if m.Type != "generation" || m.Strategy != "single" || m.Backends[0].Weight != 1 {
		t.Fatalf("model defaults wrong: %+v", m)
	}
}

func TestParseUnixSocketModeDefault(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(`
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`))
	if err != nil {
		t.Fatalf("Parse without listen.mode: %v", err)
	}
	if cfg.Server.Listen.Mode != DefaultUnixSocketMode {
		t.Fatalf("listen.mode = %q, want default %q", cfg.Server.Listen.Mode, DefaultUnixSocketMode)
	}
}

func TestParseRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{name: "empty document", yaml: "", wantErr: "empty"},
		{name: "scalar root", yaml: "42", wantErr: "cannot unmarshal"},
		{name: "duplicate top-level key", yaml: "version: 1\nversion: 2", wantErr: "already defined"},
		{
			name: "multi-document config rejected",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: "0660"
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
---
security:
  something: bad
`,
			wantErr: "multi-document",
		},
		{
			name: "duplicate nested key",
			yaml: `
version: 1
limits:
  global_burst: 5
  global_burst: 6
` + restOfConfig,
			wantErr: "already defined",
		},
		{name: "unsupported version", yaml: "version: 2\n" + restOfConfig, wantErr: "not supported (want 1)"},
		{name: "unknown top-level field", yaml: "version: 1\nbanana: 1\n" + restOfConfig, wantErr: "field banana not found"},
		{
			name: "unknown nested field",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
  bogus: true
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "field bogus not found",
		},
		{
			name:    "retry max_attempts out of range",
			yaml:    "version: 1\nretry:\n  max_attempts: 9\n" + restOfConfig,
			wantErr: "retry.max_attempts",
		},
		{
			name:    "retry backoff inverted",
			yaml:    "version: 1\nretry:\n  initial_backoff: 5s\n  max_backoff: 1s\n" + restOfConfig,
			wantErr: "retry.initial_backoff",
		},
		{
			name: "YAML alias rejected",
			yaml: `
version: 1
listen: &l
  network: unix
  address: /run/mellomting/mellomting.sock
  mode: "0660"
server:
  listen: *l
`,
			wantErr: "aliases",
		},
		{
			name: "custom tag rejected",
			yaml: `
version: 1
server: !bogus
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: "0660"
`,
			wantErr: "tag",
		},
		{
			name: "duration without unit",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
  read_header_timeout: 5
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "malformed duration",
		},
		{
			name: "duplicate backend reference",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
    - qa
`,
			wantErr: "duplicate server",
		},
		{
			name: "unknown backend reference",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    servers:
    - missing
`,
			wantErr: "unknown server",
		},
		{
			name: "malformed backend_network cidr fails closed",
			yaml: `
version: 1
` + minimalServer + `
security:
  backend_network:
    mode: allowed-cidrs
    cidrs:
    - 10.0.0.0/8
    - not-a-cidr
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "not a valid CIDR",
		},
		{
			name: "unknown qualifier reference",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    qualifier: ghost
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "field qualifier not found",
		},
		{
			name: "strategy single with two backends",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://127.0.0.1:8001
  qb:
    url: http://127.0.0.1:8002
models:
  m1:
    strategy: single
    upstream_model: M
    servers:
    - qa
    - qb
`,
			wantErr: "exactly one backend",
		},
		{
			name: "model without backends",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1: {}
`,
			wantErr: "at least one server is required",
		},
		{
			name: "non-loopback backend in loopback-only mode",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://vllm.internal:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "not a loopback IP literal",
		},
		{
			name: "backend url query rejected",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://127.0.0.1:8001/?x=1
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "query strings are not supported",
		},
		{
			name: "backend url fragment rejected",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://127.0.0.1:8001#frag
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "fragments are not supported",
		},
		{
			name: "backend url userinfo rejected",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://user@127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "userinfo",
		},
		{
			name: "backend url path rejected",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://127.0.0.1:8001/v1
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "must be empty or /",
		},
		{
			name: "backend url scheme rejected",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: ftp://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "unsupported scheme",
		},
		{
			name: "tcp listener without port",
			yaml: `
version: 1
server:
  listen:
    network: tcp
    address: 127.0.0.1
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "host:port",
		},
		{
			name: "plaintext non-loopback tcp rejected",
			yaml: `
version: 1
server:
  listen:
    network: tcp
    address: 10.0.0.5:8080
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "allow_plaintext_non_loopback",
		},
		{
			name: "tcp listener with listen.mode rejected",
			yaml: `
version: 1
server:
  listen:
    network: tcp
    address: 127.0.0.1:8080
    mode: '0660'
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "server.listen.mode: only valid for network unix",
		},
		{
			name: "plaintext localhost tcp rejected too",
			yaml: `
version: 1
server:
  listen:
    network: tcp
    address: localhost:8080
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "allow_plaintext_non_loopback",
		},
		{
			name: "non-loopback tcp accepted with opt-in",
			yaml: `
version: 1
server:
  listen:
    network: tcp
    address: 10.0.0.5:8080
  allow_plaintext_non_loopback: true
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "",
		},
		{
			name: "non-loopback tcp accepted with tls",
			yaml: `
version: 1
server:
  listen:
    network: tcp
    address: 10.0.0.5:8443
  tls:
    cert_file: /etc/mellomting/tls/cert.pem
    key_file: /etc/mellomting/tls/key.pem
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "",
		},
		{
			name: "unix socket without mode defaults",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "",
		},
		{
			name: "tls on unix listener rejected",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
  tls:
    cert_file: /etc/mellomting/tls/cert.pem
    key_file: /etc/mellomting/tls/key.pem
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "only valid when listen network is tcp",
		},
		{
			name: "bad socket mode",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: "09999"
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "octal mode",
		},
		{
			name: "negative limit",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
  max_body_bytes: -1
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "max_body_bytes: must be > 0",
		},
		{
			name: "allowed-cidrs without cidrs",
			yaml: `
version: 1
` + minimalServer + `
security:
  backend_network:
    mode: allowed-cidrs
servers:
  qa:
    url: http://10.1.0.4:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "cidrs: required",
		},
		{
			name: "allowed-cidrs accepts matching literal",
			yaml: `
version: 1
` + minimalServer + `
security:
  backend_network:
    mode: allowed-cidrs
    cidrs:
    - 10.1.0.0/24
servers:
  qa:
    url: http://10.1.0.4:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "",
		},
		{
			name: "allowed-cidrs rejects out-of-range literal",
			yaml: `
version: 1
` + minimalServer + `
security:
  backend_network:
    mode: allowed-cidrs
    cidrs:
    - 10.1.0.0/24
servers:
  qa:
    url: http://10.9.9.9:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "not within",
		},
		{
			name: "accounting disabled but fields set",
			yaml: `
version: 1
` + minimalServer + `
accounting:
  enabled: false
  path: /var/log/mellomting/usage.jsonl
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "must be empty when enabled is false",
		},
		{
			name: "accounting bad overflow",
			yaml: `
version: 1
` + minimalServer + `
accounting:
  enabled: true
  overflow: panic
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "overflow",
		},
		{
			name: "relative users_file rejected",
			yaml: `
version: 1
` + minimalServer + `
auth:
  users_file: ./users.yaml
  pepper_file: /etc/mellomting/auth.pepper
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "auth.users_file: must be an absolute path",
		},
		{
			name: "relative pepper_file rejected",
			yaml: `
version: 1
` + minimalServer + `
auth:
  users_file: /etc/mellomting/users.yaml
  pepper_file: auth.pepper
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "auth.pepper_file: must be an absolute path",
		},
		{
			name: "accounting negative reservation",
			yaml: `
version: 1
` + minimalServer + `
accounting:
  enabled: true
  unknown_usage_reservation: -5
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "unknown_usage_reservation",
		},
		{
			name: "accounting negative reservation while disabled",
			yaml: `
version: 1
` + minimalServer + `
accounting:
  enabled: false
  unknown_usage_reservation: -5
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "unknown_usage_reservation",
		},
		{
			name: "accounting disabled with quota settings accepted",
			yaml: `
version: 1
` + minimalServer + `
accounting:
  enabled: false
  ensure_stream_usage: true
  unknown_usage_reservation: 500
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "",
		},
		{
			name: "bad landlock mode",
			yaml: `
version: 1
` + minimalServer + `
security:
  landlock:
    mode: off
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "landlock.mode",
		},
		{
			name: "landlock abi above library max",
			yaml: `
version: 1
` + minimalServer + `
security:
  landlock:
    minimum_abi: 99
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "minimum_abi",
		},
		{
			name: "no servers",
			yaml: `
version: 1
` + minimalServer + `
models:
  m1:
    servers: []
`,
			wantErr: "at least one server is required",
		},
		{
			name: "no models",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
servers:
  qa:
    url: http://127.0.0.1:8001
`,
			wantErr: "models: at least one model is required",
		},
		{
			name: "unix listener without address",
			yaml: `
version: 1
server:
  listen:
    network: unix
    mode: '0660'
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "address: required for network unix",
		},
		{
			name: "missing listen network",
			yaml: `
version: 1
server:
  listen:
    address: /run/mellomting/mellomting.sock
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "server.listen.network: required",
		},
		{
			name: "listen mode only for network unix",
			yaml: `
version: 1
server:
  listen:
    address: /run/mellomting/mellomting.sock
    mode: '0660'
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "listen.mode: only valid for network unix",
		},
		{
			name: "max_header_bytes non-positive",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
  max_header_bytes: -1
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "max_header_bytes: must be > 0",
		},
		{
			name: "max_response_bytes non-positive",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
  max_response_bytes: -1
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "max_response_bytes: must be > 0",
		},
		{
			name: "max_inflight_requests non-positive",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
  max_inflight_requests: -1
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "max_inflight_requests: must be > 0",
		},
		{
			name: "max_buffered_request_bytes non-positive",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
  max_buffered_request_bytes: -1
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "max_buffered_request_bytes: must be > 0",
		},
		{
			name: "max_connections non-positive",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
  max_connections: -1
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "max_connections: must be > 0",
		},
		{
			name: "read_header_timeout non-positive",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
  read_header_timeout: -1s
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "read_header_timeout: must be > 0",
		},
		{
			name: "read_body_timeout non-positive",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
  read_body_timeout: -1s
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "read_body_timeout: must be > 0",
		},
		{
			name: "idle_timeout non-positive",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
  idle_timeout: -1s
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "idle_timeout: must be > 0",
		},
		{
			name: "stream_idle_timeout non-positive",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
  stream_idle_timeout: -1s
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "stream_idle_timeout: must be > 0",
		},
		{
			name: "stream_write_timeout non-positive",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
  stream_write_timeout: -1s
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "stream_write_timeout: must be > 0",
		},
		{
			name: "tls cert without key rejected",
			yaml: `
version: 1
server:
  listen:
    network: tcp
    address: 127.0.0.1:8080
  tls:
    cert_file: /etc/mellomting/tls/cert.pem
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "cert_file and key_file are required",
		},
		{
			name: "tls mode field rejected as unknown",
			yaml: `
version: 1
server:
  listen:
    network: tcp
    address: 127.0.0.1:8080
  tls:
    mode: acme
    hostname: llm.example.net
    email: admin@example.net
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "field mode not found",
		},
		{
			name: "backend cidrs with loopback-only mode",
			yaml: `
version: 1
` + minimalServer + `
security:
  backend_network:
    mode: loopback-only
    cidrs:
    - 10.0.0.0/8
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "only valid when mode is allowed-cidrs",
		},
		{
			name: "backend_network mode invalid",
			yaml: `
version: 1
` + minimalServer + `
security:
  backend_network:
    mode: nope
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "must be loopback-only, allowed-cidrs, or any",
		},
		{
			name: "logging format invalid",
			yaml: `
version: 1
` + minimalServer + `
logging:
  format: xml
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "logging.format",
		},
		{
			name: "logging level invalid",
			yaml: `
version: 1
` + minimalServer + `
logging:
  level: loud
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "logging.level",
		},
		{
			name: "accounting replay_max_bytes negative",
			yaml: `
version: 1
` + minimalServer + `
accounting:
  enabled: true
  replay_max_bytes: -1
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "replay_max_bytes: must be >= 0",
		},
		{
			name: "accounting fsync_interval only when interval",
			yaml: `
version: 1
` + minimalServer + `
accounting:
  enabled: true
  fsync: never
  fsync_interval: 1s
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "fsync_interval: only valid when fsync is interval",
		},
		{
			name: "accounting fsync never without interval",
			yaml: `
version: 1
` + minimalServer + `
accounting:
  enabled: true
  fsync: never
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
		},
		{
			name: "accounting fsync every without interval",
			yaml: `
version: 1
` + minimalServer + `
accounting:
  enabled: true
  fsync: every
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
		},
		{
			name: "accounting fsync invalid",
			yaml: `
version: 1
` + minimalServer + `
accounting:
  enabled: true
  fsync: always
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "accounting.fsync",
		},
		{
			name: "negative global_requests_per_second",
			yaml: `
version: 1
` + minimalServer + `
limits:
  global_requests_per_second: -1
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "global_requests_per_second",
		},
		{
			name: "negative global_burst",
			yaml: `
version: 1
` + minimalServer + `
limits:
  global_burst: -1
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "global_burst",
		},
		{
			name: "negative preauth_requests_per_second",
			yaml: `
version: 1
` + minimalServer + `
limits:
  preauth_requests_per_second: -1
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "preauth_requests_per_second",
		},
		{
			name: "negative preauth_burst",
			yaml: `
version: 1
` + minimalServer + `
limits:
  preauth_burst: -1
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "preauth_burst",
		},
		{
			name: "negative auth_failure_log_rate",
			yaml: `
version: 1
` + minimalServer + `
limits:
  auth_failure_log_rate: -1
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "auth_failure_log_rate",
		},
		{
			name: "negative shutdown grace_period",
			yaml: `
version: 1
` + minimalServer + `
shutdown:
  grace_period: -1s
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "grace_period",
		},
		{
			name: "negative responses affinity_ttl",
			yaml: `
version: 1
` + minimalServer + `
responses:
  affinity_ttl: -1s
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "affinity_ttl",
		},
		{
			name: "negative responses max_affinity_entries",
			yaml: `
version: 1
` + minimalServer + `
responses:
  max_affinity_entries: -1
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "max_affinity_entries",
		},
		{
			name: "retry negative initial_backoff",
			yaml: `
version: 1
` + minimalServer + `
retry:
  initial_backoff: -1s
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "retry.initial_backoff",
		},
		{
			name: "retry negative max_backoff",
			yaml: `
version: 1
` + minimalServer + `
retry:
  max_backoff: -1s
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "retry.max_backoff",
		},
		{
			// A relative socket path resolves against the working
			// directory, and a sandbox rule naming it would grant a
			// path the kernel never evaluates.
			name: "relative unix listen address",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: m.sock
    mode: "0660"
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "must be an absolute path",
		},
		{
			name: "seatbelt mode invalid",
			yaml: `
version: 1
` + minimalServer + `
security:
  seatbelt:
    mode: sometimes
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "security.seatbelt.mode",
		},
		{
			// PLAN §18.1: an entry that cannot match is a silent no-op
			// that leaves the operator believing a forwarded client
			// address is honoured when it is not.
			name: "trusted_proxies entry is not a CIDR",
			yaml: `
version: 1
server:
  listen:
    network: tcp
    address: 127.0.0.1:8080
  trusted_proxies:
    - 127.0.0.1
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "is not a CIDR",
		},
		{
			name: "trusted_proxies host bits set",
			yaml: `
version: 1
server:
  listen:
    network: tcp
    address: 127.0.0.1:8080
  trusted_proxies:
    - 10.0.0.7/8
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "bits set below the prefix length",
		},
		{
			name: "trusted_proxies unix entry without a unix listener",
			yaml: `
version: 1
server:
  listen:
    network: tcp
    address: 127.0.0.1:8080
  trusted_proxies:
    - unix
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: `"unix" requires server.listen.network: unix`,
		},
		{
			name: "trusted_proxies cidr with a unix listener",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: "0660"
  trusted_proxies:
    - 127.0.0.1/32
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "cannot match a Unix listener",
		},
		{
			// A model named "*" is the ACL wildcard: a key created for
			// that one model would be granted every model instead. A
			// name is attacker-supplied when it comes from an inference
			// server's /v1/models during init.
			name: "model named as the ACL wildcard",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  "*":
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "ACL wildcard",
		},
		{
			name: "model name with a comma",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  "a,b":
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "cannot contain a comma",
		},
		{
			name: "model type invalid",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    type: weird
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "must be generation or embedding",
		},
		{
			name: "model strategy invalid",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    strategy: nope
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "not a supported routing strategy",
		},
		{
			name: "model empty backend reference",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    servers:
    - ''
`,
			wantErr: "unknown server",
		},
		{
			name: "model negative max_output_tokens",
			yaml: `
version: 1
` + minimalServer + `
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    policy:
      max_output_tokens: -1
    upstream_model: M
    servers:
    - qa
`,
			wantErr: "max_output_tokens: must be >= 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse([]byte(tc.yaml))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Parse succeeded, want %q error; got %+v", tc.wantErr, cfg)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}
		})
	}
}

// TestMalformedBaseURLCredentialsRedacted is the T-M13 regression test:
// a malformed base_url with inlined credentials must never echo the
// password (or the userinfo) in a validation error, whether it failed to
// parse or was rejected by the well-formed-userinfo check.
func TestMalformedBaseURLCredentialsRedacted(t *testing.T) {
	const secret = "s3cr3t-pw"
	cases := []string{
		"http://admin:" + secret + "@host\x7f",          // parse failure (control byte) — the T-M13 case
		"http://admin:" + secret + "@",                  // parses; missing host
		"://admin:" + secret + "@host:8000",             // missing scheme (parse failure)
		"http://admin:" + secret + "@127.0.0.1:8001",    // well-formed userinfo branch
		"http://user:" + secret + "@vllm.internal:8001", // well-formed userinfo branch
	}
	for _, base := range cases {
		cfg := fmt.Sprintf("version: 1\n%s\nservers:\n  qa:\n    url: %q\nmodels:\n  m1:\n    upstream_model: M\n    servers: [qa]\n", minimalServer, base)
		if _, err := Parse([]byte(cfg)); err == nil {
			t.Fatalf("Parse succeeded for %q; want a validation error", base)
		} else if strings.Contains(err.Error(), secret) {
			t.Fatalf("validation error leaks password for %q: %s", base, err)
		}
	}
}

const restOfConfig = `
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: '0660'
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`

const minimalServer = `
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: "0660"
`

func TestLoadFromDisk(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen.Address != "/run/mellomting/mellomting.sock" {
		t.Fatalf("listen address = %q", cfg.Server.Listen.Address)
	}

	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("directory accepted")
	}
}

func TestLoadRejectsOversizedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "big.yaml")
	data := make([]byte, MaxFileBytes+1)
	for i := range data {
		data[i] = '#'
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "maximum size") {
		t.Fatalf("oversized file: err = %v, want maximum size error", err)
	}
}

// TestParseWildcardListener covers D17: an empty host in a TCP listen
// address (:PORT) is a valid wildcard bind, subject to the same
// TLS-or-explicit-plaintext policy as 0.0.0.0 and [::].
func TestParseWildcardListener(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		address string
		tls     bool
		plain   bool
		wantErr bool
	}{
		{name: "port wildcard rejected without opt-in", address: ":8080", wantErr: true},
		{name: "port wildcard accepted with plaintext opt-in", address: ":8080", plain: true},
		{name: "port wildcard accepted with tls", address: ":8443", tls: true},
		{name: "ipv4 wildcard rejected without opt-in", address: "0.0.0.0:8080", wantErr: true},
		{name: "ipv4 wildcard accepted with plaintext opt-in", address: "0.0.0.0:8080", plain: true},
		{name: "ipv4 wildcard accepted with tls", address: "0.0.0.0:8443", tls: true},
		{name: "ipv6 wildcard rejected without opt-in", address: "[::]:8080", wantErr: true},
		{name: "ipv6 wildcard accepted with plaintext opt-in", address: "[::]:8080", plain: true},
		{name: "ipv6 wildcard accepted with tls", address: "[::]:8443", tls: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tlsBlock string
			if tc.tls {
				tlsBlock = "  tls:\n    cert_file: /etc/mellomting/tls/cert.pem\n    key_file: /etc/mellomting/tls/key.pem\n"
			}
			var plainBlock string
			if tc.plain {
				plainBlock = "  allow_plaintext_non_loopback: true\n"
			}
			yaml := "version: 1\nserver:\n  listen:\n    network: tcp\n    address: \"" + tc.address + "\"\n" +
				tlsBlock + plainBlock +
				"servers:\n  qa:\n    url: http://127.0.0.1:8001\n" +
				"models:\n  m1:\n    upstream_model: M\n    servers: [qa]\n"
			_, err := Parse([]byte(yaml))
			if tc.wantErr && err == nil {
				t.Fatalf("Parse(%q) succeeded, want error", tc.address)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Parse(%q): %v", tc.address, err)
			}
		})
	}
}

// A trailing colon splits into a host and an empty port, so
// net.SplitHostPort alone accepts it. config check must reject what
// backend.parseBaseURL rejects, or an operator gets "valid" from the
// pre-flight and a refusal from serve.
func TestBaseURLEmptyPortRejected(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`
version: 1
` + minimalServer + `
servers:
  qa:
    url: "http://127.0.0.1:"
models:
  m1:
    upstream_model: M
    servers:
    - qa
`))
	if err == nil {
		t.Fatal("config check accepted a base_url with an empty port")
	}
	if !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("err = %v, want a base_url complaint", err)
	}
}

// A valid trusted_proxies list must survive validation and round-trip
// through show-effective, so an operator can confirm what the ingress
// will believe.
func TestTrustedProxiesAccepted(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: "0660"
  trusted_proxies:
    - unix
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Server.TrustedProxies) != 1 || cfg.Server.TrustedProxies[0] != TrustedProxyUnix {
		t.Fatalf("TrustedProxies = %v", cfg.Server.TrustedProxies)
	}

	tcp, err := Parse([]byte(`
version: 1
server:
  listen:
    network: tcp
    address: 127.0.0.1:8080
  trusted_proxies:
    - 127.0.0.1/32
    - 10.0.0.0/8
    - ::1/128
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    upstream_model: M
    servers:
    - qa
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(tcp.Server.TrustedProxies) != 3 {
		t.Fatalf("TrustedProxies = %v", tcp.Server.TrustedProxies)
	}
}
