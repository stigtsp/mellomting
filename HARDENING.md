# Hardening

Concrete controls, referenced to `docs/PLAN.md`. Phase markers show where a
control lands in the implementation plan.

## Authentication and keys

- Opaque bearer keys `mtk_<id>_<secret>`, 256-bit secrets from
  `crypto/rand`, accepted only in `Authorization: Bearer` / `X-Api-Key`
  headers, never query parameters (§25). [Phase 1]
- Keys stored only as `HMAC-SHA-256(pepper, key)`; pepper in
  `/etc/mellomting/auth.pepper` (0640, `root:mellomting`); constant-time
  comparison with a dummy HMAC for unknown key IDs (§26, §27). [Phase 1]
- No network management API: keys are managed only by the offline
  `mellomting key` CLI with atomic, locked users-file updates (§29).
  [Phase 3]

## Request path

- Explicit allow-listed inference endpoints only; every other path —
  including backend admin paths such as `/metrics` — is 404. No catch-all
  route (§11.3). [Phase 1]
- Client `Authorization` / `Proxy-Authorization` / `X-Api-Key` and proxy
  identity headers are stripped before forwarding; backend credentials are
  injected only from trusted configuration and read from secret files
  (§17, §18). [Phase 1]
- Shallow body parsing into `map[string]json.RawMessage`; only
  routing/policy fields are inspected, unknown fields pass through (§12).
  [Phase 1]
- Bounded everything: bodies, header size, SSE event size, in-flight
  requests, buffered bytes, backend queues, retries, qualifier concurrency
  (§4, §9.1, §22, §32-37). Every limit has a finite default.
- A streamed request is never retried after response bytes reached the
  client (§24). [Phase 2]

## Configuration handling

- Size-bounded YAML (1 MiB), strict known-field decoding, no
  aliases/anchors, no custom tags, no duplicate keys, no unsupported
  versions; fail-closed validation (§28). [Phase 0 — `config check`]
- Backend URLs validated at start-up: no query/fragment/userinfo/path,
  loopback IP literals required in `loopback-only` mode (§15.1, §16).
  [Phase 0 validation / Phase 1 enforcement]
- Plaintext non-loopback TCP listeners fail unless explicitly opted in
  with `allow_plaintext_non_loopback: true` and a loud startup warning
  (§8.2). [Phase 1]

## Ingress TLS

- Static TLS (`server.tls.mode: files`) wraps the TCP listener before
  Landlock enforcement: the certificate and key are loaded before the
  sandbox is applied and their FDs are closed before activation
  (§57 step 9, §59, §67). [Phase 6]
- Secure defaults: TLS 1.2 minimum with Go's built-in cipher suites
  (§97). Certificate reload requires a process restart in v1 (§67).
- Certificate and key are read without following the final symlink and
  must be regular files bounded to 1 MiB (§28, via `internal/securefile`).
- `tls.mode: acme` is shelved for the first release: the configuration
  stays validated, but `serve` refuses to start with it, so it is never a
  silent no-op (§68). Reverse-proxy TLS remains the recommended hardened
  deployment (§67, §68).
- Plaintext non-loopback TCP requires explicit opt-in (§8.2); loopback
  TCP and the Unix socket are the intended fronting modes for nginx or
  `tailscale serve`.

## Sandbox (Landlock)

- Applied on Linux at start-up, after all secrets are preloaded and their
  FDs are closed, and before the listener accepts (§57); a `required`
  policy never degrades silently to no sandbox — start-up fails instead,
  and the library's `BestEffort()` downgrade path is never used (§55).
  [Phase 4 — `internal/landlock.Apply`]
- Enforced on all Go runtime threads via the ABI 8+ all-thread TSYNC path;
  threads created afterwards inherit the confined domain at clone time
  (§56, covered by `TestAllThreadsEnforced`).
- Post-startup policy is minimal (§58): read the users file (SIGHUP
  reload), write the accounting log, connect to the configured backend
  TCP ports; execute nowhere; everything else denied. Scoped IPC
  (signals / abstract Unix sockets to processes outside the domain) is
  restricted on ABI 6+ (§62).
- `best-effort` mode enforces the full policy or continues with a loud
  warning (never a partially degraded policy); TCP port rules are
  port-based only — external address restriction comes from the backend
  network modes (§16, §60).
- Multipath TCP explicitly disabled on every listener/dialer because Go
  1.24+ default listeners are MPTCP-capable and bypass classic TCP
  restrictions (§61).
- Default policy: `mode: required`, `minimum_abi: 8` (§55).
- Non-Linux builds clearly report that Landlock is unavailable and
  `Apply` fails (§7); `sandbox check` reports capability either way.
- `deploy/mellomting.service` ships the hardened systemd unit that
  complements the in-process sandbox (§64).

## Logging

- Structured JSON to stdout/stderr only; operational fields (request ID,
  key ID, model, status, durations, error class) — never prompts,
  responses, keys, credentials, or raw backend error bodies (§43).
- Backend failures map to sanitized error classes (e.g.
  `backend_connect`, `backend_5xx`); client errors are OpenAI-shaped JSON
  with no Go stack traces, paths, or hostnames (§72).

## Build and supply chain

- `CGO_ENABLED=0`, `-trimpath`, version/commit/build-date embedded
  (PLAN §89, Makefile). Primary targets `linux/amd64`, `linux/arm64`.
- CI: `gofmt`, `go vet`, staticcheck, govulncheck, `go test`,
  `go test -race`, a fuzz smoke for the parser, CodeQL (via
  `security-events`), SHA-pinned actions, Dependabot for Go modules and
  GitHub Actions (PLAN §89, `.github/workflows/ci.yml`).
- Configuration files, users file, pepper, and backend secret files are
  opened without following symlinks where practical (§28) [Phase 4,
  `internal/securefile`].

## Deployment recommendations

- Unix socket under `systemd RuntimeDirectory` with mode 0660 (§8), or
  loopback TCP.
- Backends on loopback, as different unprivileged users, admin/dev
  endpoints disabled, no dynamic LoRA loading (§67).
- TLS at a trusted reverse proxy for the strongest profile; native TLS/ACME
  is convenience, not the recommended hardened deployment (§67, §68).
