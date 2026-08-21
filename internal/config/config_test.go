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
    mode: "0660"

backends:
  qwen-a:
    base_url: http://127.0.0.1:8001
    upstream_model: Qwen/Qwen3-Coder-Next

models:
  qwen-coder:
    backends:
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
	if len(cfg.Backends) != 3 || len(cfg.Models) != 1 || len(cfg.Qualifiers) != 1 {
		t.Fatalf("parsed %d backends, %d models, %d qualifiers; want 3/1/1",
			len(cfg.Backends), len(cfg.Models), len(cfg.Qualifiers))
	}
	if cfg.Server.MaxInflightRequests != 32 {
		t.Fatalf("explicit limit lost: max_inflight = %d, want 32", cfg.Server.MaxInflightRequests)
	}
	q := cfg.Qualifiers["safety-audit"]
	if q.FailurePolicy != "allow" || q.Input.Mode != "audit" || q.Output.Mode != "disabled" {
		t.Fatalf("qualifier parsed wrong: %+v", q)
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
		cfg.Security.Landlock.Mode != "required" || cfg.Security.Landlock.MinimumABI != 6 {
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

	b := cfg.Backends["qwen-a"]
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
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
    mode: "0660"
  bogus: true
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
    mode: "0660"
  read_header_timeout: 5
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
`,
			wantErr: "malformed duration",
		},
		{
			name: "duplicate backend reference",
			yaml: `
version: 1
` + minimalServer + `
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends:
      - qa
      - qa
`,
			wantErr: "duplicate backend",
		},
		{
			name: "unknown backend reference",
			yaml: `
version: 1
` + minimalServer + `
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends:
      - missing
`,
			wantErr: "unknown backend",
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
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends:
      - qa
`,
			wantErr: "not a valid CIDR",
		},
		{
			name: "unknown qualifier reference",
			yaml: `
version: 1
` + minimalServer + `
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
    qualifier: ghost
`,
			wantErr: "unknown qualifier",
		},
		{
			name: "strategy single with two backends",
			yaml: `
version: 1
` + minimalServer + `
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
  qb:
    base_url: http://127.0.0.1:8002
    upstream_model: M
models:
  m1:
    strategy: single
    backends: [qa, qb]
`,
			wantErr: "exactly one backend",
		},
		{
			name: "model without backends",
			yaml: `
version: 1
` + minimalServer + `
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1: {}
`,
			wantErr: "at least one backend reference",
		},
		{
			name: "non-loopback backend in loopback-only mode",
			yaml: `
version: 1
` + minimalServer + `
backends:
  qa:
    base_url: http://vllm.internal:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
`,
			wantErr: "not a loopback IP literal",
		},
		{
			name: "backend url query rejected",
			yaml: `
version: 1
` + minimalServer + `
backends:
  qa:
    base_url: http://127.0.0.1:8001/?x=1
    upstream_model: M
models:
  m1:
    backends: [qa]
`,
			wantErr: "query strings are not supported",
		},
		{
			name: "backend url fragment rejected",
			yaml: `
version: 1
` + minimalServer + `
backends:
  qa:
    base_url: http://127.0.0.1:8001#frag
    upstream_model: M
models:
  m1:
    backends: [qa]
`,
			wantErr: "fragments are not supported",
		},
		{
			name: "backend url userinfo rejected",
			yaml: `
version: 1
` + minimalServer + `
backends:
  qa:
    base_url: http://user@127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
`,
			wantErr: "userinfo",
		},
		{
			name: "backend url path rejected",
			yaml: `
version: 1
` + minimalServer + `
backends:
  qa:
    base_url: http://127.0.0.1:8001/v1
    upstream_model: M
models:
  m1:
    backends: [qa]
`,
			wantErr: "ambiguous",
		},
		{
			name: "backend url scheme rejected",
			yaml: `
version: 1
` + minimalServer + `
backends:
  qa:
    base_url: ftp://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
`,
			wantErr: "unsupported scheme",
		},
		{
			name: "missing upstream model",
			yaml: `
version: 1
` + minimalServer + `
backends:
  qa:
    base_url: http://127.0.0.1:8001
models:
  m1:
    backends: [qa]
`,
			wantErr: "upstream_model: required",
		},
		{
			name: "tcp listener without port",
			yaml: `
version: 1
server:
  listen:
    network: tcp
    address: 127.0.0.1
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
    mode: files
    cert_file: /etc/mellomting/tls/cert.pem
    key_file: /etc/mellomting/tls/key.pem
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
    mode: "0660"
  tls:
    mode: files
    cert_file: /etc/mellomting/tls/cert.pem
    key_file: /etc/mellomting/tls/key.pem
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
    mode: "0660"
  max_body_bytes: -1
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
backends:
  qa:
    base_url: http://10.1.0.4:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
    cidrs: [10.1.0.0/24]
backends:
  qa:
    base_url: http://10.1.0.4:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
    cidrs: [10.1.0.0/24]
backends:
  qa:
    base_url: http://10.9.9.9:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
`,
			wantErr: "not within",
		},
		{
			name: "qualifier without failure policy",
			yaml: `
version: 1
` + minimalServer + `
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
  guard:
    base_url: http://127.0.0.1:8100
    upstream_model: G
qualifiers:
  s:
    backend: guard
    model: G
    input:
      mode: audit
    output:
      mode: disabled
models:
  m1:
    backends: [qa]
`,
			wantErr: "failure_policy: required",
		},
		{
			name: "remote qualifier without opt-in",
			yaml: `
version: 1
` + minimalServer + `
security:
  backend_network:
    mode: any
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
  guard:
    base_url: http://10.1.1.1:8100
    upstream_model: G
qualifiers:
  s:
    backend: guard
    model: G
    failure_policy: allow
    input:
      mode: audit
    output:
      mode: disabled
models:
  m1:
    backends: [qa]
`,
			wantErr: "allow_remote_content",
		},
		{
			name: "accounting disabled but fields set",
			yaml: `
version: 1
` + minimalServer + `
accounting:
  enabled: false
  path: /var/log/mellomting/usage.jsonl
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
`,
			wantErr: "overflow",
		},
		{
			name: "accounting negative reservation",
			yaml: `
version: 1
` + minimalServer + `
accounting:
  enabled: true
  unknown_usage_reservation: -5
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
`,
			wantErr: "unknown_usage_reservation",
		},
		{
			name: "accounting reservation set while disabled",
			yaml: `
version: 1
` + minimalServer + `
accounting:
  enabled: false
  unknown_usage_reservation: 50
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
`,
			wantErr: "must be empty when enabled is false",
		},
		{
			name: "bad landlock mode",
			yaml: `
version: 1
` + minimalServer + `
security:
  landlock:
    mode: off
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
`,
			wantErr: "minimum_abi",
		},
		{
			name: "no backends",
			yaml: `
version: 1
` + minimalServer + `
models:
  m1:
    backends: [qa]
`,
			wantErr: "at least one backend is required",
		},
		{
			name: "no models",
			yaml: `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: "0660"
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
`,
			wantErr: "at least one public model is required",
		},
	}

	for _, tc := range cases {
		tc := tc
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
		cfg := fmt.Sprintf("version: 1\n%s\nbackends:\n  qa:\n    base_url: %q\n    upstream_model: M\nmodels:\n  m1:\n    backends: [qa]\n", minimalServer, base)
		if _, err := Parse([]byte(cfg)); err == nil {
			t.Fatalf("Parse succeeded for %q; want a validation error", base)
		} else if strings.Contains(err.Error(), secret) {
			t.Fatalf("validation error leaks password for %q: %s", base, err)
		}
	}
}

// TestRedactURL pins the redaction helper's edge cases (T-M13).
func TestRedactURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://admin:pass@host\x7f", "http://<redacted>@host\x7f"},
		{"http://user:pass@127.0.0.1:8001", "http://<redacted>@127.0.0.1:8001"},
		{"http://127.0.0.1:8001", "http://127.0.0.1:8001"},
		{"http://127.0.0.1:8001/v1", "http://127.0.0.1:8001/v1"},
		{"no-scheme-value", "no-scheme-value"},
	}
	for _, tc := range cases {
		if got := redactURL(tc.in); got != tc.want {
			t.Fatalf("redactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

const restOfConfig = `
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: "0660"
backends:
  qa:
    base_url: http://127.0.0.1:8001
    upstream_model: M
models:
  m1:
    backends: [qa]
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
