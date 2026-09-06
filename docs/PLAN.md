# Mellomting — PLAN.md

> **Mellomting** is a small, security-conscious, OpenAI-compatible LLM proxy and multiplexer written in Go.
>
> Its job is deliberately narrow: authenticate clients, apply simple policy and rate limits, route model names to one or more inference backends, stream responses correctly, account for token usage, and confine itself after startup.

**Plan revision:** 2  
**Research baseline:** 2026-08-19  
**Primary deployment:** one Linux host, multiple local vLLM servers, coding agents as clients.

---

## 0. Instructions to the implementation agent

Treat this document as a design specification, not a loose feature wish-list.

The words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY** indicate priority in the usual RFC 2119 sense.

Important rules:

- Implement the phases in order.
- Do not introduce a web UI, database server, plugin system, or management HTTP API.
- Keep dependencies small and justified.
- Prefer standard-library Go.
- Do not weaken a security requirement merely to make a test pass.
- Security-sensitive behaviour MUST have automated tests.
- Unknown OpenAI/vLLM request fields SHOULD pass through rather than being rejected.
- The proxy MUST fail closed for authentication, authorization, malformed configuration, and required sandbox setup.
- When this plan gives a concrete default, use it unless testing shows it is impractical.
- If an upstream API has changed since this document was written, preserve the architectural/security intent and update `COMPATIBILITY.md`.

---

# 1. Product definition

Mellomting provides one endpoint in front of multiple LLM inference servers:

```text
                    clients
          Codex / OpenCode / Aider / SDKs
                       |
                       | OpenAI-compatible HTTP API
                       v
              +------------------+
              |    Mellomting    |
              |------------------|
              | authentication   |
              | model ACLs       |
              | rate limiting    |
              | routing/fallback |
              | token accounting |
              | optional guards  |
              | Landlock         |
              +----+--------+----+
                   |        |
                   v        v
             vLLM :8001  vLLM :8002
               model A      model B
```

The core deployment benefit is:

```text
one Go binary
one YAML config
one YAML API-key file
zero database servers
zero application control plane
```

The default interface exposed to clients should be something such as:

```text
http://127.0.0.1:8080/v1
```

or preferably, when used behind nginx/Caddy:

```text
unix:/run/mellomting/mellomting.sock
```

Clients choose a public model name:

```json
{
  "model": "qwen-coder",
  "messages": [...]
}
```

Mellomting decides that `qwen-coder` should be served by, for example:

```text
http://127.0.0.1:8001
```

and may rewrite the model field to the model name expected by that vLLM process.

---

# 2. Design goals

## 2.1 Primary goals

Mellomting MUST be:

- easy to deploy;
- easy to audit;
- suitable for local/self-hosted LLMs;
- suitable for coding agents;
- compatible with vLLM's OpenAI-compatible API;
- secure by default;
- streaming-first;
- usable without Docker;
- usable without a reverse proxy;
- especially well suited to operation behind a reverse proxy;
- capable of routing several public model names to different local backends;
- capable of using multiple equivalent backends for one public model;
- capable of per-key request and token accounting;
- capable of per-key model ACLs and rate limits;
- capable of post-start Landlock confinement.

## 2.2 Philosophy

The correct mental model is:

> **A small authenticated inference router, not an AI platform.**

Adding a feature is not automatically an improvement.

The main competitive advantage over products such as Bifrost or LiteLLM should be:

> **Mellomting is small enough that an experienced operator can understand its security boundary.**

---

# 3. Explicit non-goals

The initial product MUST NOT contain:

- a web dashboard;
- an HTTP management API;
- browser login/session handling;
- OAuth/OIDC;
- PostgreSQL;
- Redis;
- arbitrary plugins;
- dynamic Go plugin loading;
- embedded JavaScript/Python/Lua;
- MCP execution;
- tool execution;
- shell execution;
- arbitrary URL fetching;
- web browsing;
- vector databases;
- semantic caches;
- prompt-management systems;
- agent execution;
- billing or payment processing;
- organizations/teams/customer hierarchies;
- Kubernetes-specific control-plane logic;
- dynamic LoRA management;
- arbitrary reverse-proxying of backend paths.

Cost accounting is also out of scope initially. Track tokens, requests, errors, and latency; do not build a pricing database.

---

# 4. Security invariants

These are non-negotiable project invariants.

1. Anonymous inference is **disabled by default**.
2. There is **no network-reachable administration interface**.
3. Client API keys are **never stored plaintext**.
4. Client API keys are **never logged**.
5. Prompts and responses are **never persisted by default**.
6. Backend credentials are **never returned to clients**.
7. Backend URLs are **never controlled by client input**.
8. Client requests can only reach an explicit allow-list of inference paths.
9. There is **no generic catch-all reverse proxy route**.
10. Request bodies are bounded.
11. Response event sizes are bounded.
12. Concurrent requests are bounded.
13. Backend queues are bounded.
14. Retry counts are bounded.
15. Accounting queues are bounded.
16. A streamed request is never retried after response bytes have been emitted.
17. A required Landlock policy may never silently degrade to no sandbox.
18. Landlock MUST be applied to **all Go runtime threads**, not only the calling OS thread.
19. Multipath TCP MUST be explicitly disabled where Landlock TCP restrictions are relied upon.
20. Config reload must never widen the active Landlock sandbox.
21. A client may not bypass a public model ACL using a backend model name.
22. Unknown response IDs must never be broadcast to multiple backends to discover their owner.
23. Remote qualifier models may not receive prompt contents without explicit
    configuration. (Forward-looking: qualifiers are shelved for v1 and the
    source schema has no `qualifiers` key, so nothing in the running daemon
    can reach a qualifier. See §96.)
24. Mellomting emits no product telemetry unless a future operator explicitly configures it.

---

# 5. Threat model

## 5.1 Threats Mellomting should directly address

Assume that clients may intentionally attempt:

- bearer-token brute force;
- API-key enumeration;
- malformed HTTP;
- malformed JSON;
- oversized request bodies;
- huge request headers;
- duplicate or confusing model fields;
- slowloris behaviour;
- connection exhaustion;
- request floods;
- long-running stream exhaustion;
- backend queue exhaustion;
- excessive output requests;
- retry amplification;
- malformed SSE;
- response-size attacks;
- log injection;
- cross-key response-ID access;
- attempts to reach backend administrative endpoints;
- attempts to smuggle backend authentication headers;
- attempts to bypass model ACLs;
- attempts to exploit the optional qualifier.

Also assume Mellomting itself may eventually contain an exploitable bug. Landlock and host hardening exist specifically to reduce post-compromise impact.

## 5.2 What Landlock does not solve

Landlock is defence in depth, not a magical RCE fix.

If Mellomting is compromised, an attacker may still be able to misuse resources Mellomting is intentionally allowed to access, including:

- already-open files;
- the accounting file descriptor;
- configured backend ports;
- backend credentials already present in process memory;
- the qualifier backend;
- any remote destination permitted by external network policy.

Therefore:

- local inference backends SHOULD expose only inference functionality that is actually required;
- dangerous vLLM development/admin functionality SHOULD remain disabled;
- local backends SHOULD run as different unprivileged users;
- destination-address restrictions SHOULD complement Landlock;
- remote provider credentials substantially increase post-compromise impact.

## 5.3 Out of scope

Mellomting cannot by itself protect against:

- kernel compromise;
- host root compromise;
- malicious host administrators;
- malicious model server code;
- physical attacks;
- volumetric upstream DDoS;
- compromise of the TLS terminator in front of Mellomting.

---

# 6. Recommended deployment architecture

The preferred hardened deployment is:

```text
                         Linux host
+----------------------------------------------------------------+
|                                                                |
|  Internet/LAN/VPN                                              |
|       |                                                        |
|       v                                                        |
|  nginx or Caddy :443                                           |
|       |                                                        |
|       | Unix domain socket                                     |
|       v                                                        |
|  /run/mellomting/mellomting.sock                               |
|       |                                                        |
|       v                                                        |
|  Mellomting (UID mellomting / group mellomting)               |
|       |                                                        |
|       +------------------+-------------------+                  |
|       |                  |                   |                  |
|       v                  v                   v                  |
|  127.0.0.1:8001     127.0.0.1:8002      127.0.0.1:8100        |
|    vLLM A              vLLM B          optional qualifier      |
|                                                                |
|  Mellomting:                                                   |
|    no capabilities                                             |
|    Landlock after startup                                      |
|    systemd hardening                                           |
|    localhost-only backend network                              |
|                                                                |
+----------------------------------------------------------------+
```

The reverse proxy should normally do:

- public TLS;
- optional client-IP connection limiting;
- network ACLs;
- HTTP access policy unrelated to LLM identities.

Mellomting should do:

- LLM API authentication;
- per-key authorization;
- model routing;
- LLM-aware limits;
- inference accounting;
- optional qualification.

This keeps responsibilities clean.

---

# 7. Binary and process model

Executable name:

```text
mellomting
```

Command surface:

```text
mellomting init
mellomting serve
mellomting install
mellomting key create|list|enable|disable|revoke
mellomting config check|show-effective
mellomting usage report
mellomting sandbox check
mellomting version
```

`mellomting install [--systemd]` is the sole installation command shape; a
top-level `--install` alias MUST NOT exist. `mellomting init` is the
initialization shortcut for a local deployment and requires at least one
`--server`. Every command MUST support focused `--help`.

The daemon should be a single process.

Do not fork workers.

Prefer:

```text
CGO_ENABLED=0
```

for release binaries.

Primary platforms:

```text
linux/amd64
linux/arm64
```

Landlock is Linux-specific. Non-Linux builds MAY exist for development but MUST clearly report that Landlock is unavailable.

---

# 8. Network listeners

## 8.1 Default listener

Default to a Unix socket when a path is configured by the package/service installation:

```yaml
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: "0660"
```

For standalone/manual use:

```yaml
server:
  listen:
    network: tcp
    address: 127.0.0.1:8080
```

## 8.2 Unsafe TCP exposure protection

If Mellomting is configured to listen on a non-loopback TCP address with TLS disabled, startup MUST fail unless the operator explicitly sets something equivalent to:

```yaml
server:
  allow_plaintext_non_loopback: true
```

The startup log MUST clearly warn about this mode.

## 8.3 Unix socket safety

When creating a pathname Unix socket:

- use a restrictive umask;
- refuse to unlink a symlink;
- refuse to unlink an existing non-socket file;
- validate ownership where practical;
- set configured socket mode after bind;
- prefer a systemd `RuntimeDirectory=mellomting`.

---

# 9. HTTP server implementation

Use Go's `net/http` server.

Do **not** embed the entire Caddy server in the core daemon.

Reasons:

