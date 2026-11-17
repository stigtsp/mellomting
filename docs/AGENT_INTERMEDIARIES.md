# Agent attach behind proxies and WAFs (open problem)

**Status:** noted 2026-11-15 during the Brev pilot. Nothing is decided or
implemented, and nothing here amends PLAN.md. Desk analysis only: the
claims about Cloudflare are **unverified** and need a test.

## The problem

An agent attaches with an HTTP/1.1 upgrade to a custom protocol token
(PLAN §104, `internal/agent/doc.go`):

```
GET /v1/agent/attach HTTP/1.1
Connection: Upgrade
Upgrade: mellomting-agent/1
```

After `101 Switching Protocols` the connection carries prior-knowledge
HTTP/2 with reversed roles, and HTTP/2 PINGs every 15 s. Every hop between
the agent and the edge has to:

1. forward the hop-by-hop `Connection` and `Upgrade` headers;
2. accept an Upgrade token that is not `websocket`;
3. switch to a byte tunnel after the 101, and not parse or time out what
   follows.

Tailscale Funnel (a Go reverse proxy) does all three, and the pilot has run
on it since 2026-11-14. Other common front ends differ.

## By intermediary

| Front end | Expected | Notes |
| --- | --- | --- |
| Tailscale Funnel | Works (in use) | |
| nginx | Should work | It tunnels any 101 whatever the token, but only with `proxy_http_version 1.1` and `Upgrade`/`Connection` set explicitly (they are hop-by-hop). For client traffic: `client_max_body_size` defaults to 1m, so large prompts get 413 (the edge allows 16 MiB). `proxy_buffering off` for SSE. `proxy_read_timeout` long enough for long non-streamed requests. |
| Caddy, HAProxy, Envoy | Probably works | Generic upgrade support. Not checked. |
| Cloudflare proxy (orange cloud) | **Likely fails, unverified** | Documented support is for `Upgrade: websocket`; an unknown token may be stripped or refused. Bot or WAF rules may challenge non-browser clients (the agent and API clients alike). There is a 100 s origin response timeout (524) for requests without response bytes, which matters for long non-streamed client requests too. |
| Cloudflare Tunnel / Spectrum (TCP) | Probably works | Raw TCP, so there is no HTTP parsing in the way. |

## Options for later

1. **Document it only:** the requirement and an nginx example in
   OPERATIONS.md, plus "use a DNS-only hostname or a TCP tunnel for agent
   attach behind CDNs". No code.
2. **Split hostnames:** clients go through the CDN or WAF, and agents
   attach on a separate hostname that bypasses it. Operational, no code.
3. **WebSocket transport:** an alternative attach that upgrades to
   `websocket` and carries the same reversed HTTP/2 inside WebSocket binary
   frames. This passes most CDNs and WAFs. Costs:
   - a WebSocket implementation (a dependency or about 300 lines);
   - framing overhead;
   - a second code path for the attach security boundary, which needs
     PLAN §104 amended, with tests mirroring the existing attach tests;
   - the security properties must be identical: key check before upgrade,
     slot reservation, the namespace from the key.
4. **Better diagnosis:** `mellomting agent check` currently proves only
   that the attach path is reachable (a GET without upgrade returns 426).
   It could also try the upgrade and report "intermediary stripped or
   refused the Upgrade", so a misconfigured front end fails with a clear
   message instead of a generic attach error.

Suggested order: 4 and 1 first (cheap, and they make failures obvious),
then 3 only if a real deployment needs a CDN in front of agents.

## How to test a front end

Run a real agent against any backend, for example a stub that serves
`/v1/models`, through the front end. Check for `agent attached` in the
agent log and the model in the edge's `/v1/models`. A GET to the attach
path alone is not enough: it shows the path is reachable, not that the
upgrade passes through.
