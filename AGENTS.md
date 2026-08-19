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
`accounting`, `auth`, `backend`, `config`, `guard`, `httpapi`, `landlock`,
`limiter`, `logging`, `proxy`, `routing`, `securefile`, `tlsconfig`,
`version`.

## Build, test, quality gates

```text
gofmt -l .            # must be empty
go build ./...
go test ./...
go test -race ./...
go vet ./...
staticcheck ./...     # if installed
```

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

Phase 0 (repository skeleton) — module, CLI entry point, and package layout
exist. `mellomting version` and `mellomting help` work; all other commands
are stubs. Next: structured logging, config loader with strict validation,
`config check` and `sandbox check` (PLAN §91).
