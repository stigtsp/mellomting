# Operations

Examples use an installed `mellomting` binary and `/etc/mellomting/config.yaml`.
For a local build, use `bin/mellomting` and append `--config ./config.yaml` to
commands that read configuration. The configuration flag follows the command
or subcommand; there is no automatic lookup in the working directory.

## Sandbox

Sandbox support depends on the platform:

| Platform | Mechanism | Configured by |
| --- | --- | --- |
| Linux | Landlock | `security.landlock.mode` |
| macOS | Seatbelt | `security.seatbelt.mode` |

Both default to `best-effort`: apply the sandbox when available and warn
otherwise. Set `required` to refuse startup without it, or `disabled` to skip
it. Only the setting for the current platform applies.

Check support and the configured startup behavior:

```sh
mellomting sandbox check
```

On macOS, a process already inside another sandbox may be unable to apply
Seatbelt. In `required` mode, this prevents startup.

Landlock's `minimum_abi` defaults to 6, which needs Linux 6.12 or newer.
With a lower ABI, `best-effort` runs without a sandbox and logs a warning;
`required` refuses startup.

## API keys

```sh
mellomting key create NAME
mellomting key list
mellomting key disable ID
mellomting key enable ID
mellomting key revoke ID
```

A key may use every model unless `--models` narrows it to a comma-separated
list of public names. The name must start with a lowercase letter and contain
only lowercase letters, digits and underscores, up to 32 characters; several
keys may share one. `--expires` takes a date (`2027-01-01`) or an RFC 3339
timestamp.

`key create` prints only the new key and a newline to stdout; confirmation
and apply instructions go to stderr. To capture it in a script:

```sh
KEY=$(mellomting key create ci --models my-model)
```

Save the key securely. It cannot be retrieved later. Keys are stored as
HMAC-SHA-256 hashes with a separate pepper file. Their format is
`sk-<username>-<keyid>-<secret>`, with a 16-character hex ID and a 256-bit secret.

Key commands update the users file. The running daemon applies valid changes
automatically within about a second. Existing requests continue normally.
Invalid or unreadable updates leave the previous key store active and log an
error. To trigger an immediate check:

```sh
sudo systemctl reload mellomting
```

Without systemd, send the daemon `SIGHUP`. Automatic checks use modification
time, file identity, size, and permissions. If an editor changes a file in
place while preserving all of those, use SIGHUP to force a reload.

## Agents

