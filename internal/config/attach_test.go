package config

import (
	"fmt"
	"strings"
	"testing"
)

const attachOnly = `version: 1
attach:
  edge: https://edge.example.org
  key_file: /etc/mellomting/agent.key
  servers: ["http://127.0.0.1:8000"]
`

// A daemon that only attaches needs no listener, keys, servers or models.
func TestAttachOnlyConfig(t *testing.T) {
	cfg, err := Parse([]byte(attachOnly))
	if err != nil {
		t.Fatalf("attach-only configuration must be valid: %v", err)
	}
	if cfg.ServesClients() {
		t.Fatal("an attach-only daemon must not serve clients")
	}
	if cfg.Attach == nil || cfg.Attach.Servers[0] != "http://127.0.0.1:8000" {
		t.Fatalf("attach not carried: %+v", cfg.Attach)
	}
	_, eff, err := ParseEffective([]byte(attachOnly))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(eff); err != nil {
		t.Fatalf("show-effective of an attach-only configuration must parse: %v\n%s", err, eff)
	}
}

// A daemon that attaches does nothing else: every section it would
// ignore is refused rather than accepted and dropped.
func TestAttachExcludesClientSections(t *testing.T) {
	for _, extra := range []string{
		"server:\n  listen: {network: tcp, address: \"127.0.0.1:8080\"}\n",
		"server:\n  admin_socket: /run/m.sock\n",
		"auth:\n  users_file: /etc/mellomting/users.yaml\n",
		"agents:\n  enabled: true\n",
		"accounting:\n  enabled: true\n",
		"limits:\n  global_burst: 5\n",
		"retry:\n  max_attempts: 2\n",
		"security:\n  backend_network: {mode: any}\n",
		"servers:\n  a: {url: \"http://127.0.0.1:8000\"}\nmodels:\n  m: {servers: [a]}\n",
	} {
		_, err := Parse([]byte(attachOnly + extra))
		if err == nil || !strings.Contains(err.Error(), "does not apply with attach") {
			t.Errorf("%q: got %v, want it refused", extra, err)
		}
	}
	for _, extra := range []string{
		"logging:\n  level: debug\n",
		"shutdown:\n  grace_period: 2m\n",
		"security:\n  landlock: {mode: required}\n",
	} {
		if _, err := Parse([]byte(attachOnly + extra)); err != nil {
			t.Errorf("%q applies to an attaching daemon: %v", extra, err)
		}
	}
}

