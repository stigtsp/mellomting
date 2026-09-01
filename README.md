# Mellomting

A small, security-conscious, OpenAI-compatible LLM **proxy and multiplexer** for
Go. Mellomting sits in front of one or more inference backends (typically vLLM),
authenticates clients with opaque API keys, applies per-key policy and rate
limits, routes public model names to backends, streams SSE responses correctly,
and accounts token usage into JSONL. After startup it confines itself with
[Landlock](https://docs.kernel.org/userspace-api/landlock.html).

The whole product is **one static Go binary** plus **two YAML files** — a config
file and a key file. No database, no management HTTP API, no dashboard, no
plugins, no telemetry.

```text
                 ┌────────────────────────────┐
  client ──────► │         Mellomting          │──────►  vLLM :8000
  (Bearer mtk_)  │  auth → limit → route       │──────►  vLLM :8012
                 │  stream → account → log     │
                 └────────────────────────────┘
```

## Why Mellomting?

- **Small and auditable** — one `cmd/mellomting` binary, internal packages only,
  no framework. An operator can understand its security boundary.
- **OpenAI-compatible** — `/v1/chat/completions`, `/v1/completions`,
  `/v1/embeddings`, `/v1/responses`, `/v1/models`. Anything it does not
  understand is passed through byte-for-byte (tool calls, reasoning, vLLM
  extensions, coding-agent parameters).
- **Secure by default** — keys are stored only as `HMAC-SHA-256(pepper, key)`
  hashes, compared in constant time; every limit has a finite default; unknown
  endpoints are 404, not proxied; the process confines itself with Landlock
  after startup.
- **First-class streaming** — SSE is relayed correctly, never retried once bytes
  have reached the client, and usage is reported exactly.
- **No telemetry** — Mellomting makes no network requests except to the backends
  you configure.

## Quick Start

Build the binary (a `bin/mellomting` is produced; `make release` builds the
Linux release artifacts):

```sh
make build
```

Optionally install it system-wide — the binary copies itself atomically
(mode 0755; symlink destinations are refused):

```sh
sudo bin/mellomting --install              # → /usr/local/bin/mellomting
bin/mellomting --install --prefix ~/.local # user-local prefix
```

Write a config. **This is the whole config** — every other section is optional
and has a safe default:

```yaml
# config.yaml
version: 1

server:
  listen:
    network: tcp
    address: 127.0.0.1:8080

auth:
  users_file: ./users.yaml
  pepper_file: ./auth.pepper

backends:
  local:
    base_url: http://127.0.0.1:8000
    upstream_model: qwen3.8-27b

models:
  qwen3.8-27b:
    backends:
      - local
```

What each part means:

- `server.listen` — where Mellomting accepts requests (`tcp` on a `host:port`,
  or `unix` on a socket).
- `auth.users_file` / `auth.pepper_file` — where API keys and the HMAC pepper
  live. Both are created in the next steps.
- `backends` — the inference servers Mellomting forwards to. `base_url` is where
  vLLM (or any OpenAI-compatible server) is listening; `upstream_model` is the
  name that server knows the model by.
- `models` — the **public** model names clients may request, and which backends
  serve each one. Clients only ever see these names.

Check it, then generate a secret pepper (it must never be world-readable) and
create an API key:

```sh
mellomting config check -config config.yaml
umask 077 && head -c 64 /dev/urandom | base64 > auth.pepper
mellomting key create -config config.yaml --name "my-first-key" --models qwen3.8-27b
```

`key create` prints the raw key **exactly once** — save it, it cannot be
retrieved later. The key looks like `mtk_XXXXXXXX_...`. It writes `users.yaml`
for you.

Run it:

```sh
mellomting serve -config config.yaml
```

And use it — Mellomting is a drop-in OpenAI-compatible endpoint:

```sh
curl http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer mtk_XXXXXXXX_..."

curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer mtk_XXXXXXXX_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen3.8-27b",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

Point any OpenAI-compatible client (OpenCode, OpenAI SDKs, cURL, …) at
`http://127.0.0.1:8080/v1` with your `mtk_…` key.

### Troubleshooting the Quick Start

- `mellomting serve` refuses to start — run `mellomting config check -config
  config.yaml` and read the error; the validator explains every problem.
- **Landlock** — `serve` enforces Landlock with `security.landlock.mode:
  required` by default. Run `mellomting sandbox check` to see if your kernel
  supports it. On a system without Landlock (e.g. macOS, or older Linux
  kernels) set `security.landlock.mode: best-effort` (or `disabled`).
- Backends must be reachable from where Mellomting runs. The default
  `security.backend_network.mode` is `loopback-only`, which is the safe choice
  for the Quick Start (Mellomting and vLLM on the same machine).
- `key create` complains the pepper is too permissive — the pepper file must be
  mode `0600` or `0640` (the `umask 077` above prevents this).

## Configuration

The design goal is that most configuration **is** the minimal config above.
Everything else has a finite, conservative default. To see the complete
effective config with all defaults applied:

```sh
mellomting config show-effective -config config.yaml
```

The most useful additions, shown minimally:

```yaml
# Route one public model across several backends (default strategy: least-inflight)
models:
  qwen3.8-27b:
    backends:
      - local-a
      - local-b

# Token accounting to JSONL (enables `mellomting usage report`)
accounting:
  enabled: true
  path: ./usage.jsonl

# TLS on the ingress listener
server:
  tls:
    mode: files
    cert_file: ./cert.pem
    key_file: ./key.pem
```

Per-key limits are set at key creation time:

```sh
mellomting key create --name "ci" --models qwen3.8-27b \
  --requests-per-second 10 --burst 20 --concurrent-requests 4
```

Notable defaults:

| Area | Default |
|------|---------|
| Backend network | `loopback-only` |
| Landlock | `required` (minimum ABI 6) |
| Logging | structured JSON at `info` |
| Accounting | disabled |
| Global rate limit | 100 req/s, burst 200 |
| Request body / response | 16 MiB / 64 MiB |
| In-flight requests | 64 |
| Backend timeouts | connect 3s, header 30s, request 20m, stream idle 120s |
| Backend concurrency | 4 in flight, queue 8, queue wait 5s |
| Retry | none (`max_attempts: 1`) |

The full reference is `docs/PLAN.md` §76 (suggested configuration) and §77
(example users file). The schema is validated fail-closed: unknown fields,
YAML anchors, duplicate keys, and multi-document files are rejected.

## API keys

Keys are managed offline — the daemon never writes the key file:

```sh
mellomting key create  --name NAME --models M[,M...] [--expires RFC3339] \
  [--requests-per-second R] [--burst B] [--concurrent-requests N]
mellomting key list
mellomting key enable  --id ID
mellomting key disable --id ID
mellomting key revoke  --id ID
```

- `--models` accepts a comma-separated list or `*` (explicit wildcard).
- The daemon applies users-file changes on `SIGHUP` reload or restart.
  Under `landlock.mode: required`, however, a `SIGHUP` reload of offline
  key changes is denied by the sandbox (the users file is pinned to its
  startup inode), so `create`, `enable`, `disable`, and `revoke` take
  effect only after a restart; the key commands print a reminder when
  the mode is `required`.
- Keys are stored only as `HMAC-SHA-256(pepper, key)` hashes; the raw key is
  printed once at creation and never stored or logged.

## API surface

Mellomting forwards only the allow-listed inference endpoints and returns `404`
for everything else (no catch-all, no backend admin paths):

```text
POST /v1/chat/completions
POST /v1/completions
POST /v1/embeddings
POST /v1/responses
GET  /v1/responses/{id}
POST /v1/responses/{id}/cancel
GET  /v1/models
```

Authenticate with `Authorization: Bearer mtk_…`. Errors are OpenAI-shaped JSON.
`/healthz` and `/readyz` return `200 ok` for health checks without
authentication.

Compatibility details (what is rewritten, capped, injected, or passed through)
are in `docs/COMPATIBILITY.md`.

## Operations

- **Health checks** — `/healthz`, `/readyz`.
- **Logs** — structured JSON to stdout/stderr; API keys, prompts, responses,
  and backend credentials are never logged.
- **Usage** — with `accounting.enabled: true`, run `mellomting usage report` for
  per-key token totals. Rotate the JSONL with `deploy/mellomting.logrotate`
  (`copytruncate`, not rename).
- **Installation** — `mellomting --install` copies the running binary to
  `/usr/local/bin/mellomting` atomically (temp file + rename, mode 0755);
  `--prefix DIR` targets another prefix. Re-running on an already-installed
  path is a no-op. The command installs the **binary only** — it creates no
  config files, secrets, or directories. The daemon also creates none of
  them itself, so the destination host must provide them: config in
  `/etc/mellomting/` (`config.yaml`, `users.yaml`, `auth.pepper` — authored
  via `mellomting key create`), the accounting log dir `/var/log/mellomting/`,
  and the runtime socket dir `/run/mellomting/`. On systemd,
  `deploy/mellomting.service` auto-creates the last three
  (`RuntimeDirectory`/`StateDirectory`/`LogsDirectory`); on non-systemd
  hosts create them as needed. Mellomting deliberately ships no default
  config or boilerplate secrets — they are operator-authored.
- **systemd provisioning** — on a Linux root host,
  `mellomting --install --systemd` provisions the daemon end-to-end in the
  single binary: it creates the unprivileged `mellomting` service account
  (system user, `nologin` shell), the config/log/state/run dirs with strict
  owners and modes (`/etc/mellomting` root:mellomting 0750; the rest
  mellomting:mellomting 0750), writes a **commented scaffold config** at
  `/etc/mellomting/config.yaml` (0640 root:mellomting) when no config
  exists — a byte-identical copy of
  `deploy/mellomting-config.yaml.example`, deliberately incomplete (no
  `backends:`/`models:`) so `mellomting config check` shows exactly what is
  left and the daemon stays fail-closed until it is filled in; an existing
  `config.yaml` is never touched — installs the hardened unit at
  `/etc/systemd/system/mellomting.service` and the logrotate policy at
  `/etc/logrotate.d/mellomting`, then runs `systemctl daemon-reload`. It is
  fail-closed: it refuses to run on non-Linux, as non-root, or when systemd
  is not the active init, and never creates secrets — the pepper and the
  users file remain operator-authored steps printed after provisioning
  (including `chmod 640` so the daemon user can read them). The unit's
  `ExecStart` is rendered from the installed binary path (`--prefix`
  aware); sync tests prove the embedded assets match `deploy/`. See also
  `HARDENING.md`.
- **systemd** — a hardened unit is in `deploy/mellomting.service`.

## Security

Mellomting is fail-closed by design:

- Authentication, authorization, malformed configuration, and missing sandbox
  setup all fail closed.
- Client keys are `HMAC-SHA-256` hashed and compared in constant time; the
  pepper lives in a separate 0600 file.
- Client auth headers and proxy identity headers are stripped before
  forwarding; backend credentials come only from config.
- Every request is bounded: bodies, headers, SSE events, concurrency, queues,
  retries, and response size.
- Landlock confines the process after startup to exactly the files and backend
  ports it needs.

See `SECURITY.md` (reporting), `THREAT_MODEL.md`, and `HARDENING.md`.

## Development

```sh
make check    # gofmt, build, vet, tests, race, staticcheck, govulncheck
go test ./...
go test -race ./...
```

`docs/PLAN.md` is the design document (source of truth) and `AGENTS.md`
contains guidance for contributors. This is a small, dependency-light Go
project; the allowed dependencies are enumerated in PLAN §88.