With `agents.enabled: true` (see [Configuration](CONFIGURATION.md#agents)),
remote inference servers attach themselves. On the proxy (the "edge"),
create one agent key per deployment, and client keys for the namespace:

```sh
AGENT_KEY=$(mellomting key create gpu_lab --agent lab --expires 2027-01-01)
mellomting key create alice --models 'lab/*'
```

An agent key does nothing but attach: every other route rejects it. Its
namespace is the first segment of every model its agents register, so a
server advertising `qwen3` appears as `lab/qwen3`. `--agent '*'` registers
the advertised names unprefixed. A client key's `NAMESPACE/*` entry grants
every model under that namespace.

`--register` narrows an agent key to the listed advertised ids, each
optionally served under an alias (`ID=ALIAS`). Use it for servers that list
everything they hold, such as Ollama: two hosts can each serve one model at
the root, under names you choose:

```sh
mellomting key create ollama_a --agent '*' --register qwen3-coder:30b=coder
mellomting key create ollama_b --agent '*' --register gpt-oss:20b
mellomting key create alice --models coder,gpt-oss:20b
```

Clients see `coder` and `gpt-oss:20b`; the edge sends `qwen3-coder:30b`
upstream for `coder`. Unlisted models are ignored, and `agents.max_models`
counts only the listed ones, so a host listing many models still attaches.
An alias contains no `/`, and under `*` neither does the served name, so
`org/model` ids need an alias there. Editing the list (re-create the key)
detaches the key's agents; they re-attach under the new list.

Next to the inference server, install Mellomting as on the edge (the Debian
package, or `install --systemd`) and let `init` write a configuration that
attaches the server instead of serving clients:

```sh
echo "$AGENT_KEY" | sudo mellomting init --edge https://edge.example.org \
  --server http://127.0.0.1:8000
sudo systemctl enable --now mellomting
```

`init --edge` checks the key with the edge before writing anything; a
refused key, or agents not enabled on the edge, aborts with nothing
written. It reports whether each `--server` answers yet (one still loading
is fine: the agent waits for it), then writes `/etc/mellomting/agent.key`
and a `config.yaml` whose `attach` section names it (see
[Configuration](CONFIGURATION.md#attach)). `--server` defaults to
`http://127.0.0.1:8000`; repeat it for several local servers, each attached
on its own connection. `--backend-key-file` sends a bearer token to the
local servers, which the edge never sees, and `--ca-file` verifies the edge
against your own CA; both are copied beside the configuration. The edge
must be `https://` unless it is a loopback IP address; `--insecure-edge` sends
the key over plain http anyway.

To rotate the key, replace `agent.key` and restart the service.
`mellomting agent check` repeats the key check and probes the servers, for
scripts that want to fail fast. The agent recovers by itself:

- It waits until the local server lists a model before attaching
  (`waiting for backend`).
- It detaches while the server is down (`backend unavailable; detaching`)
  and reattaches when it returns. A failed health check is not counted
  while requests are in flight, so a busy server is never cut off.
- When its model list changes it opens a new attachment before draining
  the old one, so in-flight streams finish. At an attachment cap it
  replaces the attachment in place, with a gap of one handshake.
- It reconnects with jittered backoff when the connection drops, the
  network goes silent, or the edge restarts.
- While its key is disabled or revoked it keeps retrying, logging
  `agent disconnected; reconnecting` with
  `error="edge refused attach: 401 Unauthorized"`.

Stopping the service drains instead of cutting streams: the edge stops
routing to it at once (`top` shows `draining`), and in-flight streams get
up to `shutdown.grace_period` (default 30s) to finish. Set it above your
longest expected stream; above 90 seconds, also raise the unit's
`TimeoutStopSec` with a drop-in, or systemd kills the drain. While the only
agent serving a model drains, new requests for that model get 404; run two
agents per model for restarts without a gap.

### Scaling to hundreds of sessions

One agent connection carries any number of concurrent streams: each is an
HTTP/2 stream with its own flow control, so a client that stops reading
does not stall the others. The test suite runs 256 concurrent streams
through one agent. What limits concurrency is configuration; raise every
layer a request passes through:

| Layer | Setting | Default | For about 256 sessions |
| --- | --- | --- | --- |
| Agent | `agents.max_concurrency` (per attachment; match the engine, e.g. vLLM `--max-num-seqs`) | 8 | 256 |
| Agent queue | `agents.queue_size`, `agents.queue_timeout` | 32, 120s | as needed for bursts above the cap |
| Proxy | `server.max_inflight_requests` | 64 | 512 |
| Client key | `key create --concurrent-requests` | 8 | 256 or more |
| Burst of new requests | `limits.global_burst`, `limits.global_requests_per_second` | 200, 100/s | 1000, 500/s |
| Burst per source address | `limits.preauth_burst`, `limits.preauth_requests_per_second` | 200, 100/s | 1000, 500/s |

Above `agents.max_concurrency`, requests wait in the agent's queue (shown
as QUEUE in `mellomting top`); beyond the queue they get 503. A request
over any other limit gets 429 or 503 at once.

On the edge, `agent attached` lists each agent's models and context
windows. `agent detached` gives the reason (connection closed or lost, agent
key no longer active, discovery failed, edge shutting down), how long the
agent was attached, and the requests it served. `mellomting top` adds an
agents table: slots in use out of `max_concurrency`, queued requests, open
streams, requests and transport failures per agent. Each request row names
the agent serving it. Request log lines and accounting records name the
agent as the backend (`agent-<key name>-<n>`).

On the agent, `agent status` lines report active and total requests every
minute while there is traffic, and `logging.level: debug` logs each
relayed request's path, status, size and duration (never bodies).
Revoking, disabling or expiring the agent key detaches its agents at once.
A TLS terminator or reverse proxy in front of the edge must pass HTTP/1.1
`Upgrade` requests through; the agent always requests HTTP/1.1.
CDNs and WAFs may not: see `docs/AGENT_INTERMEDIARIES.md` (open problem).

## Installation

To install just the binary from a local build:

```sh
sudo bin/mellomting install
```

This installs `/usr/local/bin/mellomting`. Use `--prefix "$HOME/.local"` for a
user-local installation; ensure its `bin` directory is on your `PATH`.
Binary installation creates no configuration or auth files. It uses atomic
replacement, sets mode `0755`, and refuses symlink destinations.

On Linux with systemd, `sudo bin/mellomting install --systemd` also prepares:

- The unprivileged `mellomting` service account.
- `/etc/mellomting` owned by `root:mellomting`, mode `0750`.
- Log, state, and runtime directories owned by `mellomting:mellomting`, mode `0750`.
- The service unit and logrotate policy, followed by `systemctl daemon-reload`.
- An incomplete configuration, random pepper, and empty users file when new,
  each mode `0640`, owned by `root:mellomting`.

The service is not started automatically. Complete the server and model
entries, validate the configuration, and create a key before starting it.
The unit uses the installed binary path, including a custom `--prefix`.

Existing configuration and auth files are never rewritten. With an existing
configuration, a missing default auth file is created only when the
configuration explicitly names that exact default path. Custom auth paths
remain operator-managed.

### Debian package

`make deb` builds `dist/mellomting_<version>_<arch>.deb` for amd64 and arm64
on any host with Go; it fetches nfpm through `go run`. The package installs:

- `/usr/bin/mellomting`.
- The service unit at `/lib/systemd/system/mellomting.service`, identical to
  `deploy/mellomting.service` except for the binary path.
- The logrotate policy at `/etc/logrotate.d/mellomting`, as a conffile.
- The configuration example at `/usr/share/mellomting/config.yaml.example`.

On install it creates the `mellomting` service account and the configuration,
log, and state directories with the same owners and modes as
`install --systemd`. It writes nothing into `/etc/mellomting` and neither
enables nor starts the service. Generate the configuration and auth files in
place, create a key, then start the service:

```sh
sudo apt install ./mellomting_<version>_<arch>.deb
sudo mellomting init --server http://127.0.0.1:8000 --config /etc/mellomting/config.yaml
sudo mellomting key create production
sudo systemctl enable --now mellomting
```

Because `/etc/mellomting` is owned by `root:mellomting`, `init` writes its files
with that group and mode `0640`, so the service can read them. On upgrade a
running service is restarted so the new binary takes effect. Removing the
package stops the service and keeps `/etc/mellomting`; purging deletes the
configuration, auth files, state, and accounting log, but keeps the service
account.

### Releases

Pushing a release tag (`0.2.0`, `v0.2.0`; a suffixed tag such as `0.2.0-rc1`
becomes a pre-release) runs `.github/workflows/release.yml`: it re-runs the
quality gates on the tagged commit, builds `make deb` and `make release`, and
publishes a GitHub release with the three static binaries, the Debian
packages, and `SHA256SUMS` over all of them. The embedded version must equal
the tag, so a release is always built from exactly the tagged tree. Verify a
download against the published sums:

```sh
sha256sum -c --ignore-missing SHA256SUMS
```

## Health, logs, and usage

`/healthz` and `/readyz` are available without authentication. All inference
requests require an API key. Unsupported routes, including backend admin
paths such as `/metrics`, return `404`.

Operational logs are structured JSON to stdout/stderr. API keys, prompts,
responses, and backend credentials are never logged. For a systemd service:

```sh
sudo journalctl -u mellomting
```

Every completed request writes one `request` line, which is the access log:

```json
{"time":"...","level":"INFO","msg":"request","request_id":"...","key_id":"a1b2",
 "key_name":"ci","remote":"198.51.100.9","user_agent":"codex-cli/1.2.3",
 "endpoint":"POST /v1/chat/completions","public_model":"my-model",
 "backend":"a","status":200,"duration_ms":8412,"bytes_in":712,"bytes_out":4108,
 "tokens_in":120,"tokens_out":480,"tokens_total":600,"usage_status":"exact",
 "retry_count":0,"queue_ms":0,"error_class":"ok"}
```

`remote` is the client address after `server.trusted_proxies` is applied, and
`unix` for a Unix-socket client. `user_agent` is client-controlled: it is
truncated and stripped of control characters, but not otherwise trusted.
`usage_status` says whether the backend reported the token counts or they are
unknown. `queue_ms` is the time spent waiting for a backend admission slot,
summed over attempts; the rest of `duration_ms` is the backend's. To read only
the access log:

```sh
sudo journalctl -u mellomting -o cat | jq 'select(.msg == "request")'
```

`error_class` says which kind of failure occurred, not which request was
malformed. Each bound that can expire has its own class, so a `504` or `503`
names the setting to look at:

| class | bound that ran out |
|-------|--------------------|
| `backend_dial_timeout` | `connect_timeout` |
| `backend_header_timeout` | `header_timeout`: no response header, so queueing and prefill inside the engine |
| `backend_timeout` | `request_timeout` on a non-streaming request |
| `queue_full` | `queue_size`: no slot to wait in |
| `queue_timeout` | `queue_timeout`: waited, but no concurrency slot came free |

The timeouts in force for each backend are logged at startup on the
`backend admission` line. When the class alone is not enough — to tell a chat-template
mismatch from a tool-schema, output-limit, or context-window failure, or to
see which backend timed out during a wave of `504`s — raise `logging.level`
to `debug`. Every failed attempt then writes a line of its own, carrying what
the backend said when it answered at all:

```json
{"time":"...","level":"DEBUG","msg":"backend attempt failed","request_id":"...",
 "backend":"a","error_class":"backend_4xx","queue_ms":0,"attempt_ms":912,"status":400,
 "upstream_type":"BadRequestError","upstream_code":"400",
 "upstream_message":"only one system message is allowed"}
```

A failure with no response at all — a header timeout, a refused connection, a
full queue — carries the class and the backend but no `status` or
`upstream_*` fields, because there was nothing to read. `queue_ms` and
`attempt_ms` split each attempt between waiting for admission and waiting on
the backend.

Only the `type`, `code`, and `message` members of an OpenAI-shaped error body
are read, each bounded and stripped of control characters; everything else in
the body is discarded unparsed. It is off by default because a rejection
message can quote the request that caused it.

When the backend answered with a status but a body this cannot read — not
JSON, or past the parse bound — the `upstream_*` fields are present and
empty. That is the answer: the backend refused the request and said nothing
that can safely be repeated. A body past `max_response_bytes` is a different
failure (`backend_5xx`) and never reaches this point.

None of this covers a backend that reports a failure inside a stream it has
already answered `200` to: that reaches the client as stream content and is
logged as `backend_stream_error` or `ok`, not as a rejection.

Both the nested `{"error": {...}}` shape and the flat `{"object": "error",
...}` shape are read; SGLang uses the flat one on chat completions and the
nested one on `/v1/responses`.

To hand the same message to the client instead of only the operator, set
`relay_upstream_errors: true` on that server's entry under `servers:` (see
[Configuration](CONFIGURATION.md)). The message is appended to the sanitized
error; its status, `type`, and `code` do not change. Only a 4xx is relayed:
a 5xx says the server broke rather than that the request was wrong, and its
text is where a traceback or a model path would appear. The debug log above
carries those either way. Setting it says the server and every client routed
to it are equally trusted.

With accounting enabled, `mellomting usage report` reports per-key token and
request totals. Use the supplied logrotate policy for the JSONL file; it uses
`copytruncate` because the sandbox retains access to the open file. Renaming
the file does not redirect the running writer to a replacement.

## Watching the daemon

`mellomting top` shows what the daemon is doing and what is holding it back.
Configure an admin socket:

```yaml
server:
  admin_socket: /run/mellomting/admin.sock
```

Restart the daemon, then run `mellomting top`. To try the view without a
daemon, run `mellomting top --simulate`.

It opens on **Overview**: throughput graphs for the last five minutes, an
attention box listing what is holding traffic back (a key refused at its
`concurrent_requests`, a server in cooldown, vLLM queueing behind the edge,
accounting dropping records), the busiest keys, servers and models, and the
live requests. A refusal is counted apart from errors: it never reaches a
backend, so the gap between *offered* and *completed* requests is traffic a
limit turned away.

The other views are **Keys** (in flight against `concurrent_requests`, rate
against `requests_per_second`, token quota), **Models**, **Backends** (edge
admission beside the server's own report: vLLM and SGLang running, waiting
and KV cache use; Ollama's loaded models), **Requests**, and **Usage** (token
use per key, model or server over 1 hour, 24 hours and 7 days, from the
accounting log). Enter opens a detail box for the selected item. The keys are
listed in the bottom border.

Server reports come from `/metrics` (vLLM; SGLang started with
`--enable-metrics`) or `/api/ps` (Ollama). A server that publishes neither
shows "no metrics". To stop reading one, set `metrics: false` on it. An
agent's operator can refuse with `attach.metrics: false`.

The Usage view reads the accounting log and its rotations directly, so run
`top` as a user who can read it; if it cannot, that view says so and the rest
still work.

`r` (or `--redact`) replaces IP addresses with labels such as `ip-1`, the same
for the whole session, for screenshots and screen sharing. `--once`, or output
that is not a terminal, prints plain text; `NO_COLOR` turns colour off. A
daemon older than this `top` gets the old request table.

The socket has mode `0600`, restricting access to the service account and
root. It exposes client addresses, user agents, and token usage. Nothing is
tracked when no admin socket is configured.

## Troubleshooting

- Configuration errors: run `mellomting config check` and correct the named
  field. Use `config show-effective` to inspect defaults.
- Sandbox startup errors: run `mellomting sandbox check`. The required mode
  refuses startup without full confinement. Use a supported Linux kernel, or
  explicitly choose `best-effort` if running without the sandbox is acceptable.
- Connection failures: check that inference servers are reachable from the
  proxy host and permitted by `security.backend_network`.
- Pepper permission errors: use mode `0600` for a local file, or `0640` with
  an appropriate service group. The setup commands create these modes for you.
- Key changes not taking effect: check the daemon logs for a reload error
  and confirm the key command uses the same configuration as the daemon.
- `mellomting top` cannot reach the daemon: check that `server.admin_socket` is
  set in the same configuration the daemon was started with, and that the
  daemon has been restarted since it was added.
