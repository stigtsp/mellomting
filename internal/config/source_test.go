package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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

// TestSourceRelayUpstreamErrors pins the per-server relay flag through
// the whole path an operator uses: a server that says nothing relays,
// because a rejection the client cannot read is a rejection it cannot fix
// (PLAN §43); an explicit false turns it off, for a backend not trusted
// with its clients; and normalization carries the resolved value onto
// every backend generated for that server.
func TestSourceRelayUpstreamErrors(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(`
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  unset:
    url: http://127.0.0.1:8001
  on:
    url: http://127.0.0.1:8002
    relay_upstream_errors: true
  off:
    url: http://127.0.0.1:8003
    relay_upstream_errors: false
models:
  m1:
    servers: [unset, on, off]
  m2:
    servers: [off]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	for _, tc := range []struct {
		model, server string
		want          bool
	}{
		{"m1", "unset", true},
		{"m1", "on", true},
		{"m1", "off", false},
		{"m2", "off", false},
	} {
		b := cfg.Backends[defaultBackendName(tc.model, tc.server)]
		if b.RelayUpstreamErrors != tc.want {
			t.Fatalf("%s on %s: relay = %v, want %v", tc.model, tc.server, b.RelayUpstreamErrors, tc.want)
		}
	}
}

// TestSourceEffectiveKeepsRelayOff: with relay on by default, an explicit
// false is the one value show-effective must not drop. Omitted, it would
// parse back as the default and turn relay on for a backend the operator
// had turned it off for.
func TestSourceEffectiveKeepsRelayOff(t *testing.T) {
	t.Parallel()

	cfg, out, err := ParseEffective([]byte(serverLimitsYAML(`    relay_upstream_errors: false`)))
	if err != nil {
		t.Fatalf("ParseEffective: %v", err)
	}
	var doc struct {
		Servers map[string]map[string]any `yaml:"servers"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal effective: %v", err)
	}
	if got, ok := doc.Servers["tuned"]["relay_upstream_errors"]; !ok || got != false {
		t.Fatalf("effective servers.tuned.relay_upstream_errors = %v (present %v), want false\n%s", got, ok, out)
	}
	if got := doc.Servers["plain"]["relay_upstream_errors"]; got != true {
		t.Fatalf("effective servers.plain.relay_upstream_errors = %v, want the default true\n%s", got, out)
	}
	again, err := Parse(out)
	if err != nil {
		t.Fatalf("effective output does not parse: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(cfg, again) {
		t.Fatalf("effective output parses to a different config:\nfirst = %+v\nagain = %+v", cfg, again)
	}
}

// TestSourceBackendsRecordTheirServer: every backend generated from one
// server names it, so the daemon can give them one admission. A server
// is one inference engine however many public models it serves, and its
// capacity is the engine's, not each model's.
func TestSourceBackendsRecordTheirServer(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(sourceHappyYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, tc := range []struct{ model, server string }{
		{"qwen3.8-27b", "local-a"},
		{"qwen3.8-27b", "local-b"},
		{"qwen-coder", "local-b"},
	} {
		b := cfg.Backends[defaultBackendName(tc.model, tc.server)]
		if b.Server != tc.server {
			t.Fatalf("backend for %s on %s records server %q", tc.model, tc.server, b.Server)
		}
	}
}

// serverLimitsYAML is a two-server document whose first server sets
// its admission limits and whose second leaves them to the defaults.
func serverLimitsYAML(limits string) string {
	return `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  tuned:
    url: http://127.0.0.1:8001
` + limits + `
  plain:
    url: http://127.0.0.1:8002
models:
  m1:
    servers: [tuned, plain]
  m2:
    servers: [tuned]
`
}

// TestSourceServerAdmissionLimits: an operator sizes a server's
// admission to what the engine behind it runs at once, and every
// backend generated from that server carries the setting. A server that
// sets nothing keeps the defaults. queue_size: 0 is a real choice — no
// waiting, so a busy server is refused and routing can fall back at once
// — and must not be read as "unset".
func TestSourceServerAdmissionLimits(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(serverLimitsYAML(`    max_concurrency: 3
    queue_size: 0
    queue_timeout: 15s`)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, model := range []string{"m1", "m2"} {
		b := cfg.Backends[defaultBackendName(model, "tuned")]
		if b.MaxConcurrency != 3 || b.QueueSize != 0 || b.QueueTimeout.Duration() != 15*time.Second {
			t.Fatalf("%s on tuned: admission = %d/%d/%s, want 3/0/15s",
				model, b.MaxConcurrency, b.QueueSize, b.QueueTimeout.Duration())
		}
	}
	b := cfg.Backends[defaultBackendName("m1", "plain")]
	if b.MaxConcurrency != defaultMaxConcurrency || b.QueueSize != defaultBackendQueueSize ||
		b.QueueTimeout.Duration() != defaultQueueTimeout {
		t.Fatalf("plain server lost the defaults: %d/%d/%s",
			b.MaxConcurrency, b.QueueSize, b.QueueTimeout.Duration())
	}
}

// TestSourceServerAdmissionLimitsRejected: a limit outside its bound is
// refused and named by the server the operator wrote, never by a
// synthetic backend. An explicit zero cap or wait is refused rather
// than read as "use the default": the operator asked for something the
// daemon cannot do.
func TestSourceServerAdmissionLimitsRejected(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ limits, want string }{
		{"    max_concurrency: 0", "servers.tuned.max_concurrency"},
		{"    max_concurrency: -1", "servers.tuned.max_concurrency"},
		{fmt.Sprintf("    max_concurrency: %d", maxServerConcurrency+1), "servers.tuned.max_concurrency"},
		{"    queue_size: -1", "servers.tuned.queue_size"},
		{fmt.Sprintf("    queue_size: %d", maxServerQueueSize+1), "servers.tuned.queue_size"},
		{"    queue_timeout: 0s", "servers.tuned.queue_timeout"},
		{"    queue_timeout: -5s", "servers.tuned.queue_timeout"},
		{"    queue_timeout: 31m", "servers.tuned.queue_timeout"},
	} {
		_, err := Parse([]byte(serverLimitsYAML(tc.limits)))
		if err == nil {
			t.Fatalf("%q: accepted, want rejection", tc.limits)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%q: error %q does not name %s", tc.limits, err, tc.want)
		}
		if strings.Contains(err.Error(), "auto-") {
			t.Fatalf("%q: error names a synthetic backend: %v", tc.limits, err)
		}
	}
}

// decisionServerYAML is the minimal §D11 block: one decision server and
// one decision model, nothing else. Adding a decision server should
// need exactly this.
const decisionServerYAML = `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  clef:
    url: http://127.0.0.1:8011
models:
  clef-flash:
    type: decision
    servers: [clef]
`

// TestSourceDecisionServerDefaults pins §D11: the minimal decision block
// parses, and its server resolves the decision timeouts (30s header and
// request) with the ordinary admission defaults. The default queue
// wait, above a decision server's 30s request bound, is lowered to that
// bound rather than refused.
func TestSourceDecisionServerDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(decisionServerYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b := cfg.Backends[defaultBackendName("clef-flash", "clef")]
	if b.HeaderTimeout.Duration() != decisionHeaderTimeout {
		t.Fatalf("header timeout = %s, want %s", b.HeaderTimeout.Duration(), decisionHeaderTimeout)
	}
	if b.RequestTimeout.Duration() != decisionRequestTimeout {
		t.Fatalf("request timeout = %s, want %s", b.RequestTimeout.Duration(), decisionRequestTimeout)
	}
	if b.MaxConcurrency != defaultMaxConcurrency || b.QueueSize != defaultBackendQueueSize {
		t.Fatalf("admission = %d/%d, want the ordinary defaults %d/%d",
			b.MaxConcurrency, b.QueueSize, defaultMaxConcurrency, defaultBackendQueueSize)
	}
	if b.QueueTimeout.Duration() != decisionRequestTimeout {
		t.Fatalf("queue wait = %s, want it lowered to the 30s request bound", b.QueueTimeout.Duration())
	}
	// upstream_model defaults to the public name (§D11), which also
	// keeps the one relayed message that names the upstream model
	// harmless (§D6).
	if b.UpstreamModel != "clef-flash" {
		t.Fatalf("upstream = %q, want the public name default", b.UpstreamModel)
	}
	if m := cfg.Models["clef-flash"]; m.Type != "decision" {
		t.Fatalf("model type = %q, want decision", m.Type)
	}
}

// TestSourceDecisionServerSkipsMetrics: a decision server publishes
// neither /metrics nor /api/ps, so top does not read a decision-only
// server unless the operator writes metrics: true. A server shared with
// a generation model is read as before.
func TestSourceDecisionServerSkipsMetrics(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(decisionServerYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.Backends[defaultBackendName("clef-flash", "clef")].NoMetrics {
		t.Fatal("decision-only server is polled for metrics by default")
	}
	on := strings.Replace(decisionServerYAML, "url: http://127.0.0.1:8011", "url: http://127.0.0.1:8011\n    metrics: true", 1)
	cfg, err = Parse([]byte(on))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Backends[defaultBackendName("clef-flash", "clef")].NoMetrics {
		t.Fatal("metrics: true ignored on a decision server")
	}
	mixed := decisionServerYAML + "  qwen:\n    servers: [clef]\n"
	cfg, err = Parse([]byte(mixed))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, model := range []string{"clef-flash", "qwen"} {
		if cfg.Backends[defaultBackendName(model, "clef")].NoMetrics {
			t.Fatalf("%s: shared server not polled for metrics", model)
		}
	}
}

// TestSourceMixedServerKeepsGenerationTimeouts pins §D11: a server
// shared by a decision and a generation model keeps the generation
// timeouts. The smaller bounds would cut a generation call off, while
// the larger ones hurt a decision call only by waiting longer.
func TestSourceMixedServerKeepsGenerationTimeouts(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(`
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  shared:
    url: http://127.0.0.1:8011
models:
  clef-flash:
    type: decision
    servers: [shared]
  qwen:
    servers: [shared]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, model := range []string{"clef-flash", "qwen"} {
		b := cfg.Backends[defaultBackendName(model, "shared")]
		if b.HeaderTimeout.Duration() != defaultHeaderTimeout || b.RequestTimeout.Duration() != defaultRequestTimeout {
			t.Fatalf("%s: timeouts = %s/%s, want the generation defaults %s/%s",
				model, b.HeaderTimeout.Duration(), b.RequestTimeout.Duration(), defaultHeaderTimeout, defaultRequestTimeout)
		}
		if b.QueueTimeout.Duration() != defaultQueueTimeout {
			t.Fatalf("%s: queue wait = %s, want the default %s", model, b.QueueTimeout.Duration(), defaultQueueTimeout)
		}
	}
}

// TestSourceQueueTimeoutBoundIsTheServersOwn pins §D11: queue_timeout
// is checked against the server's own request bound, not the
// generation constant. On a decision server the bound is 30s, so an
// explicit wait above it is refused, naming the server; at the bound it
// is kept; below it, kept. On a generation server the bound is the
// generation request timeout.
func TestSourceQueueTimeoutBoundIsTheServersOwn(t *testing.T) {
	t.Parallel()

	decision := func(limits string) string {
		return `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
servers:
  clef:
    url: http://127.0.0.1:8011
` + limits + `
models:
  clef-flash:
    type: decision
    servers: [clef]
`
	}

	// Above the decision bound: refused, naming the server.
	if _, err := Parse([]byte(decision("    queue_timeout: 31s"))); err == nil ||
		!strings.Contains(err.Error(), "servers.clef.queue_timeout") {
		t.Fatalf("31s on a decision server: err = %v, want a servers.clef.queue_timeout rejection", err)
	}
	// At the bound: kept.
	cfg, err := Parse([]byte(decision("    queue_timeout: 30s")))
	if err != nil {
		t.Fatalf("30s on a decision server: %v", err)
	}
	if got := cfg.Backends[defaultBackendName("clef-flash", "clef")].QueueTimeout.Duration(); got != 30*time.Second {
		t.Fatalf("queue wait = %s, want 30s", got)
	}
	// A generation server's bound is its request timeout: 31m is
	// refused there too, and the default wait is untouched.
	if _, err := Parse([]byte(serverLimitsYAML("    queue_timeout: 31m"))); err == nil ||
		!strings.Contains(err.Error(), "servers.tuned.queue_timeout") {
		t.Fatalf("31m on a generation server: err = %v, want a servers.tuned.queue_timeout rejection", err)
	}
}

// TestSourceEffectiveDecisionRoundTrip pins §D11: show-effective gains
// no field for the resolved timeouts (they have no configuration key),
// and its output for the minimal decision block parses back to the same
// Config, with the model keeping type: decision.
func TestSourceEffectiveDecisionRoundTrip(t *testing.T) {
	t.Parallel()

	cfg, out, err := ParseEffective([]byte(decisionServerYAML))
	if err != nil {
		t.Fatalf("ParseEffective: %v", err)
	}
	var doc struct {
		Models map[string]map[string]any `yaml:"models"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal effective: %v", err)
	}
	if got := doc.Models["clef-flash"]["type"]; got != "decision" {
		t.Fatalf("effective models.clef-flash.type = %v, want decision\n%s", got, out)
	}
	again, err := Parse(out)
	if err != nil {
		t.Fatalf("effective output does not parse: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(cfg, again) {
		t.Fatalf("effective output parses to a different config:\nfirst = %+v\nagain = %+v", cfg, again)
	}
}

// TestSourceEffectiveShowsServerLimits: show-effective is where an
// operator confirms what a server is admitted under, so it prints the
// limits each server actually runs with, defaults included, and its
// output is itself a configuration that parses to the same Config.
func TestSourceEffectiveShowsServerLimits(t *testing.T) {
	t.Parallel()

	cfg, out, err := ParseEffective([]byte(serverLimitsYAML(`    max_concurrency: 3
    queue_size: 0`)))
	if err != nil {
		t.Fatalf("ParseEffective: %v", err)
	}
	var doc struct {
		Servers map[string]map[string]any `yaml:"servers"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal effective: %v", err)
	}
	for server, want := range map[string]map[string]any{
		"tuned": {"max_concurrency": 3, "queue_size": 0, "queue_timeout": defaultQueueTimeout.String()},
		"plain": {"max_concurrency": defaultMaxConcurrency, "queue_size": defaultBackendQueueSize, "queue_timeout": defaultQueueTimeout.String()},
	} {
		for key, v := range want {
			if got, ok := doc.Servers[server][key]; !ok || got != v {
				t.Fatalf("effective servers.%s.%s = %v (present %v), want %v\n%s", server, key, got, ok, v, out)
			}
		}
	}
	again, err := Parse(out)
	if err != nil {
		t.Fatalf("effective output does not parse: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(cfg, again) {
		t.Fatalf("effective output parses to a different config:\nfirst = %+v\nagain = %+v", cfg, again)
	}
}

// TestSourceServerRequestTimeout pins SYSTEMONE_PLAN §D14: a decision
// server whose calls take tens of seconds (a 30K-token state takes ~22s
// on Karpo on an A100, and most servers run one request at a time, so a
// call also waits behind the ones ahead of it on the server) needs a
// longer bound than the 30s default. request_timeout sets the server's
// request bound; the header bound follows it on a decision server and
// never exceeds it on any server; the queue wait is checked against it.
func TestSourceServerRequestTimeout(t *testing.T) {
	t.Parallel()

	with := func(limits string) string {
		return strings.Replace(decisionServerYAML, "url: http://127.0.0.1:8011", "url: http://127.0.0.1:8011\n"+limits, 1)
	}
	cfg, err := Parse([]byte(with("    request_timeout: 10m\n    queue_timeout: 5m")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b := cfg.Backends[defaultBackendName("clef-flash", "clef")]
	if b.RequestTimeout.Duration() != 10*time.Minute || b.HeaderTimeout.Duration() != 10*time.Minute {
		t.Fatalf("timeouts = %s/%s, want 10m/10m", b.HeaderTimeout.Duration(), b.RequestTimeout.Duration())
	}
	if b.QueueTimeout.Duration() != 5*time.Minute {
		t.Fatalf("queue wait = %s, want 5m", b.QueueTimeout.Duration())
	}

	// Without queue_timeout the default 120s wait fits under 10m.
	cfg, err = Parse([]byte(with("    request_timeout: 10m")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Backends[defaultBackendName("clef-flash", "clef")].QueueTimeout.Duration(); got != defaultQueueTimeout {
		t.Fatalf("queue wait = %s, want the default %s", got, defaultQueueTimeout)
	}

	// A generation server: the header bound never exceeds the request
	// bound, and a longer request bound leaves the header default.
	gen, err := Parse([]byte(serverLimitsYAML("    request_timeout: 60s")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, model := range []string{"m1", "m2"} {
		gb := gen.Backends[defaultBackendName(model, "tuned")]
		if gb.RequestTimeout.Duration() != time.Minute || gb.HeaderTimeout.Duration() != time.Minute {
			t.Fatalf("generation timeouts = %s/%s, want 60s/60s", gb.HeaderTimeout.Duration(), gb.RequestTimeout.Duration())
		}
		if gb.QueueTimeout.Duration() != time.Minute {
			t.Fatalf("generation queue wait = %s, want it lowered to 60s", gb.QueueTimeout.Duration())
		}
	}

	for limits, want := range map[string]string{
		"    request_timeout: 0s":                          "servers.clef.request_timeout",
		"    request_timeout: 31m":                         "servers.clef.request_timeout",
		"    request_timeout: 60s\n    queue_timeout: 61s": "servers.clef.queue_timeout",
	} {
		if _, err := Parse([]byte(with(limits))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want one naming %s", limits, err, want)
		}
	}

	// show-effective prints what the operator wrote, and parses back.
	cfg, out, err := ParseEffective([]byte(with("    request_timeout: 10m")))
	if err != nil {
		t.Fatalf("ParseEffective: %v", err)
	}
	if !strings.Contains(string(out), "request_timeout: 10m0s") {
		t.Fatalf("effective output lacks the request timeout:\n%s", out)
	}
	again, err := Parse(out)
	if err != nil || !reflect.DeepEqual(cfg, again) {
		t.Fatalf("effective output does not parse back to the same config: %v\n%s", err, out)
	}
}
