// Package agent implements self-registering backends (PLAN §104).
//
// An agent (serve with an attach section) runs next to an inference
// server that the edge cannot reach, for example on a cloud GPU instance
// without inbound access. It dials the edge's ingress listener, authenticates with an
// agent key and asks to upgrade the connection:
//
//	GET /v1/agent/attach HTTP/1.1
//	Authorization: Bearer sk-...
//	Connection: Upgrade
//	Upgrade: mellomting-agent/1
//
// After "101 Switching Protocols" the roles reverse: the edge is the
// HTTP/2 client and the agent the HTTP/2 server, speaking prior-knowledge
// HTTP/2 over the connection the agent opened. Each request the edge
// forwards is one HTTP/2 stream, so streaming, cancellation and flow
// control come from the protocol. The agent relays every stream to its
// local inference server.
//
// On attach the edge asks the agent for GET /v1/models and registers
// each advertised model as "<namespace>/<id>", where the namespace comes
// from the agent key, never from the agent. The models leave the routing
// table when the connection ends, and when the agent key is revoked,
// disabled or expires.
//
// The edge makes no outbound connection for an agent: the sandbox's
// connect rules are unchanged.
package agent

const (
	// AttachPath is the ingress path agents attach on.
	AttachPath = "/v1/agent/attach"
	// UpgradeProtocol is the Upgrade token of the attach handshake.
	UpgradeProtocol = "mellomting-agent/1"
)
