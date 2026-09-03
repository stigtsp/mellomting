package config

import (
	"reflect"
	"strings"
	"testing"
)

const sourceHappyYAML = `
version: 1

server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock

servers:
  local-a:
    url: http://127.0.0.1:8001
  local-b:
    url: http://127.0.0.1:8010
    api_key_file: /etc/mellomting/keys/local-b

models:
  qwen3.8-27b:
    servers: [local-a, local-b]
  qwen-coder:
    servers: [local-b]
`

// TestSourceSchemaHappyPath pins D11: the server-oriented source form
// parses and normalizes into the runtime Config with synthetic backends.
func TestSourceSchemaHappyPath(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(sourceHappyYAML))
	if err != nil {
		t.Fatalf("Parse source form: %v", err)
	}
	if len(cfg.Backends) != 3 {
		t.Fatalf("parsed %d backends, want 3 (one per model/server pair)", len(cfg.Backends))
	}
	if len(cfg.Models) != 2 {
		t.Fatalf("parsed %d models, want 2", len(cfg.Models))
	}
	// qwen3.8-27b has two replicas in YAML order, weight 1.
	m := cfg.Models["qwen3.8-27b"]
	if len(m.Backends) != 2 {
		t.Fatalf("model has %d backend refs, want 2", len(m.Backends))
	}
	for i, ref := range m.Backends {
		if ref.Weight != 1 {
			t.Fatalf("ref %d weight = %d, want 1", i, ref.Weight)
		}
		b, ok := cfg.Backends[ref.Name]
		if !ok {
			t.Fatalf("ref %d names missing backend %q", i, ref.Name)
		}
		if b.UpstreamModel != "qwen3.8-27b" {
			t.Fatalf("upstream = %q, want public name default", b.UpstreamModel)
		}
	}
	if cfg.Backends[m.Backends[0].Name].BaseURL != "http://127.0.0.1:8001" {
		t.Fatalf("first replica URL = %q", cfg.Backends[m.Backends[0].Name].BaseURL)
	}
	// The api_key_file travels to the generated backend.
	second := m.Backends[1].Name
	if got := cfg.Backends[second].APIKeyFile; got != "/etc/mellomting/keys/local-b" {
		t.Fatalf("api_key_file = %q, want the operator path", got)
	}
	// Shared server local-b yields distinct backends per model.
	for name, b := range cfg.Backends {
		if strings.HasPrefix(name, "auto-local-b-") && b.BaseURL != "http://127.0.0.1:8010" {
			t.Fatalf("backend %s URL = %q", name, b.BaseURL)
		}
	}
	if len(cfg.Qualifiers) != 0 {
		t.Fatalf("qualifiers = %d, want 0", len(cfg.Qualifiers))
	}
}

// TestSourceRejectsLegacyAndShelvedForms pins D11: the development-era
// backends: form and the shelved qualifiers: form are rejected as unknown
// fields.
func TestSourceRejectsLegacyAndShelvedForms(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"backends form": `
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
		"qualifiers form": sourceHappyYAML + `
qualifiers:
  safety-audit:
    backend: local-a
`,
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(yaml))
			if err == nil {
				t.Fatalf("Parse accepted %s", name)
			}
			if !strings.Contains(err.Error(), "field backends not found") &&
				!strings.Contains(err.Error(), "field qualifiers not found") {
				t.Fatalf("Parse error = %v, want unknown-field rejection", err)
			}
		})
	}
}

// TestSourceRejectsUnknownFields pins D11: unknown fields at every level
// of the source schema are rejected by strict decoding.
func TestSourceRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	base := `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  local-a:
    url: http://127.0.0.1:8001
    `
	cases := []struct {
		name string
		yaml string
	}{
		{"top-level unknown field", base + `
bogus: 1
models:
  m1:
    servers: [local-a]
`},
		{"server unknown field", base + `
    extra: true
models:
  m1:
    servers: [local-a]
`},
		{"model unknown field", base + `
models:
  m1:
    servers: [local-a]
    qualifier: x
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("Parse accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), "not found in type") {
				t.Fatalf("Parse error = %v, want unknown-field rejection", err)
			}
		})
	}
}

// TestSourceMissingAndDuplicateServerRefs pins D12 step 1: missing and
// duplicate server references fail closed.
func TestSourceMissingAndDuplicateServerRefs(t *testing.T) {
	t.Parallel()

	missing := `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  local-a:
    url: http://127.0.0.1:8001
models:
  m1:
    servers: [ghost]
`
	if _, err := Parse([]byte(missing)); err == nil || !strings.Contains(err.Error(), `unknown server "ghost"`) {
		t.Fatalf("missing server ref: err = %v, want unknown server", err)
	}

	dup := `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  local-a:
    url: http://127.0.0.1:8001
models:
  m1:
    servers: [local-a, local-a]
`
	if _, err := Parse([]byte(dup)); err == nil || !strings.Contains(err.Error(), `duplicate server "local-a"`) {
		t.Fatalf("duplicate server ref: err = %v, want duplicate server", err)
	}

	// An empty server list is also rejected.
	empty := `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  local-a:
    url: http://127.0.0.1:8001
models:
  m1:
    servers: []
`
	if _, err := Parse([]byte(empty)); err == nil || !strings.Contains(err.Error(), "at least one server is required") {
		t.Fatalf("empty server list: err = %v, want at-least-one error", err)
	}

	// A server no model references is rejected fail-closed.
	unused := `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  local-a:
    url: http://127.0.0.1:8001
  local-b:
    url: http://127.0.0.1:8010
models:
  m1:
    servers: [local-a]
`
	if _, err := Parse([]byte(unused)); err == nil || !strings.Contains(err.Error(), "local-b") {
		t.Fatalf("unused server: err = %v, want not-referenced error", err)
	}
}