- lower dependency count;
- smaller attack surface;
- explicit listener/dialer ownership;
- easier Landlock reasoning;
- easier MPTCP control;
- easier protocol fuzzing.

Optional ACME can later use Caddy's `certmagic` library without embedding the Caddy server.

## 9.1 HTTP limits

Suggested initial defaults:

```yaml
server:
  max_header_bytes: 32768
  max_body_bytes: 16777216
  max_response_bytes: 67108864

  read_header_timeout: 5s
  read_body_timeout: 60s
  idle_timeout: 120s
  stream_idle_timeout: 120s
  stream_write_timeout: 30s

  max_inflight_requests: 64
  max_buffered_request_bytes: 67108864
```

All limits MUST be configurable but MUST have finite defaults.

`max_response_bytes` bounds the total response in both modes (FIX-12):
the buffered path (non-stream and error upstream bodies) and the live SSE
pump's cumulative emitted bytes. A backend streaming small events forever
is therefore cut off at the same cap as an over-large buffered response,
with a `backend_stream_error` stream class; `stream_idle_timeout` remains
the per-event idle bound in addition.

Do not use a normal short global `WriteTimeout` that breaks long LLM streams. Instead use a per-write/per-stream idle policy.

## 9.2 Request compression

MVP behaviour:

- accept `Content-Encoding: identity`;
- reject compressed request bodies.

This avoids decompression bombs and unnecessary parser complexity.

Mellomting SHOULD request uncompressed backend responses when it must parse them for accounting.

---

# 10. Request processing pipeline

The pipeline ordering matters.

```text
TCP/Unix admission
        |
HTTP header limits/timeouts
        |
pre-auth source limiter
        |
authenticate API key
        |
global + per-key concurrency admission
        |
bounded body acquisition
        |
parse endpoint envelope
        |
endpoint authorization
        |
model lookup + model ACL
        |
request policy / output cap
        |
token-quota admission
        |
optional input qualifier
        |
Responses affinity lookup
        |
backend selection
        |
backend concurrency/queue
        |
forward request
        |
stream/parse response
        |
optional output qualifier
        |
usage settlement
        |
accounting record
```

Authentication MUST happen before reading a potentially large body.

---

# 11. OpenAI-compatible API scope

The goal is not to clone every OpenAI backend behaviour. The goal is to correctly proxy the OpenAI-compatible APIs needed by vLLM and coding agents.

## 11.1 MVP endpoints

MUST support:

```text
GET  /v1/models
POST /v1/chat/completions
POST /v1/completions
POST /v1/responses
GET  /v1/responses/{response_id}
POST /v1/responses/{response_id}/cancel
POST /v1/embeddings
```

The Responses retrieve/cancel endpoints matter because current vLLM supports stateful Responses API operations.

## 11.2 Later endpoints

Consider after the core is stable:

```text
POST /v1/chat/completions/batch
POST /v1/audio/transcriptions
POST /v1/audio/translations
POST /v1/rerank
POST /v1/messages
POST /v1/messages/count_tokens
```

The Anthropic Messages API is attractive for coding-agent compatibility because current vLLM exposes an Anthropic-compatible interface. It should initially be **pass-through**, not protocol translation.

## 11.3 No catch-all proxy

This is critical.

Mellomting MUST return 404 for unimplemented paths.

It MUST NOT forward arbitrary paths to vLLM.

For example, a client must never be able to reach backend endpoints such as:

```text
/metrics
/start_profile
/stop_profile
/v1/load_lora_adapter
/v1/unload_lora_adapter
/docs
```

merely because the backend happens to expose them.

---

# 12. Compatibility strategy: shallow parse, broad pass-through

Mellomting should understand only what it needs for routing and policy.

For JSON inference requests, decode the top-level object into something equivalent to:

```go
map[string]json.RawMessage
```

Inspect only fields such as:

```text
model
stream
stream_options
previous_response_id
max_tokens
max_completion_tokens
max_output_tokens
```

Unknown fields MUST be retained and forwarded.

This is important for:

- tool calling;
- structured output;
- reasoning fields;
- new OpenAI fields;
- vLLM-specific extensions;
- coding-agent-specific parameters.

Do not use strict endpoint structs that accidentally discard new fields.

## 12.1 Bounded buffering

Because the model field is in the JSON body, Mellomting needs to inspect the request before routing it.

MVP may buffer one bounded JSON object, but aggregate memory MUST also be bounded.

Use a process-wide weighted byte budget.

If `Content-Length` is known:

```text
reserve approximately the required body memory before reading
```

If it is unknown:

```text
reserve a conservative amount and enforce the hard body limit while reading
```

Account for peak memory caused by decode + re-encode.

A request that cannot acquire memory budget quickly should receive a bounded overload error rather than wait indefinitely.

---

# 13. Model aliases

The operator-facing configuration is server-oriented. Operators name the
inference servers and the public models routed to them; Mellomting normalizes
this into the internal backend/model routing representation. Synthetic
internal backend names never appear in operator-facing configuration.

Example:

```yaml
servers:
  local-a:
    url: http://127.0.0.1:8000
  local-b:
    url: http://127.0.0.1:8010

models:
  qwen-coder:
    type: generation
    strategy: least-inflight
    servers: [local-a, local-b]

    policy:
      max_output_tokens: 32768
```

Clients see only:

```text
qwen-coder
```

Backend A may require:

```text
Qwen/Qwen3-Coder-...
```

Backend B may use another served name. The public model map key is the public
name; `upstream_model` selects the ID sent upstream (defaults to the public
map key).

Mellomting rewrites the outbound `model` field.

Client-supplied model names MUST match configured public model names exactly.

Do not permit implicit:

```text
provider/model
backend/model
```

syntax that bypasses ACLs.

The development-era `backends:` source form is not accepted; it is rejected as
an unknown field rather than migrated or accepted in parallel.

---

# 14. `/v1/models`

Mellomting synthesizes `/v1/models`.

The response MUST be filtered by the authenticated key's model ACL.

A key that can access:

```text
qwen-coder
embedding-small
```

must not discover other configured models.

The list should contain stable OpenAI-like model objects with at least:

```text
id
object
created
owned_by
```

`owned_by` may be:

```text
mellomting
```

Do not expose backend URLs or backend implementation names.

---

# 15. Backend configuration

A backend is an OpenAI-compatible inference server. The operator-facing source
configuration names servers and public models; the loader normalizes the
server-oriented form into the internal backend/model representation before
strict validation. The source schema is the sole accepted schema.

Server fields:

```yaml
servers:
  NAME:
    url: URL            # literal IPv4/IPv6 with explicit port
    api_key_file: PATH  # optional; not accepted by init
```

Model fields:

```yaml
models:
  PUBLIC_NAME:
    type: generation         # optional, default generation
    servers: [SERVER_NAME]
    upstream_model: ID       # optional, default public map key
    strategy: least-inflight # optional; single when one server,
                             # least-inflight when several
    policy:                  # optional
      max_output_tokens: 32768
```

Internal backend names are generated deterministically by D12 and are never
exposed to operators.

## 15.1 Backend URL validation

Backend URLs MUST be parsed and validated during startup.

Reject:

- unsupported URL schemes;
- URL fragments;
- userinfo unless explicitly supported;
- unexpected query strings;
- malformed ports;
- paths that make endpoint construction ambiguous;
- hostnames and empty hosts for discovery servers (literal IPv4 or IPv6 with
  explicit port required, D6/D9).

In local-only mode, require literal loopback destinations.

Construct backend endpoint paths from trusted configuration plus an internal endpoint enum. Do not use an attacker-controlled URL or arbitrary `ResolveReference` input.

---

# 16. Backend network modes

Example:

```yaml
security:
  backend_network:
    mode: loopback-only
```

Supported modes:

```text
loopback-only
allowed-cidrs
any
```

Default:

```text
loopback-only
```

## 16.1 `loopback-only`

Allow only:

```text
127.0.0.0/8
::1/128
```

Prefer IP literals rather than DNS names.

This should be the documented standard deployment.

## 16.2 `allowed-cidrs`

For remote/private inference servers, resolve through a controlled dialer and verify that the selected IP belongs to configured CIDRs.

Revalidate each new connection.

## 16.3 `any`

This weakens post-compromise egress guarantees.

Require an explicit configuration opt-in and emit a startup warning.

---

# 17. Backend authentication

Local vLLM backends may not need credentials.

When credentials are required, support secret files rather than embedding secrets directly in YAML:

```yaml
servers:
  remote-a:
    url: https://192.168.1.5:8443
    api_key_file: /run/credentials/mellomting/provider-a
```

Potential future support:

```text
systemd credentials
environment references
```

but secret-file support is sufficient initially.

Rules:

- read secrets before Landlock;
- close the secret file;
- keep only the required value in memory;
- never log it;
- never forward client `Authorization` or `x-api-key`;
- inject backend authentication from trusted config.

---

# 18. Header policy

Strip hop-by-hop headers.

MUST remove client-controlled authentication/proxy identity headers before forwarding, including at least:

```text
Authorization
Proxy-Authorization
X-Api-Key
Forwarded
X-Forwarded-For
X-Forwarded-Host
X-Forwarded-Proto
```

Backend authentication is then added by Mellomting.

Normal end-to-end headers and client `User-Agent` MAY pass through where safe.

Do not forward an arbitrary client `Host`.

## 18.1 Trusted client IPs

Rate limiting by source IP must not blindly trust `X-Forwarded-For`.

Only consume proxy headers when:

- the peer is a configured trusted proxy CIDR; or
- the connection arrives through the explicitly configured Unix reverse-proxy socket.

Otherwise use the socket peer address.

`server.trusted_proxies` is that configuration: a list whose entries are CIDRs,
or the literal `unix` for the peer of a Unix-socket listener, which has no
address of its own — whoever may open the socket is the proxy. It is empty by
default, and an empty list means `X-Forwarded-For` is never read.

From a trusted peer, the client address is the RIGHTMOST entry of the
`X-Forwarded-For` chain that is not itself a trusted proxy. Right-to-left is
what makes it safe: a client may prepend anything it likes, but its forgeries
sit to the left of the entry the trusted proxy appended, so they are never
reached. A chain that breaks — an element that is not an address — falls back
to the socket peer rather than believing what lies beyond the break, and the
number of hops walked is bounded.

The resolved address governs per-source pre-auth rate limiting (§33) and the
address recorded in operator logs and accounting. It never affects
authentication or authorization, which depend on the API key alone, so a
misconfigured `trusted_proxies` cannot grant access.

---

# 19. Routing strategies

Initial strategies:

```text
single
round-robin
weighted-round-robin
least-inflight
weighted-least-inflight
```

Recommended default for equivalent local model replicas:

```text
least-inflight
```

Weighted least-inflight can approximately score:

```text
(inflight + 1) / weight
```

but keep the implementation deterministic and testable.

Do not implement latency-learning/adaptive routing in v1.

---

# 20. Optional stickiness

Coding agents often resend large common prefixes. Stable routing can improve backend prefix-cache reuse.

Later support MAY include:

```yaml
models:
  qwen-coder:
    stickiness:
      mode: api-key
```

or:

```yaml
stickiness:
  mode: header
  header: X-Mellomting-Session
```

Use rendezvous hashing rather than an unbounded session map where possible.

Responses API affinity, described below, takes precedence over generic stickiness.

---

# 21. Responses API affinity

The Responses API is not always stateless.

A request can refer to:

```text
previous_response_id
```

and clients can retrieve or cancel an existing response.

If one public model uses several backends, Mellomting MUST remember which backend owns a response.

## 21.1 Affinity record

Maintain a bounded in-memory mapping:

```text
(API key ID, public response ID)
    ->
(public model, backend ID, expiry)
```

Bind response state to the API key that created it.

A different key must not be able to retrieve/cancel/use the response even if it obtains the ID.

## 21.2 Capture

Capture response IDs from:

- non-streaming Responses JSON;
- `response.created` / equivalent streaming events.

## 21.3 Lookup

For:

```text
GET /v1/responses/{id}
POST /v1/responses/{id}/cancel
POST /v1/responses with previous_response_id
```

route to the owning backend.

Unknown IDs:

- if there is one and only one possible backend, MAY be forwarded there;
- if several backends could own the ID, return a controlled 404/409;
- MUST NOT probe all backends.

## 21.4 Bounds

Configure:

```yaml
responses:
  affinity_ttl: 2h
  max_affinity_entries: 10000
```

Use bounded eviction.

Document that proxy restart clears v1 affinity state.

Do not persist this mapping until a concrete need appears.

---

# 22. Backend admission and queues

Each backend has:

```text
active request semaphore
bounded waiting queue
queue timeout
```

Example:

```yaml
max_concurrency: 4
queue_size: 8
queue_timeout: 5s
```

If capacity is unavailable:

1. select another eligible backend if routing allows;
2. otherwise return a controlled overload response.

Never create one goroutine per unbounded queued request.

The global request limiter must prevent attackers from filling every backend queue indefinitely.

---

# 23. Retry and fallback semantics

Take the useful Bifrost idea of bounded retries/fallbacks, but make Mellomting conservative.

Default:

```yaml
retry:
  max_attempts: 1
```

That means no retry unless explicitly enabled.

When enabled:

```yaml
retry:
  max_attempts: 2
  initial_backoff: 100ms
  max_backoff: 1s
  jitter: true
```

Retry candidates may include:

- connection failure;
- connection timeout;
- 429;
- 502;
- 503;
- 504.

Do not automatically retry:

- authentication failures;
- client validation errors;
- qualifier blocks;
- client cancellation;
- policy errors;
- arbitrary 4xx;
- a stream after any bytes have reached the client.

Use exponential backoff with jitter.

Total retry attempts across fallback backends MUST have a global bound so a configuration cannot accidentally create multiplicative retry amplification.

---

# 24. Streaming

Streaming is a first-class requirement.

Mellomting MUST:

- preserve SSE event order;
- flush promptly;
- avoid whole-response buffering;
- notice client disconnects;
- cancel the upstream request promptly;
- enforce a backend stream-idle timeout;
- enforce a client write-idle timeout;
- bound an individual parsed SSE event;
- never retry after output begins.

Do not use `bufio.Scanner` with its default token size.

Implement an SSE parser with an explicit event-size limit.

Unknown SSE fields/events should pass through unchanged.

---

# 25. Authentication

Use opaque bearer keys.

Format (D18):

```text
sk-<username>-<keyid>-<secret>
```

Example:

```text
sk-codex-4f92c16a0b7de831-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

Properties:

- `username` is `^[a-z][a-z0-9_]{0,31}$` and equals `key create --name`;
- `keyid` is 8 random bytes as 16 lowercase hex characters;
- `secret` is 32 random bytes (256 bits) as 64 lowercase hex characters;
- ID and secret generated independently with `crypto/rand`;
- no additional separator or suffix is accepted.

Accepted headers:

```text
Authorization: Bearer sk-...
```

Optionally, for compatibility:

```text
x-api-key: sk-...
```

Never accept credentials in query parameters.

---

# 26. Key storage

Default users file:

```text
/etc/mellomting/users.yaml
```

Example:

```yaml
version: 1

keys:
  - id: 4f92c16a0b7de831
    name: codex
    secret_hash: "hmac-sha256:BASE64..."
    enabled: true
    expires_at: null

    models:
      - qwen-coder
      - embedding-small

    limits:
      requests_per_second: 4
      burst: 8
      concurrent_requests: 4

      tokens_per_hour: 500000
      tokens_per_day: 5000000
```

The service daemon MUST treat this file as read-only.

Only the offline CLI modifies it.

---

# 27. Key hashing

API keys are high-entropy machine credentials, so expensive password hashing on every inference request is unnecessary.

## 27.1 API-key format

Every generated and accepted client API key MUST have exactly this form (D18):

```text
sk-<username>-<keyid>-<secret>
```

The grammar is strict:

- `username` matches `^[a-z][a-z0-9_]{0,31}$` and equals `key create --name`;
- `keyid` is 8 random bytes encoded as exactly 16 lowercase hexadecimal
  characters;
- `secret` is 32 random bytes (256 bits) encoded as exactly 64 lowercase
  hexadecimal characters;
- no segment is empty and no additional separator or suffix is accepted.

Multiple keys may share a username because `keyid` distinguishes them. Only
this grammar is accepted; oversized inputs are rejected before segment
parsing. ID and secret are generated independently with `crypto/rand`.
Authentication extracts the fixed-format ID, runs the dummy-HMAC path for
unknown IDs, verifies the HMAC of the complete raw key in constant time, and
for a matching hash requires the parsed username to equal the stored key name.

Use:

```text
stored = HMAC-SHA-256(server_pepper, full_api_key)
```

Pepper:

```text
/etc/mellomting/auth.pepper
```

Recommended ownership:

```text
root:mellomting
```

Recommended permissions:

```text
0640
```

Validation:

1. syntactically validate the key;
2. extract key ID;
3. lookup key record;
4. calculate HMAC;
5. compare with `subtle.ConstantTimeCompare`;
6. apply enabled/expiry checks.

For an unknown key ID, perform a dummy HMAC operation to reduce trivial timing differences.

The API-key secret itself is never recoverable from the users file.

---

# 28. Secure configuration-file handling

Configuration files are trusted local inputs, but still parse defensively.

MUST:

- impose a maximum YAML file size;
- use strict known-field decoding for Mellomting configuration;
- reject YAML aliases/anchors if they are not needed;
- reject custom YAML tags;
- detect duplicate logical identifiers;
- reject malformed durations;
- reject negative limits;
- reject unknown backend/model references.

Sensitive files SHOULD be opened without following symlinks where practical.

On Linux, consider a small helper built on `openat2()`/`O_NOFOLLOW` for:

```text
config
users file
pepper
backend secret files
TLS key
```

Do not introduce complicated path traversal logic without tests.

---

# 29. Offline key management

No HTTP management API.

Commands:

```text
mellomting key create --name codex --models qwen-coder
mellomting key create --name codex        # infer sole model when unambiguous
mellomting key list
mellomting key disable <keyid>
mellomting key enable <keyid>
mellomting key revoke <keyid>
```

`key create` prints the full key once and it is the only stdout content;
explanatory text goes to stderr (D14, D18).

```text
sk-codex-4f92c16a0b7de831-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

The secret is not stored and cannot be displayed again.

Never accept a plaintext secret as a normal CLI argument.

## 29.1 Atomic users-file updates

Use:

1. lock;
2. parse and validate;
3. create temp file in same directory;
4. mode 0600/0640 as appropriate;
5. write;
6. fsync file;
7. atomic rename;
8. fsync parent directory.

Preserve ownership safely.

---

# 30. Reload semantics

Keep reload deliberately narrow.

`SIGHUP` reloads:

- API keys;
- key enabled state;
- key expiry;
- key model ACLs;
- key rate limits.

It does **not** reload:

- listener;
- backend addresses;
- TLS mode;
- filesystem paths;
- Landlock policy;
- backend connect ports;
- qualifier backend address.

Those require restart.

This prevents reload from trying to widen an already-enforced Landlock sandbox.

A reload must:

1. read the new users file;
2. fully validate it, including the admission gates startup applies — a key
   set startup would have refused to serve MUST NOT be installable through a
   reload;
3. build an immutable auth/policy snapshot;
4. atomically swap the snapshot.

On failure, retain the previous valid snapshot.

The snapshot is one value: the key store and the per-key limit registry built
for it swap together, so a request that resolves its key record and its limit
state across a reload cannot pair the two generations. Anything derived from
the key set — such as whether a token quota is in effect, which decides
stream-usage injection (§38) — MUST be read from the live key record rather
than sampled once at startup.

A `SIGHUP` reload MUST apply under every `security.landlock.mode`. Offline
`key create/disable/revoke` publishes the users file by atomically renaming a
new file over it, and a Landlock rule binds to the inode behind the path when
the ruleset is built — so a policy naming the users file alone stops matching
at the first mutation, the reload is denied, and a revoked key keeps working
until the process is restarted. The post-startup policy therefore grants
read on the DIRECTORY holding the users file (§58), which the rename does not
change. Key rotation and revocation apply on reload, with no restart, in every
mode.

That grant is read-only and covers one directory, but the directory normally
also holds the pepper and the configuration, so a confined daemon can re-open
those too. Both are already read at startup and the pepper is held in memory
for the process lifetime, so the grant widens what a compromised daemon can
re-read rather than what it can reach for the first time. An operator who
wants the narrower blast radius can place the users file in a directory of
its own and point `auth.users_file` at it.

Revocation applies to new requests. Existing inference streams are not forcibly terminated in v1.

---

# 31. Authorization

After authentication:

- endpoint access must be checked;
- requested public model must exist;
- key must be authorized for that model.

For unauthorized/unknown models, prefer a response that does not unnecessarily reveal whether the model exists, such as:

```text
404 model_not_found_or_not_allowed
```

`/v1/models` is the supported discovery mechanism.

Do not expose backend model names.

---

# 32. Rate limiting architecture

Rate limiting should be layered:

```text
source/IP admission
        |
global request limiter
        |
API-key request limiter
        |
API-key concurrency limiter
        |
API-key token budget
        |
model/backend admission
```

Each layer addresses a different failure mode.

---

# 33. Pre-auth flood protection

Authentication itself must not be an unlimited resource.

Before full key processing, enforce:

- connection limits;
- header limits;
- pre-auth requests/second by source;
- bounded invalid-auth log rate.

Do not log every invalid token during a flood.

If behind a trusted reverse proxy, use the validated original client IP.

---

# 34. Request-rate limiting

Per-key configuration:

```yaml
limits:
  requests_per_second: 4
  burst: 8
```

Use a token-bucket or equivalent algorithm.

Also support global request admission:

```yaml
limits:
  global_requests_per_second: 100
  global_burst: 200
```

Rejected requests return:

```text
429 Too Many Requests
```

with a sensible:

```text
Retry-After
```

when calculable.

---

# 35. Concurrency limiting

Per key:

```yaml
concurrent_requests: 4
```

Global:

```yaml
max_inflight_requests: 64
```

Acquire these limits before reading large bodies.

Do not allow a single key to occupy the entire process even if its RPS is low.

---

# 36. Generative output caps

Token accounting alone cannot stop one admitted request from generating an enormous response.

Each generative public model SHOULD define:

```yaml
policy:
  max_output_tokens: 32768
```

Endpoint-specific handling:

```text
Chat Completions:
  max_completion_tokens / supported max_tokens variant

Completions:
  max_tokens

Responses:
  max_output_tokens
```

Policy:

- if a client asks for more than the configured cap, reject;
- if no output limit is supplied, Mellomting MAY inject the configured cap;
- never silently raise a client limit.

Make this behaviour explicit in `COMPATIBILITY.md`.

---

# 37. Token accounting

Track at least:

```text
input_tokens
output_tokens
total_tokens
cached_tokens          if reported
reasoning_tokens       if reported
```

Dimensions:

```text
API key ID
public model
backend
request ID
time
```

Prefer backend-reported token usage.

Do not embed model tokenizers in v1.

---

# 38. Streaming token usage

For vLLM streaming, token usage is not always emitted unless requested.

Mellomting should support a backend option:

```yaml
accounting:
  ensure_stream_usage: true
```

For known OpenAI-compatible/vLLM Chat/Completions requests, Mellomting MAY internally ensure:

```json
"stream_options": {
  "include_usage": true
}
```

while preserving any other existing stream options.

If the client did not request a usage chunk, Mellomting should, where safely identifiable, consume the synthetic final usage-only chunk for accounting and avoid exposing a semantic change to the client.

This behaviour requires extensive compatibility tests.

If exact streaming usage cannot be obtained:

- complete the inference normally;
- mark accounting as `partial` or `unknown`;
- never invent exact token counts.

Current vLLM also supports forcing usage server-side; document that as an operational alternative.

---

# 39. Token quota semantics

Support simple fixed UTC token windows:

```yaml
tokens_per_hour: 500000
tokens_per_day: 5000000
```

Fixed windows are preferred over a complex rolling database in v1 because they are:

- deterministic;
- cheap;
- easy to replay from JSONL;
- easy to audit.

At admission:

- check already-settled token usage;
- optionally reserve configured/requested output capacity;
- reject if clearly over quota.

At completion:

- settle against actual backend-reported usage.

If a stream terminates before usage is known:

- record `usage_status: unknown`;
- conservatively charge a configured reservation for quota purposes;
- do not write a fake `exact` token count.

Token quotas are an abuse-control mechanism, not billing-grade accounting.

---

# 40. Persistent quota state

A daemon restart SHOULD NOT trivially reset daily/hourly token quotas.

On startup:

- replay accounting records covering the longest active quota window;
- reconstruct current per-key token buckets/windows.

Configuration:

```yaml
accounting:
  replay_on_start: true
  replay_max_bytes: 1073741824
```

If quota enforcement is configured as strict and the log cannot be replayed sufficiently, startup should fail rather than silently reset quotas.

Optimise this only if real deployments show startup cost is excessive.

---

# 41. Accounting log

Default:

```text
/var/log/mellomting/usage.jsonl
```

Example:

```json
{
  "time": "2026-08-19T15:14:12.123Z",
  "request_id": "req_01K...",
  "key_id": "4f92c16a0b7de831",
  "model": "qwen-coder",
  "backend": "qwen-a",
  "endpoint": "chat.completions",
  "status": 200,
  "duration_ms": 18542,
  "input_tokens": 18422,
  "output_tokens": 921,
  "total_tokens": 19343,
  "usage_status": "exact",
  "retries": 0,
  "charged_tokens": 19343
}
```

Do not include:

```text
prompt
response
tool arguments
tool results
Authorization header
backend credentials
raw backend error body
```

Accounting is **not** a tamper-evident audit log. Do not claim otherwise.

---

# 42. Accounting writer

Use one bounded writer queue.

Inference must never allocate an unbounded backlog because a filesystem is slow.

Example:

```yaml
accounting:
  queue_size: 4096
  overflow: drop-and-alert
  fsync: interval
  fsync_interval: 5s
```

Token-limit state should be updated independently of the JSONL writer so an accounting-disk stall does not disable live quota enforcement.

When records are dropped:

- increment an internal counter;
- emit a rate-limited operational error;
- expose it through future metrics if metrics are implemented.

The writer opens the JSONL once at startup and holds the file descriptor
forever; there is no in-process reopen path in v1 (FIX-11/N7). Operators
must therefore rotate with `copytruncate`, not with rename: a renamed file
is a new inode that the Landlock policy (PLAN §57) does not grant, so the
daemon would keep appending to the rotated file until restart. The shipped
policy is `deploy/mellomting.logrotate`; it keeps the inode unchanged, so
it works under `security.landlock.mode: required` without any reopen.

---

# 43. Operational logging

Operational logs go to stdout/stderr by default.

Use structured JSON.

Fields may include:

```text
timestamp
level
request_id
remote_ip
key_id
endpoint
public_model
backend
status
duration_ms
bytes_in
bytes_out
retry_count
error_class
qualifier_action
```

Do not log raw errors from an upstream if they can contain response bodies or secrets.

Map backend failures into sanitized error classes such as:

```text
backend_connect
backend_timeout
backend_429
backend_5xx
backend_stream_error
```

---

# 44. Usage CLI

Initial command:

```text
mellomting usage report --since 24h
mellomting usage report --key 4f92c16a0b7de831 --since 7d
mellomting usage report --model qwen-coder --since 24h
mellomting usage report --backend auto-local-a-xxxx --since 24h
```

Example:

```text
KEY               REQUESTS   INPUT       OUTPUT      TOTAL
4f92c16a0b7de831  423        8,424,331   923,122     9,347,453
a91q2c0b7de83111  118        1,842,920   310,221     2,153,141
```

Scan JSONL initially.

Do not introduce SQLite unless profiling demonstrates that JSONL has become a real operational problem.

---

# 45. Optional qualifier / guard architecture

Mellomting should support an optional **qualifier**: a separate model that classifies a request and/or response.

The qualifier is not an agent and does not execute tools.

Use cases include:

- general safety classification;
- prompt-injection detection;
- secret leakage checks;
- policy classification;
- data-loss prevention;
- organisation-specific request policy.

The qualifier MUST be optional and disabled by default.

---

# 46. Qualifier configuration

Example:

```yaml
qualifiers:
  default-safety:
    backend: guard-a
    model: Qwen/Qwen3Guard-Gen-0.6B

    timeout: 3s
    failure_policy: allow

    input:
      mode: audit

    output:
      mode: disabled

models:
  qwen-coder:
    qualifier: default-safety
```

Modes:

```text
disabled
audit
block
```

Failure policies:

```text
allow
audit
block
```

Never silently choose the failure policy.

---

# 47. Qualifier security boundary

Qualifier traffic contains sensitive prompt material.

Therefore:

- local qualifier backends are the default;
- a remote qualifier backend MUST require explicit `allow_remote_content: true`;
- emit a startup warning when prompt data can leave the host;
- qualifier credentials are treated like backend credentials;
- qualifier requests use an internal client path, not public Mellomting ingress;
- qualifier calls MUST NOT recursively trigger qualification;
- qualifier capacity has separate concurrency and queue limits.

---

# 48. Qualifier input representation

Do not blindly send the entire raw HTTP request.

Build a stable internal document containing the semantic material required for classification.

Conceptual form:

```json
{
  "endpoint": "chat.completions",
  "public_model": "qwen-coder",
  "messages": [...],
  "input": ...,
  "tools_present": true
}
```

Avoid sending unnecessary authentication, routing, or network metadata.

Multimodal handling must be explicit:

```text
unsupported multimodal input -> configured allow/audit/block behaviour
```

Do not fetch image URLs for a qualifier.

---

# 49. Qualifier result

Normalize all guard models into:

```go
type Verdict struct {
    Action     Action
    Categories []string
    Severity   string
    Confidence *float64
    Reason     string
}
```

Internal actions:

```text
allow
audit
block
```

The adapter SHOULD request structured JSON where the guard model supports it.

Malformed classifier output follows `failure_policy`.

Do not let arbitrary model text directly become an HTTP error or log entry without sanitization.

---

# 50. Recommended qualifier rollout

For coding/security workloads, generic guard models can produce false positives on:

- exploit examples;
- reverse engineering;
- malware analysis;
- security testing;
- shell commands;
- code involving credentials or authentication.

Therefore the default recommended rollout is:

```text
input = audit
output = disabled
```

Collect category statistics before enabling blocking.

A safety classifier should not be treated as a mathematically reliable security policy engine.

---

# 51. Candidate guard models

A good initial candidate is Qwen3Guard-Gen.

The Qwen project provides generative guard models at several sizes, including a small 0.6B model, which is attractive for a local secondary classifier.

Qwen3Guard-Stream is specifically designed for token-level streaming classification, but streaming integration should be treated as a separate later project and tested against the chosen serving stack rather than assumed to work as an ordinary vLLM OpenAI endpoint.

Other guard models MAY be supported through adapters later.

Mellomting's internal qualifier interface must remain model-agnostic.

---

# 52. Output qualification

Output qualification has unavoidable latency/streaming trade-offs.

## 52.1 Non-streaming output block

Supported after the qualifier framework is stable:

```text
backend -> buffer bounded response -> qualifier -> client
```

If blocked:

- discard model output;
- return policy error;
- still account for model tokens consumed.

## 52.2 Streaming audit

MAY inspect streamed output for audit purposes without delaying client delivery.

This does not provide prevention.

## 52.3 Streaming block

Do not claim secure streaming blocking in v1.

Any approach that sends content before classification has a leakage window.

A later streaming-guard implementation must explicitly document:

- buffered-token window;
- maximum leakage before block;
- classifier cadence;
- failure behaviour;
- memory bounds.

---

# 53. Landlock purpose

Landlock is a **post-start zero-day containment mechanism**.

The intention is:

> If an attacker obtains arbitrary code execution inside Mellomting after startup, the compromised process should have dramatically fewer filesystem, network, and IPC capabilities than the service account would normally possess.

It is not just a file-permission helper.

---

# 54. Landlock library

Use:

```text
github.com/landlock-lsm/go-landlock
```

unless a future review finds a materially better maintained option.

At this plan's research baseline, go-landlock exposes V9 support, network restriction, IPC scoping, and all-goroutine restriction support.

Pin the dependency to a reviewed release.

---

# 55. Landlock ABI policy

Default configuration:

```yaml
security:
  landlock:
    mode: required
    minimum_abi: 6
```

Modes:

```text
required
best-effort
disabled
```

Production documentation should recommend:

```text
required
```

Why minimum ABI 6:

- the service needs filesystem read/write controls (ABI 1+), which cover
  the users file read and the accounting append;
- TCP network restriction is available from ABI 4;
- IPC scoping is available from ABI 6;
- ABI 8+ adds the all-thread TSYNC path, which is preferred when the
  kernel offers it (a Go service spawns threads at runtime); below ABI 8
  the pinned go-landlock all-thread prctl/restrict-self sequence is
  still applied, so ABI 6 kernels are fully confined, not silently
  degraded.

Implementation:

1. query kernel Landlock ABI;
2. compare with configured minimum;
3. choose the highest ABI supported by both kernel and Mellomting's pinned library;
4. in `required` mode, fail if the minimum cannot be enforced;
5. do not call a broad `BestEffort()` path that can degrade all the way to no protection when the operator asked for `required`.

Future library support for newer ABIs should be reviewed deliberately.

---

# 56. All-thread enforcement

This deserves an explicit requirement.

Go may have multiple OS threads before application startup is complete.

Applying a ruleset only to the current thread is insufficient.

Mellomting MUST use go-landlock's all-goroutine/thread-synchronised enforcement path and MUST test it.

Acceptance test:

- create several goroutines pinned/active across OS threads;
- apply sandbox;
- have each attempt a forbidden operation;
- every attempt must fail.

If this cannot be guaranteed on a platform, `landlock.mode=required` must refuse startup.

---

# 57. Landlock startup sequence

Strict sequence:

```text
 1. set restrictive umask
 2. parse CLI
 3. read config
 4. validate config completely
 5. read users database
 6. read auth pepper
 7. read backend credentials
 8. load system CA pool if remote TLS backends are configured
 9. load static TLS cert/key if native TLS is configured
10. initialize accounting state and replay quota windows
11. open accounting output
12. construct backend HTTP transports
13. create the inbound listener
14. perform optional startup backend checks
15. close every no-longer-needed config/secret FD
16. construct Landlock policy
17. set no_new_privs / apply Landlock to all runtime threads
18. verify effective mode
19. only now begin Accept()/Serve()
```

No client request may be processed before step 18 succeeds.

---

# 58. Landlock filesystem policy

The policy should be generated from actual runtime needs.

After startup, the ideal daemon needs almost no filesystem access.

Example policy:

```text
READ:
    /etc/mellomting/users.yaml       only because SIGHUP reload needs it
    /etc/mellomting/                 files in it, because every key mutation
                                     renames a new users.yaml over the old
                                     one and a rule bound to the replaced
                                     inode would deny the reload (§30)

WRITE:
    accounting destination only if opened/reopened by pathname

EXECUTE:
    nowhere

DENY:
    everything else
```

Prefer preloading:

```text
config.yaml
auth.pepper
backend credentials
TLS keys
CA roots
```

and then denying path access to them.

The startup file descriptors for those secrets MUST be closed before sandbox activation.

---

# 59. Open-file caveat

Landlock filesystem checks generally happen when resources are opened/resolved.

Already-open file descriptors can retain capabilities after confinement.

Therefore:

- keep only intentionally required descriptors;
- close config FDs;
- close secret FDs;
- close temporary files;
- close test/probe sockets;
- do not leave a directory FD open unless required.

The intentional long-lived descriptors are normally:

```text
listener
accounting file
stdout/stderr
backend sockets created later under network policy
```

Review the process FD table in integration tests where practical.

---

# 60. Landlock TCP network policy

For the standard local backend configuration:

```text
allow CONNECT_TCP 8001
allow CONNECT_TCP 8002
allow CONNECT_TCP 8100   # only if qualifier is enabled
```

Do not grant new TCP bind rights after startup; the listening socket already exists.

This means a compromised process should not be able to open a second listening TCP service.

Important limitation:

> Landlock TCP rules are based on **port**, not destination IP.

Allowing TCP destination port 8001 does not mean only `127.0.0.1:8001`.

External address restrictions are therefore still required.

---

# 61. Multipath TCP

Current Go versions may use MPTCP for listeners, while Landlock TCP restrictions apply to classic TCP semantics in ways that make relying on implicit Go defaults unsafe.

Mellomting MUST explicitly disable MPTCP on every listener/dialer it owns:

```go
var lc net.ListenConfig
lc.SetMultipathTCP(false)
```

and:

```go
var d net.Dialer
d.SetMultipathTCP(false)
```

Do this regardless of the current Go default.

Add regression tests.

Do not rely only on:

```text
GODEBUG=multipathtcp=0
```

because security-critical behaviour should be explicit in code.

---

# 62. Landlock IPC scoping

When supported by the effective ABI/library, enable scoped restrictions for:

- signalling processes outside the Landlock domain;
- abstract Unix-domain sockets outside the domain.

Validate that normal Go runtime shutdown and service operation still work.

Do not disable IPC scoping merely because an integration test was written incorrectly.

---

# 63. UDP and DNS

At the research baseline, go-landlock V9 does not provide the newer UDP restrictions available in later kernel ABIs.

The standard Mellomting deployment should therefore avoid requiring UDP at all:

- use literal loopback backend IPs;
- do not perform runtime DNS for local backends;
- use external service/network policy to restrict unexpected network access.

If future go-landlock versions expose UDP restrictions, evaluate and add them.

Remote DNS/provider mode necessarily has a wider sandbox and should be documented as such.

---

# 64. systemd hardening

Landlock should be complemented by systemd restrictions.

Ship a hardened unit template similar to:

```ini
[Service]
User=mellomting
Group=mellomting

RuntimeDirectory=mellomting
StateDirectory=mellomting
LogsDirectory=mellomting

UMask=0077

NoNewPrivileges=yes

CapabilityBoundingSet=
AmbientCapabilities=

ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes

ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes

RestrictSUIDSGID=yes
RestrictRealtime=yes
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes

RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6

IPAddressDeny=any
IPAddressAllow=localhost

ReadOnlyPaths=/etc/mellomting
ReadWritePaths=/var/log/mellomting /var/lib/mellomting
```

Test each directive on supported distributions.

If a directive must be removed, document exactly why.

---

# 65. Additional zero-day hardening

Landlock does not cover all syscalls.

After the core product is stable, investigate an internal post-start seccomp policy or a carefully designed service-level equivalent to prohibit operations Mellomting never needs, particularly:

```text
execve / execveat
ptrace
mount-family operations
bpf
perf_event_open
module-loading operations
namespace creation
```

Do not rush into a fragile giant syscall allow-list.

A small deny-list of clearly unnecessary dangerous operations may be more maintainable.

This is a defence-in-depth phase, not an MVP blocker.

---

# 66. vLLM backend hardening

Mellomting's HTTP allow-list protects clients from reaching vLLM administration endpoints through the proxy.

It does **not** stop a fully compromised Mellomting process from speaking directly to an allowed backend port.

Therefore deployment documentation MUST recommend:

- keep vLLM bound to loopback/private namespace;
- run it as a different unprivileged user;
- do not enable dynamic LoRA loading on production inference listeners;
- do not enable development/profiling functionality unless needed;
- use backend API auth where it materially reduces other-local-user access;
- apply separate systemd/container hardening to vLLM.

Mellomting should not promise to sandbox vLLM itself.

---

# 67. Native TLS

Support static TLS earlier than ACME.

Static TLS is enabled by the presence of its two file fields; there is no
`mode` field:

```yaml
server:
  listen:
    network: tcp
    address: :8443
  tls:
    cert_file: /etc/mellomting/tls/cert.pem
    key_file: /etc/mellomting/tls/key.pem
```

Both fields are required together; one without the other is invalid. An empty
TLS block enables nothing. `mode` and ACME-shaped source fields are rejected
as unknown. TLS remains invalid on a Unix socket.

Load cert/key before Landlock.

Certificate reload can require process restart in v1.

---

# 68. Optional ACME

> **Status: shelved for the first release.** The v1 source schema has no
> `tls.mode` key (static TLS is `cert_file`/`key_file` alone), so an
> `acme` configuration is refused by the strict decoder as an unknown
> field at `config check` time. That rejection is the whole mechanism:
> there is no ACME code and no separate runtime refusal. This design is
> retained for a later release, which re-adds the schema and the runtime
> together.

Native ACME is a convenience feature, not the recommended hardened deployment.

If implemented, use Caddy's `certmagic` library rather than embedding the complete Caddy server.

Example:

```yaml
server:
  tls:
    mode: acme
    hostname: llm.example.net
    email: admin@example.net
```

ACME complicates confinement because renewal requires:

- persistent writable state;
- outbound CA connectivity;
- possibly ports 80/443.

In particular, permitting arbitrary TCP port 443 in Landlock is not a useful destination restriction by itself.

Therefore:

- make ACME optional;
- document the wider sandbox;
- recommend external Caddy/nginx for the strongest profile.

---

# 69. Health endpoints

Expose only:

```text
GET /healthz
GET /readyz
```

Responses should reveal almost nothing.

Example:

```text
200 ok
```

`/healthz`:

```text
process/event loop alive
```

`/readyz`:

```text
configuration loaded
sandbox applied
server accepting traffic
```

Do not return:

- backend URLs;
- API-key names;
- model credentials;
- filesystem paths;
- verbose failure stacks.

Detailed backend status belongs in logs, not a public endpoint.

---

# 70. Optional backend health

Phase 2 may add passive health:

- mark recent failing backend temporarily unavailable;
- short cooldown;
- exponential recovery;
- bounded state.

Optional active checks may use a configured trusted path such as vLLM `/health`, but this is an internal request, never a public proxy route.

Do not add complex service discovery.

---

# 71. CORS

CORS disabled by default.

If explicitly enabled:

```yaml
server:
  cors:
    allowed_origins:
      - https://specific.example
```

Do not default to:

```text
*
```

Coding agents do not need browser CORS.

---

# 72. Error format

Return OpenAI-like errors where practical:

```json
{
  "error": {
    "message": "rate limit exceeded",
    "type": "rate_limit_error",
    "param": null,
    "code": "rate_limit_exceeded"
  }
}
```

Never return:

- Go stack traces;
- backend hostnames unless explicitly safe;
- filesystem paths;
- secret names;
- raw backend bodies;
- raw qualifier prompts.

---

# 73. Request IDs

Generate an internal request ID for every accepted request.

Example:

```text
req_01K2...
```

Return:

```text
X-Request-ID
```

If accepting a client-provided request ID at all:

- validate charset;
- bound length;
- log it separately from the internal ID.

Do not make security decisions using client request IDs.

---

# 74. Graceful shutdown

On SIGTERM/SIGINT:

1. stop accepting new connections;
2. reject new inference admissions;
3. let active streams finish up to a configured deadline;
4. cancel remaining upstream contexts;
5. settle available accounting;
6. flush accounting writer;
7. close listeners/files;
8. exit.

Example:

```yaml
shutdown:
  grace_period: 30s
```

---

# 75. Bifrost features intentionally adopted

Mellomting should borrow the useful gateway ideas, not the entire product.

| Bifrost-like concept | Mellomting implementation |
|---|---|
| Virtual keys | File-backed opaque API keys |
| Model restrictions | Per-key public-model ACL |
| Request limits | Per-key/global token bucket |
| Token limits | Per-key fixed UTC token windows |
| Weighted routing | Weighted backend strategies |
| Retry/backoff | Small bounded retry policy |
| Fallbacks | Backend group failover |
| Provider exclusion under limit/failure | Backend eligibility state |
| Token observability | JSONL accounting |
| Per-request latency/status | Structured operational logs |
| Guardrails | Optional local qualifier interface |
| Session/routing affinity | Responses affinity; optional sticky routing later |

Features deliberately not copied:

- dashboard;
- SQL control plane;
- dynamic plugins;
- provider governance hierarchy;
- cost/budget database;
- arbitrary external content fetch;
- enterprise policy language;
- massive provider matrix in v1.

---

# 76. Suggested configuration

A representative hardened local setup:

```yaml
version: 1

server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: "0660"

  max_header_bytes: 32768
  max_body_bytes: 16777216
  max_response_bytes: 67108864
  max_inflight_requests: 64
  max_buffered_request_bytes: 67108864

  read_header_timeout: 5s
  read_body_timeout: 60s
  idle_timeout: 120s
  stream_idle_timeout: 120s
  stream_write_timeout: 30s

auth:
  users_file: /etc/mellomting/users.yaml
  pepper_file: /etc/mellomting/auth.pepper

security:
  backend_network:
    mode: loopback-only

  landlock:
    mode: required
    minimum_abi: 6

logging:
  format: json
  level: info

accounting:
  enabled: true
  path: /var/log/mellomting/usage.jsonl
  ensure_stream_usage: true
  replay_on_start: true
  queue_size: 4096
  overflow: drop-and-alert
  fsync: interval
  fsync_interval: 5s

limits:
  global_requests_per_second: 100
  global_burst: 200

servers:
  local-a:
    url: http://127.0.0.1:8001
  local-b:
    url: http://127.0.0.1:8002

models:
  qwen-coder:
    type: generation
    strategy: least-inflight

    policy:
      max_output_tokens: 32768

    servers:
      - local-a
      - local-b
```

Qualifiers remain shelved for the first release (§96). The development-era
`backends:` source form and `qualifiers:` are rejected as unknown fields; the
`qualifiers:` block shown below is retained only as a forward-compatible
design sketch, not an accepted v1 source field:

```yaml
# Not accepted in v1 — forward-compatible design sketch only.
qualifiers:
  safety-audit:
    backend: guard-a
    model: Qwen/Qwen3Guard-Gen-0.6B
```

---

# 77. Example users file

```yaml
version: 1

keys:
  - id: 4f92c16a0b7de831
    name: codex
    secret_hash: "hmac-sha256:..."

    enabled: true
    expires_at: null

    models:
      - qwen-coder

    limits:
      requests_per_second: 4
      burst: 8
      concurrent_requests: 4
      tokens_per_hour: 500000
      tokens_per_day: 5000000

  - id: b8170d34ac290fe6
    name: opencode
    secret_hash: "hmac-sha256:..."

    enabled: true
    expires_at: "2026-12-31T23:59:59Z"

    models:
      - "*"

    limits:
      requests_per_second: 8
      burst: 16
      concurrent_requests: 8
```

Wildcard model permission should be explicit and easy to spot in review.

---

# 77a. Operator interface, initialization, and key contracts

This section fixes the operator-facing interface in RFC 2119 terms. It is the
normative contract for `init`, `install`, configuration lookup, the source
schema, discovery, and client API keys. The UX plan
(`docs/UX_SIMPLIFICATION_PLAN.md`) is the execution ledger; this section is
authoritative.

## 77a.1 Configuration lookup (D1)

Every config-dependent CLI command MUST use one resolver:

1. an explicit `--config PATH`, when supplied;
2. `/etc/mellomting/config.yaml` otherwise.

The working directory MUST NOT be consulted. These commands are routinely run
as root, and an implicit `./config.yaml` would let anyone who can write to a
directory root happens to run from supply the configuration — and with it
`auth.users_file`, `auth.pepper_file`, the backends, and the sandbox mode. A
local file is used by naming it: `--config ./config.yaml`.

`auth.users_file` and `auth.pepper_file` MUST be absolute paths, so the auth
files a privileged `key` command reads and rewrites never depend on the
working directory.

`mellomting init` differs because it creates a file: its default destination
is an absolute path formed from the current working directory and
`config.yaml`, and it never falls back to `/etc`.

The generated config MUST contain absolute users-file and pepper-file paths.
There is no root-level global flag: `mellomting --config X serve` is invalid.
The accepted shape is `mellomting <command> [subcommand] --config PATH`.

## 77a.2 Initialization transaction (D3, D4, D5)

`mellomting init` MUST require at least one `--server`; bare `init` is a usage
error and MUST NOT create an incomplete scaffold. Systemd installation retains
its separate commented scaffold behavior.

For a local non-system installation, config, users, and pepper MUST share one
parent directory. The resolved config destination is the completion marker and
is published only after both auth entries are durably synchronized. The
initialization order is fixed: resolve absolute paths; validate arguments,
platform policy, every destination, and the destination directory's ownership
and mode; complete all server discovery; render config and users bytes in
memory and generate pepper bytes; validate the in-memory representation;
create three mode-0600 temporary files in the destination directory; write and
fsync each; publish pepper and users using an atomic create-only hard link from
each temporary file to its final name, then unlink the temporary names; fsync
the parent directory; publish the config destination with the same create-only
link and unlink its temporary name; fsync the parent directory again.

`os.Rename` MUST NOT be used to publish these files. Publication uses a
same-directory, directory-FD-relative `linkat(temp, final)` that fails when
the final path exists. The destination directory MUST be owned by the
effective user and MUST NOT be group- or world-writable. Init MUST refuse if
any destination already exists, reject symlinks and other non-regular
destination conditions, and clean up only files created by the failed
invocation and only when their identity can be established safely. Crash
recovery is fail-closed: remaining create-only files are reported for manual
inspection and are never overwritten on a re-run. This is fail-closed, not
fully transactional across crashes.

Local `init` writes config, users, and pepper as `0600`. Systemd provisioning
retains `0640 root:mellomting` for those files.

## 77a.3 Local sandbox behavior (D2)

`init` defaults to `security.landlock.mode: required` with the default minimum
ABI. Before writing, it runs the equivalent of the Landlock capability check:

- supported Linux host meeting the minimum: continue;
- unsupported/too-old host: fail without writing and show the explicit
  `--landlock best-effort` alternative;
- `--landlock best-effort` or `--landlock disabled`: accepted only when the
  operator supplied it explicitly, and written into config.

There is no platform-dependent silent downgrade.

## 77a.4 Discovery scope, limits, and response (D6, D7, D8, D9, D10)

Discovery MUST query only server URLs explicitly provided by the operator. It
MUST NOT scan the host, network, DNS, container runtime, or service manager,
and MUST NOT be performed during `serve`, config loading, reload, or any
background task. Server URL hosts MUST be literal IPv4 or IPv6 addresses with
explicit ports; an empty host is invalid and DNS names are not resolved.

Fixed initial limits: at most 16 servers per init; 256 models per server; 1,024
unique public models; 1 MiB response bytes per server; 256 bytes per model ID;
3-second connect timeout; 10-second response-header timeout; 15-second total
request timeout.

Discovery uses `GET <base_url>/v1/models` with MPTCP disabled, the derived
backend-network policy enforced on every connection, HTTP proxying disabled
regardless of environment, redirects disabled, response compression disabled,
no retries, no client Authorization/Proxy-Authorization/X-Api-Key/forwarding/
proxy-identity headers, `Accept: application/json`, and no request body. Only
HTTP 200 is accepted. A present `Content-Type` is parsed with
`mime.ParseMediaType` and must be `application/json` (parameters allowed); a
truly absent header is accepted. Raw response bodies are never included in
errors or logs.

The response MUST be decoded as a bounded subset: top-level `object == "list"`,
non-null `data`, each entry `object == "model"`, and each `id` valid UTF-8,
non-empty, at most 256 bytes, with no Unicode control/format character, no
Unicode line/paragraph separator, and no leading/trailing Unicode whitespace,
with no duplicate ID within one server. An `id` MUST NOT be `*` or contain a
comma: `*` is the model-ACL wildcard and a comma separates entries in
`key create --models`, so a discovered ID that is either would let the queried
server name a model the authorization language reads as a sentinel. Capabilities are not inferred from
other response fields; every discovered model defaults to `type: generation`.

Accepted server syntax is `--server URL` and `--server NAME=URL`. A sole
unnamed server is named `local`; multiple unnamed servers are named `local-1`,
`local-2` in argument order. If any explicit name is used, all servers MUST
have explicit names. Explicit names match `^[a-z][a-z0-9-]{0,62}$`, are unique,
and URLs pass backend base-URL validation including a literal IP, explicit
port, and no userinfo/path/query/fragment. Canonical `(scheme, IP, port)`
destinations are unique after normalizing IP spellings and unmapping
IPv4-mapped IPv6 addresses.

If any server fails, `init` MUST fail before writing; there is no
`--allow-partial`. `init` is non-interactive by default. `--dry-run` performs
all validation and discovery, writes the exact config YAML and nothing else to
stdout, writes bounded discovery context to stderr, and writes no files.

## 77a.5 Source configuration schema (D11)

The server-oriented form is the sole accepted source schema; the
development-era `backends:` form is removed rather than migrated or accepted in
parallel. Configuration keeps `version: 1`.