func TestAttachValidation(t *testing.T) {
	for doc, want := range map[string]string{
		"version: 1\nattach:\n  edge: http://localhost:8080\n  key_file: /k\n  servers: [\"http://127.0.0.1:8000\"]\n":                    "must be https",
		"version: 1\nattach:\n  key_file: /k\n  servers: [\"http://127.0.0.1:8000\"]\n":                                                   "attach.edge: required",
		"version: 1\nattach:\n  edge: http://edge.example.org\n  key_file: /k\n  servers: [\"http://127.0.0.1:8000\"]\n":                  "must be https",
		"version: 1\nattach:\n  edge: https://u:p@e.example.org\n  key_file: /k\n  servers: [\"http://127.0.0.1:8000\"]\n":                "credentials",
		"version: 1\nattach:\n  edge: https://e.example.org\n  servers: [\"http://127.0.0.1:8000\"]\n":                                    "attach.key_file: required",
		"version: 1\nattach:\n  edge: https://e.example.org\n  key_file: k\n  servers: [\"http://127.0.0.1:8000\"]\n":                     "attach.key_file: must be an absolute path",
		"version: 1\nattach:\n  edge: https://e.example.org\n  key_file: /k\n  ca_file: ca.pem\n  servers: [\"http://127.0.0.1:8000\"]\n": "attach.ca_file",
		"version: 1\nattach:\n  edge: https://e.example.org\n  key_file: /k\n":                                                            "at least one server",
		"version: 1\nattach:\n  edge: https://e.example.org\n  key_file: /k\n  servers: [\"http://127.0.0.1:8000/v1\"]\n":                 "path",
		"version: 1\nattach:\n  edge: https://e.example.org\n  key_file: /k\n  servers: [\"http://127.0.0.1:\"]\n":                        "malformed",
		"version: 1\nattach:\n  edge: https://e.example.org\n  key_file: /k\n  servers: [\"http://a:1\", \"http://a:1\"]\n":               "twice",
	} {
		_, err := Parse([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want error containing %q", doc, err, want)
		}
	}
	// A plain http edge at a loopback address, or with insecure_edge, is
	// allowed; "localhost" is a name, and a name can resolve anywhere.
	for _, edge := range []string{"http://127.0.0.1:8080", "http://[::1]:8080", "http://edge.example.org\n  insecure_edge: true"} {
		doc := "version: 1\nattach:\n  edge: " + edge + "\n  key_file: /k\n  servers: [\"http://127.0.0.1:8000\"]\n"
		if _, err := Parse([]byte(doc)); err != nil {
			t.Errorf("edge %q: %v", edge, err)
		}
	}
}

// A configuration written before attach existed parses as before and
// serves clients; nothing it names is refused or reinterpreted.
func TestPreAttachConfigUnchanged(t *testing.T) {
	const existing = `auth:
    pepper_file: /etc/mellomting/auth.pepper
    users_file: /etc/mellomting/users.yaml
models:
    model-a:
        servers: [gpu1]
        type: generation
    model-b:
        servers: [remote]
        type: generation
security:
    backend_network: {mode: any}
    landlock: {minimum_abi: 6, mode: best-effort}
server:
    listen: {address: "127.0.0.1:8080", network: tcp}
    trusted_proxies: [127.0.0.1/32]
    admin_socket: /run/mellomting/admin.sock
servers:
    gpu1: {url: "http://192.0.2.10:8001"}
    remote: {url: "https://gpu.example.org:443", api_key_file: /etc/mellomting/remote.key}
version: 1
`
	cfg, eff, err := ParseEffective([]byte(existing))
	if err != nil {
		t.Fatalf("existing configuration refused: %v", err)
	}
	if !cfg.ServesClients() || cfg.Attach != nil || len(cfg.Models) != 2 || cfg.Server.AdminSocket == "" {
		t.Fatalf("existing configuration reinterpreted: %+v", cfg)
	}
	if strings.Contains(string(eff), "attach") {
		t.Fatalf("show-effective grew an attach section:\n%s", eff)
	}
	if _, err := Parse(eff); err != nil {
		t.Fatalf("show-effective does not parse: %v\n%s", err, eff)
	}
}

func TestServerMetricsSwitch(t *testing.T) {
	base := `version: 1
auth: {pepper_file: /etc/m/p, users_file: /etc/m/u.yaml}
server: {listen: {address: "127.0.0.1:8080", network: tcp}}
models: {m1: {servers: [s1]}}
servers:
  s1: {url: "http://127.0.0.1:8000"%s}
`
	cfg, eff, err := ParseEffective([]byte(fmt.Sprintf(base, "")))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range cfg.Backends {
		if b.NoMetrics {
			t.Fatal("metrics off by default")
		}
	}
	if strings.Contains(string(eff), "metrics") {
		t.Fatalf("show-effective names the default:\n%s", eff)
	}
	cfg, eff, err = ParseEffective([]byte(fmt.Sprintf(base, ", metrics: false")))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range cfg.Backends {
		if !b.NoMetrics {
			t.Fatal("metrics: false ignored")
		}
	}
	if !strings.Contains(string(eff), "metrics: false") {
		t.Fatalf("show-effective dropped metrics: false:\n%s", eff)
	}
	if _, err := Parse(eff); err != nil {
		t.Fatalf("show-effective does not parse: %v", err)
	}
}

func TestAttachMetricsKey(t *testing.T) {
	base := "version: 1\nattach:\n  edge: https://edge.example.org\n  key_file: /etc/m/agent.key\n  servers: [\"http://127.0.0.1:8000\"]\n%s"
	cfg, err := Parse([]byte(fmt.Sprintf(base, "")))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Attach.MetricsEnabled() {
		t.Fatal("metrics off by default")
	}
	cfg, err = Parse([]byte(fmt.Sprintf(base, "  metrics: false\n")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Attach.MetricsEnabled() {
		t.Fatal("metrics: false ignored")
	}
}

const attachDecision = `version: 1
attach:
  edge: https://edge.example.org
  key_file: /etc/mellomting/agent.key
  decision_servers:
    - url: http://127.0.0.1:18440
      models: [karpo-decision-31b]
      health: /healthz
`

// A decision server is enough on its own (SYSTEMONE_PLAN §D13), and
// show-effective round-trips it.
func TestAttachDecisionServers(t *testing.T) {
	cfg, err := Parse([]byte(attachDecision))
	if err != nil {
		t.Fatalf("decision-only attach must be valid: %v", err)
	}
	d := cfg.Attach.DecisionServers
	if len(cfg.Attach.Servers) != 0 || len(d) != 1 || d[0].Models[0] != "karpo-decision-31b" || d[0].HealthPath() != "/healthz" {
		t.Fatalf("decision server not carried: %+v", cfg.Attach)
	}
	if got := (DecisionServer{}).HealthPath(); got != DefaultDecisionHealth {
		t.Fatalf("default health = %q", got)
	}
	_, eff, err := ParseEffective([]byte(attachDecision))
	if err != nil {
		t.Fatal(err)
	}
	if again, err := Parse(eff); err != nil || again.Attach.DecisionServers[0].Health != "/healthz" {
		t.Fatalf("show-effective must parse back to the same decision server: %v\n%s", err, eff)
	}
}

func TestAttachDecisionValidation(t *testing.T) {
	const head = "version: 1\nattach:\n  edge: https://e.example.org\n  key_file: /k\n"
	for body, want := range map[string]string{
		"  decision_servers:\n    - url: http://127.0.0.1:1\n":                                                         "models: at least one",
		"  decision_servers:\n    - url: http://127.0.0.1:1\n      models: [\"a b\"]\n":                                "must be ASCII",
		"  decision_servers:\n    - url: http://127.0.0.1:1\n      models: [m, m]\n":                                   "listed twice",
		"  decision_servers:\n    - url: http://127.0.0.1:1/v1\n      models: [m]\n":                                   "path",
		"  servers: [\"http://127.0.0.1:1\"]\n  decision_servers:\n    - url: http://127.0.0.1:1\n      models: [m]\n": "listed twice",
		"  decision_servers:\n    - url: http://127.0.0.1:1\n      models: [m]\n      health: healthz\n":               "health",
		"  decision_servers:\n    - url: http://127.0.0.1:1\n      models: [m]\n      health: /a/../metrics\n":         "health",
		"  decision_servers:\n    - url: http://127.0.0.1:1\n      models: [m]\n      health: /health?x=1\n":           "health",
		"  decision_servers:\n    - url: http://127.0.0.1:1\n      models: [m]\n      health: //health\n":              "health",
		"  decision_servers:\n    - url: http://127.0.0.1:1\n      models: [m]\n      health: /h%2f\n":                 "health",
		"  decision_servers:\n    - url: http://127.0.0.1:1\n      models: [m]\n      typo: 1\n":                       "typo",
	} {
		_, err := Parse([]byte(head + body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want error containing %q", body, err, want)
		}
	}
	var many strings.Builder
	many.WriteString(head + "  decision_servers:\n")
	for i := range maxAttachServers + 1 {
		fmt.Fprintf(&many, "    - url: http://127.0.0.1:%d\n      models: [m]\n", 1000+i)
	}
	if _, err := Parse([]byte(many.String())); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("%d decision servers: got %v, want the server cap", maxAttachServers+1, err)
	}
}

// An attachment of only decision models takes the decision timeouts of
// a configured decision-only server, its queue wait no longer than the
// request bound.
func TestAgentDecisionBackend(t *testing.T) {
	a := Agents{MaxConcurrency: 8, QueueSize: 32, QueueTimeout: Duration(120e9)}
	b := AgentDecisionBackend(a)
	if b.RequestTimeout != Duration(decisionRequestTimeout) || b.HeaderTimeout != Duration(decisionHeaderTimeout) {
		t.Fatalf("timeouts = %v/%v, want the decision bounds", b.HeaderTimeout, b.RequestTimeout)
	}
	if b.QueueTimeout != Duration(decisionRequestTimeout) || b.MaxConcurrency != 8 || b.QueueSize != 32 {
		t.Fatalf("admission = %+v, want the agents' caps with the wait lowered", b)
	}
	if g := AgentBackend(a); g.RequestTimeout != Duration(defaultRequestTimeout) {
		t.Fatalf("generation attachment request timeout = %v", g.RequestTimeout)
	}
}
