package config

import (
	"strings"
	"testing"
)

const agentsBase = `version: 1
server:
  listen: {network: tcp, address: "127.0.0.1:8080"}
auth:
  users_file: /etc/mellomting/users.yaml
  pepper_file: /etc/mellomting/auth.pepper
`

func TestAgentsOnlyConfig(t *testing.T) {
	cfg, err := Parse([]byte(agentsBase + "agents:\n  enabled: true\n"))
	if err != nil {
		t.Fatalf("agents-only edge must be valid: %v", err)
	}
	if cfg.Agents.MaxAttachments != defaultAgentMaxAttachments || cfg.Agents.MaxConcurrency != defaultMaxConcurrency || cfg.Agents.MaxModels != defaultAgentMaxModels {
		t.Fatalf("defaults not applied: %+v", cfg.Agents)
	}
	if _, err := Parse([]byte(agentsBase)); err == nil {
		t.Fatal("no servers, no models and agents disabled must be refused")
	}
}

func TestAgentsValidation(t *testing.T) {
	for doc, want := range map[string]string{
		"servers:\n  a: {url: \"http://127.0.0.1:8000\"}\nmodels:\n  m: {servers: [a]}\nagents:\n  max_attachments: 4\n": "agents.enabled is false",
		"agents:\n  enabled: true\n  max_attachments: 999\n":                                                             "agents.max_attachments",
		"agents:\n  enabled: true\n  max_concurrency: 5000\n":                                                            "agents.max_concurrency",
		"agents:\n  enabled: true\n  max_models: 1000\n":                                                                 "agents.max_models",
	} {
		_, err := Parse([]byte(agentsBase + doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want error containing %q", doc, err, want)
		}
	}
}

func TestAgentNamespaceAndNames(t *testing.T) {
	for ns, ok := range map[string]bool{"lab": true, "exp-1.a_b": true, "*": true, "": false, "Lab": false, "a/b": false, "-x": false} {
		if got := ValidateAgentNamespace(ns) == nil; got != ok {
			t.Errorf("ValidateAgentNamespace(%q) ok=%v, want %v", ns, got, ok)
		}
	}
	if AgentModelName("lab", "org/m") != "lab/org/m" || AgentModelName("*", "m") != "m" {
		t.Fatal("AgentModelName")
	}
	if ValidateModelName("lab/*") == nil {
		t.Fatal(`a model named "lab/*" would read as a namespace grant`)
	}
}