// TestSourceDefaultTypeAndStrategy pins D12: empty type defaults to
// generation; empty strategy defaults to single for one server and
// least-inflight for several.
func TestSourceDefaultTypeAndStrategy(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(sourceHappyYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Models["qwen3.8-27b"]; got.Type != "generation" || got.Strategy != "least-inflight" {
		t.Fatalf("two-server model defaults = %q/%q, want generation/least-inflight", got.Type, got.Strategy)
	}
	if got := cfg.Models["qwen-coder"]; got.Type != "generation" || got.Strategy != "single" {
		t.Fatalf("one-server model defaults = %q/%q, want generation/single", got.Type, got.Strategy)
	}
}

// TestSourceUpstreamAlias pins D12: upstream_model aliases the internal
// backend upstream while the map key stays the public model name.
func TestSourceUpstreamAlias(t *testing.T) {
	t.Parallel()

	yaml := `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  local-a:
    url: http://127.0.0.1:8001
models:
  display-name:
    servers: [local-a]
    upstream_model: ORIGINAL_ID
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := cfg.Models["display-name"]; !ok {
		t.Fatalf("public model key = %v, want display-name", cfg.Models)
	}
	for _, b := range cfg.Backends {
		if b.UpstreamModel != "ORIGINAL_ID" {
			t.Fatalf("upstream = %q, want ORIGINAL_ID", b.UpstreamModel)
		}
	}
}

// TestSourceDeterministicBackendNames pins D12 step 3: the generated
// backend names are stable sha256-derived constants, independent of map
// iteration order.
func TestSourceDeterministicBackendNames(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(sourceHappyYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string]struct{}{
		defaultBackendName("qwen3.8-27b", "local-a"): {},
		defaultBackendName("qwen3.8-27b", "local-b"): {},
		defaultBackendName("qwen-coder", "local-b"):  {},
	}
	if len(cfg.Backends) != len(want) {
		t.Fatalf("backends = %v, want exactly %v", cfg.Backends, want)
	}
	for name := range cfg.Backends {
		if _, ok := want[name]; !ok {
			t.Fatalf("unexpected backend name %q (want deterministic %v)", name, want)
		}
		delete(want, name)
	}
}

// TestSourceGeneratedNameCollision pins D12 step 4: the (practically
// impossible) generated-name collision path is rejected, unit tested by
// injecting a colliding backendNameFor implementation.
func TestSourceGeneratedNameCollision(t *testing.T) {
	t.Parallel()

	doc := sourceDoc{
		Version: 1,
		Servers: map[string]sourceServer{
			"a": {URL: "http://127.0.0.1:8001"},
			"b": {URL: "http://127.0.0.1:8002"},
		},
		Models: map[string]sourceModel{
			"m1": {Servers: []string{"a", "b"}},
		},
	}
	_, err := normalizeSourceWith(doc, func(string, string) string { return "auto-fixed-collision" })
	if err == nil || !strings.Contains(err.Error(), "generated backend name collision") {
		t.Fatalf("collision: err = %v, want generated-name collision error", err)
	}
}

// TestSourceDefaultedRoundTrip pins D11: a source document with defaults
// omitted normalizes to the same Config as the same document with every
// default written explicitly. This is the property that makes the
// fully-defaulted effective source output re-parseable (B2).
func TestSourceDefaultedRoundTrip(t *testing.T) {
	t.Parallel()

	minimal := `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  a:
    url: http://127.0.0.1:8001
  b:
    url: http://127.0.0.1:8002
models:
  m1:
    servers: [a, b]
  m2:
    servers: [a]
`
	explicit := `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  a:
    url: http://127.0.0.1:8001
  b:
    url: http://127.0.0.1:8002
models:
  m1:
    servers: [a, b]
    type: generation
    strategy: least-inflight
    upstream_model: m1
  m2:
    servers: [a]
    type: generation
    strategy: single
    upstream_model: m2
`
	min, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse minimal: %v", err)
	}
	expl, err := Parse([]byte(explicit))
	if err != nil {
		t.Fatalf("Parse explicit: %v", err)
	}
	if !reflect.DeepEqual(min, expl) {
		t.Fatalf("defaulted configs differ:\nminimal = %+v\nexplicit = %+v", min, expl)
	}
}