```yaml
version: 1

server:
  listen:
    network: tcp
    address: 127.0.0.1:8080

auth:
  users_file: /absolute/path/users.yaml
  pepper_file: /absolute/path/auth.pepper

servers:
  local-a:
    url: http://127.0.0.1:8000
  local-b:
    url: http://127.0.0.1:8010

models:
  qwen3.8-27b:
    servers: [local-a, local-b]
  qwen-coder:
    servers: [local-b]
```

Server fields are `url` (required) and `api_key_file` (optional). Model fields
are `type` (optional, default generation), `servers` (required), `upstream_model`
(optional, default public map key), `strategy` (optional; single when one
server, least-inflight when several), and `policy` (optional). `backends:` and
`qualifiers:` are rejected as unknown source fields.

Static TLS uses the presence of its two file fields with no `mode` field; both
`cert_file` and `key_file` are required together, an empty TLS block enables
nothing, and `mode`/`acme` source fields are rejected. Native ACME remains
shelved.

## 77a.6 Deterministic normalization (D12)

Normalize source form before applying defaults and validation. For each compact
public model in UTF-8 bytewise sorted public-model order and each referenced
server in YAML order: verify the server exists and is not repeated; compute
`sha256(publicModel + "\x00" + serverName)`; create the internal backend name
`"auto-" + serverName + "-" + first 12 lowercase hex characters of the digest`;
reject the impossible generated-name collision; copy server URL and API-key
path into the backend; set `upstream_model` to the public/discovered model ID;
create a backend reference with weight 1. The normalized model copies `type`,
`strategy`, and `policy`; empty type defaults to `generation`, empty strategy
defaults to `single` for one server, otherwise `least-inflight`.

The resulting runtime `Config.Backends` and `Config.Models` use the existing
types and contain no source-only state. `config show-effective` emits a fully
defaulted server-oriented version-1 document that can be fed back into
`config check`; synthetic internal backend names never appear in
operator-facing configuration.

## 77a.7 Key creation and API-key format (D14, D18)

When `--models` is absent: exactly one configured public model is inferred;
zero models fails; two or more models fails and lists only the model names,
sorted, with a request to pass `--models`; wildcard access is never inferred.
Configuration validation rejects a public model named `*` or containing a
comma, so a configured name is always a plain model. In `--models`, `*` is
accepted only on its own: mixed with named models it is a usage error rather
than a list that collapses to the wildcard.

Every generated and accepted client API key MUST have exactly the form
`sk-<username>-<keyid>-<secret>` with `username` matching
`^[a-z][a-z0-9_]{0,31}$`, `keyid` 8 random bytes as 16 lowercase hex characters,
`secret` 32 random bytes (256 bits) as 64 lowercase hex characters, no empty
segment, and no additional separator or suffix. Only this grammar is accepted;
oversized inputs are rejected before segment parsing. The username equals
`key create --name` and is stored as the key's human-visible name.

On success, `key create` stdout contains the raw key and a trailing newline
only; all human context goes to stderr. The raw key is printed exactly once.

## 77a.8 Installation command (D15, D16)

`mellomting install [--systemd]` is the sole installation command shape; a
top-level `--install` alias MUST NOT exist. For an existing systemd config the
installer MUST NOT require it to validate (a previously installed scaffold is
deliberately invalid until edited). If the regular, non-symlink config
explicitly names a fixed default auth path, create that artifact only when it
is missing; otherwise never create custom auth files, parent directories, or
unrelated default auth files. Existing config and auth files are never
rewritten. Referenced default auth paths are determined with a bounded,
no-side-effect YAML source parse; a parse failure creates no auth files. For a
newly created default scaffold, generate the fixed default pepper and empty
users file. The installer MUST NOT start a deliberately invalid configuration
automatically.

Routine success output follows the human-output policy: one completion line
and at most three next actions, with no artifact/permission inventory and no
design rationale.

## 77a.9 Listener syntax (D17)

`init --listen` defaults to `127.0.0.1:8080`. A value beginning with `/` is a
Unix socket and must be an absolute, cleaned path; every other value is parsed
with `net.SplitHostPort`; the host is empty or a literal IP address; the port
is decimal in 1..65535. Empty TCP hosts are accepted as a wildcard bind subject
to the same TLS-or-explicit-plaintext policy as `0.0.0.0` and `[::]`. Relative
paths, hostnames, URL schemes, bare ports, zone-scoped IPv6, and unbracketed
IPv6 are rejected. For Unix, generate `network: unix`, the path, and the
default socket mode; for TCP generate `network: tcp` and the address. If the
TCP host is empty, unspecified, or non-loopback and static TLS is absent, also
generate `allow_plaintext_nonloopback: true`.

## 77a.10 Security invariants

- Discovery never connects outside the policy derived from explicit servers.
- Every discovery connection has MPTCP disabled.
- Redirects cannot escape the validated destination.
- No client credential or proxy identity header reaches a server.
- Init accepts no backend credentials and never invents authentication policy.
- Raw discovery error bodies are never logged or returned.
- All discovery inputs and outputs are bounded.
- Discovery completes before filesystem mutation.
- Init never overwrites an existing final path and never follows a
  final-component symlink.
- New local config, users, and pepper files are mode 0600.
- Pepper comes from `crypto/rand` with 64 random bytes before base64.
- The empty users file authenticates nobody.
- Server-oriented source config is normalized before the strict internal
  validation boundary.
- Development-era `backends:` source config is rejected.
- Runtime never discovers, adds, removes, or remaps models.
- Wildcard key authorization is never inferred.
- Raw API keys and peppers are never logged.
- A username parsed from a presented credential is never logged.
- API-key usernames and both hexadecimal segments satisfy the D18 grammar.
- Only the D18 key format is accepted.

---

# 78. Suggested package layout

```text
cmd/
  mellomting/
    main.go
    config_path.go
    init.go
    install.go
    serve.go

integration/
  ux_journey_test.go

internal/
  accounting/
    record.go
    writer.go
    replay.go
    usage.go

  auth/
    key.go
    store.go
    hmac.go
    reload.go

  backend/
    backend.go
    client.go
    health.go

  config/
    config.go
    load.go
    validate.go
    source.go

  discovery/
    discovery.go

  httpapi/
    server.go
    errors.go
    headers.go
    models.go
    responses_affinity.go

  landlock/
    landlock_linux.go
    landlock_other.go
    policy.go

  limiter/
    requests.go
    concurrency.go
    tokens.go
    memory.go

  logging/
    logging.go

  proxy/
    request.go
    response.go
    sse.go
    usage_parser.go

  routing/
    router.go
    roundrobin.go
    leastinflight.go

  securefile/
    open_linux.go
    atomic.go

  tlsconfig/
    tls.go

  version/
    version.go
```

Do not create public `pkg/` packages until there is an actual external API to support.

---

# 79. Small internal interfaces

Keep interfaces narrow.

```go
type Authenticator interface {
    Authenticate(token string) (*Principal, error)
}

type Router interface {
    Select(ctx context.Context, model string, hint RouteHint) (*Backend, error)
}

type Qualifier interface {
    QualifyInput(ctx context.Context, doc InputDocument) (Verdict, error)
    QualifyOutput(ctx context.Context, doc OutputDocument) (Verdict, error)
}

type UsageSink interface {
    Record(UsageRecord) bool
}
```

Avoid interface-heavy architecture for internal code that has only one implementation.

---

# 80. Security-sensitive parser tests

Mandatory fuzz/unit targets:

```text
Authorization parser
API-key parser
YAML config parser
YAML users parser
public model extraction
previous_response_id extraction
max-token field extraction
backend URL validation
header filtering
SSE framing
streaming usage extraction
Responses response-ID extraction
qualifier structured verdict parser
OpenAI-style error rewriting
```

Fuzz with arbitrary bytes.

---

# 81. HTTP/security integration tests

Tests MUST verify at least:

- unauthenticated inference -> 401;
- malformed bearer token -> 401;
- unknown key -> 401;
- disabled key -> 401;
- expired key -> 401;
- valid key + unauthorized model -> hidden/restricted error;
- `/v1/models` is ACL-filtered;
- unknown paths never reach backend;
- backend admin paths never reach backend;
- client Authorization is not forwarded;
- client `x-api-key` is not forwarded;
- backend credential is injected only internally;
- oversized headers fail;
- oversized bodies fail before excessive allocation;
- compressed request body is rejected;
- global concurrency is bounded;
- per-key concurrency is bounded;
- backend queue is bounded;
- request RPS limits work;
- token quota survives daemon restart/replay;
- a disconnected client cancels upstream;
- a slow client cannot hold an unbounded writer forever;
- a partial stream is never retried;
- response-ID access cannot cross API keys;
- unknown response IDs are never sprayed across backends;
- raw prompts never appear in logs;
- API keys never appear in logs;
- backend secrets never appear in logs;
- raw backend error content never appears in logs.

---

# 82. Landlock integration tests

On a kernel supporting the required ABI, run a test process using Mellomting's real sandbox builder.

After confinement, verify failure when attempting:

```text
read /etc/shadow
read an unrelated user's home
read auth.pepper
read backend secret file
write /tmp
create arbitrary files
execute /bin/sh
bind a new TCP listening port
connect to a non-allowed TCP port
signal an unrelated process where scoped restrictions apply
connect to disallowed abstract Unix sockets
```

Also verify success for intentional operations:

```text
accept on pre-opened listener
connect to configured vLLM port
write accounting through intended mechanism
read users.yaml for SIGHUP reload
```

Run operations from multiple goroutines/OS threads after sandbox activation.

---

# 83. MPTCP regression test

The listener and backend dialer constructors MUST be tested to confirm they explicitly set:

```text
MultipathTCP(false)
```

Do not rely on the CI host lacking MPTCP.

A source-level/unit assertion around the constructors is acceptable in addition to integration testing.

---

# 84. Fake backend test harness

Build a small in-process fake OpenAI backend for tests.

It should simulate:

```text
normal JSON response
Chat SSE response
Responses SSE response
usage-only final chunk
slow headers
slow stream
client cancellation
429
500
502
503
malformed JSON
malformed SSE
huge SSE event
partial stream then disconnect
response ID creation
retrieve response
cancel response
backend auth verification
```

Use it for deterministic tests rather than depending only on real vLLM.

---

# 85. Real vLLM compatibility tests

Nightly/manual integration should test against current vLLM.

At minimum:

```text
/v1/models
/v1/chat/completions
Chat streaming
tool calls
structured output passthrough
/v1/completions
/v1/responses
Responses streaming
Responses previous_response_id
Responses retrieve
Responses cancel
/v1/embeddings
stream usage accounting
client cancellation
```

Record the tested vLLM version in `COMPATIBILITY.md`.

---

# 86. Coding-agent compatibility tests

Test real clients where practical:

- Codex-style Responses API client;
- OpenCode;
- Aider;
- generic official OpenAI SDK clients.

The desired client configuration is only:

