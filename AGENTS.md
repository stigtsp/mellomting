# AGENTS.md

Guidance for AI coding agents (and humans) working in this repository.

## What this project is

Mellomting is a small, security-conscious, OpenAI-compatible LLM proxy and
multiplexer written in Go. It authenticates clients with opaque bearer keys,
applies per-key policy and rate limits, routes public model names to local
inference backends (vLLM), streams SSE responses correctly, accounts token
usage into JSONL, and confines itself with Landlock after startup.

- One Go binary: `mellomting`
- One YAML config + one YAML API-key file
- No database, no management HTTP API, no dashboard, no plugins

The intended mental model: **a small authenticated inference router, not an
AI platform.** Its advantage is being small enough that an operator can
understand its security boundary. When in doubt, choose the small explicit
feature whose security properties are easy to reason over.

## The design spec

`docs/PLAN.md` is the working design document and the source of truth. It is
written in RFC 2119 terms (MUST / MUST NOT / SHOULD / MAY).

- Follow the implementation phases in order (PLAN §91–98). Phase 0 is the
  repository skeleton; there is no inference code yet.
- The security invariants (PLAN §4) are non-negotiable. Never weaken a
  security requirement merely to make a test pass.
- Respect the explicit non-goals (PLAN §3): no web UI, no HTTP management
  API, no OAuth/OIDC, no PostgreSQL/Redis, no plugins, no databases server,
  no telemetry of any kind (PLAN §90).
- Security-sensitive behaviour MUST have automated tests.
- If an upstream (OpenAI/vLLM) API changed since PLAN.md was written,
  preserve the plan's architectural and security intent and update
  `docs/COMPATIBILITY.md`.
- PLAN gives concrete defaults (timeouts, limits, error formats). Use them
  unless testing shows they are impractical.

## Security rules that shape all code

- Fail closed for authentication, authorization, malformed configuration,
  and missing required sandbox setup.
- No catch-all reverse proxy route. Only explicit allow-listed inference
  endpoints are forwarded; everything else (including backend admin paths
  like `/metrics`) is 404 (PLAN §11.3).
- Never log API keys, prompts, responses, tool arguments, backend
  credentials, or raw backend error bodies (PLAN §24, §41, §43). Operational
  logs are structured JSON to stdout/stderr; backend failures map to
  sanitized error classes.
- Client API keys are stored only as `HMAC-SHA-256(pepper, key)` hashes and
  compared with `subtle.ConstantTimeCompare` (PLAN §25–27).
- Client `Authorization` / `Proxy-Authorization` / `X-Api-Key` headers and
  proxy identity headers are stripped before forwarding; backend auth is
  injected only from trusted config (PLAN §17–18).
- Shallow-parse request bodies: decode into `map[string]json.RawMessage`,
  inspect only routing/policy fields (`model`, `stream`, `max_tokens`, ...),
  and pass unknown fields through unchanged (PLAN §12).
- Bound everything: request bodies, header size, SSE event size, global and
  per-key concurrency, backend queues, retries (PLAN §4, §9.1, §22, §23).
- Streaming is first-class; a request is never retried once response bytes
  have reached the client (PLAN §24).
- Landlock (Linux-only) must be applied to all Go runtime threads, and MPTCP
  must be explicitly disabled on every listener/dialer; on non-Linux builds,
  clearly report that Landlock is unavailable rather than silently
  degrading (PLAN §54–63).
- Never return Go stack traces, filesystem paths, backend hostnames, or
  backend secrets in client-facing errors; use OpenAI-shaped JSON errors
  (PLAN §72).

## Repository layout

Per PLAN §78:

```text
cmd/mellomting/    entry point + CLI subcommands
internal/          all implementation packages (no public pkg/)
docs/PLAN.md       design spec (working document)
```

Internal packages (create files as phases land, in PLAN §78):
`accounting`, `apierr`, `auth`, `backend`, `config`, `discovery`,
`httpapi`, `landlock`, `limiter`, `logging`, `proxy`, `redact`,
`routing`, `securefile`, `systemd`, `testsupport`, `tlsconfig`,
`version`.

## Build, test, quality gates

```text
gofmt -l .            # must be empty
go build ./...
go test ./...
go test -race ./...
go vet ./...
staticcheck ./...     # if installed, on the primary linux build
govulncheck ./...     # if installed (also in `make check`)
```

`make check` runs all of the above and fails on `gofmt -l` output. staticcheck
and govulncheck are run on the primary linux build (as in CI). SA4023 under
`GOOS=darwin` at `serve.go` is a known non-issue: `landlock.Apply` must fail
closed on non-Linux (PLAN §7), so its nil-check is intentionally always-true
there.

- Security-critical behaviour must have automated tests; fuzz the parser
  targets listed in PLAN §80 with arbitrary bytes.
