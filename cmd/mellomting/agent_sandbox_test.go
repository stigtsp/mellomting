package main

import (
	"net"
	"slices"
	"testing"

	"mellomting/internal/config"
)

// An edge serving only attached models dials nothing, so its sandbox
// grants no TCP connect at all (PLAN §104.5).
func TestAgentsOnlyEdgeGrantsNoConnect(t *testing.T) {
	cfg, err := config.Parse([]byte(`version: 1
server:
  listen: {network: tcp, address: "127.0.0.1:8080"}
auth:
  users_file: /etc/mellomting/users.yaml
  pepper_file: /etc/mellomting/auth.pepper
agents:
  enabled: true
`))
	if err != nil {
		t.Fatal(err)
	}
	pol, err := sandboxPolicy(cfg, []net.Addr{&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}})
	if err != nil {
		t.Fatal(err)
	}
	if len(pol.ConnectTCP) != 0 || pol.Resolve {
		t.Fatalf("agents-only edge grants connect %v, resolve %v; want none", pol.ConnectTCP, pol.Resolve)
	}
}

// A daemon that only attaches may connect to the edge and its servers,
// resolve the edge's name, and read nothing: its keys are read before
// confinement and it has no users file to reload.
func TestAttachOnlySandboxPolicy(t *testing.T) {
	cfg, err := config.Parse([]byte(`version: 1
attach:
  edge: https://edge.example.org
  key_file: /etc/mellomting/agent.key
  servers: ["http://127.0.0.1:8000", "http://127.0.0.1:8001"]
`))
	if err != nil {
		t.Fatal(err)
	}
	pol, err := sandboxPolicy(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pol.ConnectTCP, []uint16{443, 8000, 8001}) {
		t.Errorf("connect %v, want the edge's 443 and both servers", pol.ConnectTCP)
	}
	if !pol.Resolve {
		t.Error("an edge named by hostname must keep the resolver")
	}
	if len(pol.ReadPaths) != 0 || len(pol.WriteFiles) != 0 || len(pol.Listeners) != 0 {
		t.Errorf("attach-only policy grants read %v, write %v, listen %v; want none", pol.ReadPaths, pol.WriteFiles, pol.Listeners)
	}
}

// A decision server's port is granted like a discovered server's.
func TestAttachDecisionSandboxPolicy(t *testing.T) {
	cfg, err := config.Parse([]byte(`version: 1
attach:
  edge: https://edge.example.org
  key_file: /etc/mellomting/agent.key
  decision_servers:
    - url: http://127.0.0.1:18440
      models: [karpo-decision-31b]
`))
	if err != nil {
		t.Fatal(err)
	}
	pol, err := sandboxPolicy(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pol.ConnectTCP, []uint16{443, 18440}) {
		t.Errorf("connect %v, want the edge's 443 and the decision server", pol.ConnectTCP)
	}
}