```text
base URL
API key
public model name
```

No Mellomting-specific SDK should be required.

---

# 87. Performance expectations

Inference is slow compared with proxy operations, so Mellomting should add very little latency.

Benchmarks should cover:

```text
HMAC auth
model lookup
rate limiter
request JSON rewrite
backend routing
raw response forwarding
SSE parse+forward
usage extraction
accounting enqueue
```

Security limits must not be disabled for benchmarks.

Avoid expensive password hashes per request.

---

# 88. Dependency policy

Prefer the Go standard library.

Expected justified dependencies:

```text
gopkg.in/yaml.v3
github.com/landlock-lsm/go-landlock
golang.org/x/sys        if required for secure file helpers
```

Later, optional:

```text
github.com/caddyserver/certmagic
```

Every dependency should have a reason.

Do not import an entire framework for one helper.

---

# 89. Build and supply-chain security

CI should run:

```text
go test ./...
go test -race ./...
go vet ./...
staticcheck ./...
govulncheck ./...
```

Also:

- CodeQL;
- dependency review;
- fuzz tests;
- secret scanning;
- release SBOM;
- checksum generation;
- signed release artifacts;
- SHA-pinned GitHub Actions;
- Dependabot or Renovate.

Release artifacts:

```text
mellomting-linux-amd64
mellomting-linux-arm64
SHA256SUMS
SBOM.spdx.json
signatures
```

Use:

```text
-trimpath
```

and capture:

```text
version
git commit
build date if reproducibility policy permits
Go version
```

---

# 90. No hidden telemetry

The binary MUST NOT automatically:

- phone home;
- check for updates;
- send crash reports;
- send usage metrics;
- contact a project API.

`mellomting version` can display the installed version.

Operators can check releases themselves.

This is both a privacy and Landlock simplification.

---

# 91. Phase 0 — repository skeleton

Implement:

- module/repository;
- CLI skeleton;
- config loader;
- strict config validation;
- structured logging;
- signal framework;
- CI;
- `SECURITY.md`;
- `THREAT_MODEL.md`;
- `HARDENING.md`;
- version embedding.

Acceptance:

```text
mellomting version
mellomting config check
mellomting sandbox check
```

work.

No inference yet.

---

# 92. Phase 1 — secure single-backend proxy

Implement:

- Unix + loopback listener;
- API-key generation/storage;
- bearer auth;
- key ACL;
- `/v1/models`;
- `/v1/chat/completions`;
- `/v1/completions`;
- `/v1/responses`;
- Responses retrieve/cancel;
- `/v1/embeddings`;
- bounded JSON parsing;
- model rewrite;
- header sanitation;
- backend auth;
- streaming;
- request IDs;
- basic operational logs;
- no generic catch-all route.

Use only one backend per public model initially.

Acceptance:

A coding agent can use Mellomting against two different local vLLM processes by selecting different public model names.

---

# 93. Phase 2 — routing and resilience

Implement:

- multiple equivalent backends per public model;
- round-robin;
- least-inflight;
- weights;
- backend concurrency limits;
- bounded backend queues;
- conservative retries;
- fallbacks;
- passive health state;
- Responses affinity map.

Acceptance:

- pre-stream backend failure can safely move to another backend;
- post-stream backend failure is returned, never retried;
- stateful Responses requests stay on their owning backend.

---

# 94. Phase 3 — accounting and abuse controls

Implement:

- usage parser;
- stream usage acquisition;
- JSONL accounting;
- replay;
- usage CLI;
- RPS limiter;
- global limiter;
- concurrency limiter;
- token windows;
- generative output cap;
- conservative unknown-usage handling.

Acceptance:

```text
mellomting usage --key <id> --since 24h
```

matches known backend usage for normal completed requests.

Daily/hourly quotas remain in force after daemon restart.

---

# 95. Phase 4 — Landlock hardening

Implement:

- ABI detection;
- required/best-effort/disabled modes;
- minimum ABI enforcement;
- filesystem rules;
- TCP connect rules;
- IPC scopes;
- all-thread synchronization;
- explicit MPTCP disable;
- startup confinement sequence;
- sandbox self-check output;
- hardened systemd unit.

Acceptance:

All Landlock integration tests pass on the target GB10/kernel.

The daemon begins accepting requests only after the effective sandbox is confirmed.

---

# 96. Phase 5 — qualifier framework

> **Status: shelved for the first release.** The qualifier framework is
> deferred beyond v1. The design in §45–52 remains authoritative for a later
> release. The v1 source schema (D11) has no `qualifiers` key, so a qualifier
> configuration is refused by the strict decoder as an unknown field. That
> rejection is the whole control: there is no qualifier type in the runtime
> configuration, no separate startup check, and no request pipeline hook in
> v1. A later release re-adds the schema and the runtime together.

Implement:

- qualifier backend abstraction;
- bounded internal qualifier client;
- input-document extraction;
- structured verdict parser;
- audit mode;
- block mode;
- timeout;
- explicit fail policy;
- qualifier accounting metadata;
- local-only default.

Test first with a small Qwen3Guard-Gen deployment.

Do not implement streaming output block in this phase.

---

# 97. Phase 6 — TLS convenience

This phase ships static TLS only (PLAN §67). ACME is shelved for the first
release (PLAN §68).

Implement:

- static certificate/key mode;
- TLS tests;
- secure TLS defaults.

Shelved with ACME beyond the first release, to evaluate later:

- CertMagic ACME;
- persistent certificate storage;
- renewal under Landlock;
- additional egress required.

Reverse-proxy TLS remains the recommended hardened deployment.

---

# 98. Phase 7 — defence-in-depth review

Before a stable `1.0`:

- manual threat-model review;
- independent code review if available;
- fuzz corpus review;
- dependency review;
- host-hardening test;
- Landlock bypass review;
- file-descriptor review;
- log-secret audit;
- request-smuggling tests;
- slowloris tests;
- load tests;
- response-affinity abuse tests;
- qualifier bypass/failure tests (only if a qualifier is enabled; shelved
  for v1, see §96).

Evaluate a small post-start seccomp deny policy.

---

# 99. Definition of MVP

The MVP is complete when Mellomting provides:

```text
single Go binary
YAML config
YAML users/key DB
opaque bearer keys
per-key model ACL
OpenAI-compatible core inference routes
multiple public models
multiple local vLLM endpoints
correct SSE streaming
bounded HTTP resources
request/concurrency rate limiting
token accounting per key
token quotas
JSONL accounting
Landlock post-start confinement
hardened systemd unit
```

The qualifier can arrive immediately after MVP if necessary, but its interface should be anticipated in the request pipeline.

Decision for the first release: the qualifier is shelved (§96). Config stays
forward-compatible, and `serve` fails closed when a qualifier is configured.

---

# 100. Definition of "secure enough for normal deployment"

Do not call Mellomting production-ready merely because requests proxy successfully.

A stable release should satisfy all of the following:

- no known authentication bypass;
- no public admin API;
- all request paths allow-listed;
- no credential forwarding bug;
- no content logging by default;
- key DB contains no plaintext secrets;
- strict file-permission checks;
- bounded memory and queues;
- client disconnect cancellation;
- safe retry semantics;
- token/accounting tests;
- stateful Responses routing tests;
- Landlock applied to all threads;
- MPTCP explicitly disabled;
- target host passes Landlock self-tests;
- systemd hardening documented;
- dependency/security scans clean or exceptions documented;
- real coding-agent integration tests pass.

---

# 101. Future ideas — only if justified

Possible later work:

- Anthropic Messages pass-through;
- Prometheus metrics on a separate local/Unix listener;
- optional mTLS;
- optional OIDC;
- systemd socket activation;
- per-model global quotas;
- rendezvous-hash routing;
- better provider adapters;
- persistent response affinity;
- remote accounting sink;
- streaming qualifier adapter;
- built-in secret detector;
- seccomp post-start sandbox;
- cgroup-aware resource metrics.

Every proposed feature should answer:

> Does this preserve Mellomting's advantage of being small, predictable, and auditable?

If not, leave it out.

---

# 102. Research notes / upstream facts used by this plan

These are implementation references, not dependencies on exact upstream behaviour forever.

## vLLM

Current vLLM documentation describes an OpenAI-compatible server with, among other endpoints:

- `/v1/completions`
- `/v1/chat/completions`
- `/v1/responses`
- `/v1/responses/{response_id}`
- `/v1/responses/{response_id}/cancel`
- `/v1/embeddings`

It also documents Anthropic Messages compatibility and vLLM-specific administrative endpoints that Mellomting must never expose through a catch-all proxy.

Reference:

- https://docs.vllm.ai/en/latest/serving/online_serving/

vLLM's streaming usage documentation notes that Chat/Completions stream usage can be requested with `stream_options.include_usage`, or forced server-side.

Reference:

- https://docs.vllm.ai/en/latest/features/per_request_metrics/

## Landlock

Linux Landlock is intended as an unprivileged, stackable sandbox and provides filesystem, network, and IPC restriction features. TCP bind/connect rules are port-oriented; newer kernel ABIs also add additional features.

Reference:

- https://docs.kernel.org/userspace-api/landlock.html

The Go Landlock library documents all-goroutine restriction, network restriction, IPC scoping, and the important Multipath TCP caveat.

References:

- https://github.com/landlock-lsm/go-landlock
- https://pkg.go.dev/github.com/landlock-lsm/go-landlock/landlock

Go's `net` package provides explicit `SetMultipathTCP(false)` controls for both listeners and dialers.

Reference:

- https://pkg.go.dev/net

## CertMagic

CertMagic is Caddy's Go library for automated certificate issuance and renewal and can be embedded independently of the complete Caddy server.

Reference:

- https://github.com/caddyserver/certmagic

## Bifrost concepts

The useful concepts intentionally retained from Bifrost include:

- virtual-key-like access;
- model restrictions;
- request/token limits;
- weighted routing;
- bounded retries/fallbacks;
- token/latency observability;
- optional request/response guardrails.

References:

- https://docs.getbifrost.ai/features/governance/virtual-keys
- https://docs.getbifrost.ai/features/governance/budget-and-limits
- https://docs.getbifrost.ai/features/governance/routing
- https://docs.getbifrost.ai/features/retries-and-fallbacks
- https://docs.getbifrost.ai/features/observability/default
- https://docs.getbifrost.ai/enterprise/guardrails

## Qwen3Guard

Qwen3Guard includes generative guard models and a distinct stream-oriented variant intended for incremental safety classification.

Reference:

- https://github.com/QwenLM/Qwen3Guard

---

# 103. Final project rule

When choosing between:

```text
a clever feature with broad behaviour
```

and:

```text
a small explicit feature whose security properties are easy to reason about
```

choose the second.

That is the point of Mellomting.