- Do not disable security limits to make benchmarks or tests pass.
- Release builds: `CGO_ENABLED=0`, `-trimpath`, primary targets
  `linux/amd64` and `linux/arm64`, version/commit embedded (PLAN §89).

## Conventions

- Prefer the Go standard library. Allowed dependencies are those justified in
  PLAN §88 (`gopkg.in/yaml.v3`, `github.com/landlock-lsm/go-landlock`,
  `golang.org/x/sys` if required). Every new dependency needs a documented
  reason; pin Landlock to a reviewed release.
- Keep interfaces narrow; avoid interface-heavy architecture for internal
  code with a single implementation (PLAN §79).
- Use Go's `net/http` server; do not embed a full web server or framework.
- Idempotent, deterministic, testable routing (PLAN §19); no latency
  learning in v1.
- Match the style of neighbouring code; no vendored frameworks.

## Current status

Phase 0 and Phase 1 (PLAN §91–92) are complete and pass the quality gates:
module, CLI entry point, package layout, structured logging
(`internal/logging`), strict config loader (`internal/config`),
`mellomting config check` / `config show-effective` / `sandbox check` /
`key create|list|enable|disable|revoke` / `serve`, version/commit embedding
(Makefile), CI, and `SECURITY.md` / `THREAT_MODEL.md` / `HARDENING.md`.

Phase 1 adds `internal/securefile`, `internal/auth` (key store, HMAC,
offline CLI), `internal/routing`, `internal/backend` (bounded upstream
client, header sanitation, MPTCP-off dialer, admission queues),
`internal/proxy` (shallow parse, model rewrite, SSE streaming, Responses
affinity, byte budget), `internal/httpapi` (allow-listed routes, bearer
auth, `/v1/models`, health endpoints, no catch-all), and the `serve`
command (Unix/TCP listeners, signals, graceful shutdown).

Phase 2 adds multi-backend routing and resilience (PLAN §93): per-model
strategies (single, round-robin, weighted round-robin, least-inflight,
weighted least-inflight) with live admission load; passive backend health
with exponential cooldowns; a bounded pre-stream retry/fallback budget
(`retry.max_attempts`, jittered exponential backoff, immediate fallback
to untried replicas, no retry after any byte reaches the client); and
distinct timeout classes (dial vs. header vs. total) so only PLAN §23
retry candidates are retried.

Phase 3a adds request-rate and concurrency limiting (PLAN §32-35):
bounded per-key and global RPS limiters with burst, plus global and
per-backend in-flight concurrency caps admitted before forwarding.

Phase 3b adds token accounting and quotas (PLAN §36-44):
`internal/accounting` (backend-reported usage parsing, bounded JSONL
writer with drop-and-alert overflow, fixed UTC per-key token windows,
startup replay, per-key report), the generative output cap (reject over
cap, inject cap when absent, never raise a client limit), stream-usage
injection with synthetic-chunk swallow, conservative unknown-usage
reservation, the `mellomting usage report` CLI, and
`docs/COMPATIBILITY.md`.

Phase 4 adds Landlock hardening (PLAN §95): `internal/landlock` builds
the minimal post-startup policy from config (users-file read, accounting
write, backend TCP connect ports; everything else denied), enforces it
strictly on all runtime threads (ABI 8+ TSYNC path) only after all
startup FDs are settled and before the listener accepts (PLAN §57);
`security.landlock.mode: required` fails closed, `best-effort` warns and
continues (never a partial policy). MPTCP is explicitly disabled on every
listener/dialer it owns (PLAN §61). `deploy/mellomting.service` ships the
hardened systemd unit (PLAN §64). `mellomting install --systemd` (via
`internal/systemd`, assets embedded byte-identical to `deploy/`) provisions
the service end-to-end: system user, config/log/state/run dirs, the unit,
logrotate, and — when no config exists — a **commented scaffold** at
`/etc/mellomting/config.yaml` (0640 root:mellomting). The scaffold is
deliberately invalid until `backends:`/`models:` are filled in (it is not a
default config; serve stays fail-closed), and an existing config.yaml is
never rewritten; secrets (pepper, users file) remain operator-authored.

Phase 6 adds static TLS (PLAN §97, §67): `internal/tlsconfig` loads the
configured certificate/key before the sandbox is applied (PLAN §57 step 9),
enforces a TLS 1.2 minimum, and the TCP ingress listener is wrapped with
`tls.NewListener` before Landlock enforcement; certificate reload requires a
process restart in v1.

Shelved beyond the first release: the qualifier framework (PLAN §96) and
native ACME (PLAN §68). The configuration schema stays forward-compatible,
but `serve` refuses to start when a qualifier or `tls.mode: acme` is
configured, so neither is ever a silent no-op.

Remaining deferred work (later phases): optional stickiness (PLAN §20);
defence-in-depth review (PLAN §98). (The §30 SIGHUP users-file reload is
implemented; under `landlock.mode: required` offline key changes are
pinned-inode-denied and require a restart, FIX-02/N2.)
