# Operations

Examples use an installed `mellomting` binary and `/etc/mellomting/config.yaml`.
For a local build, use `bin/mellomting` and append `--config ./config.yaml` to
commands that read configuration. The configuration flag follows the command
or subcommand; there is no automatic lookup in the working directory.

## Sandbox

The daemon confines itself after startup, once every secret is loaded and
its file descriptors are closed. Which mechanism enforces that depends on
the host, and only the one for the running platform is consulted:

| Platform | Mechanism | Configured by |
| --- | --- | --- |
| Linux | Landlock | `security.landlock.mode` |
| macOS | Seatbelt | `security.seatbelt.mode` |

Both default to `best-effort`: the sandbox is applied wherever the host
can enforce it, and a host that cannot is served with a warning. It is
containment for a process that is already compromised, not the control
that keeps an attacker out, so it does not decide whether the proxy runs.
Set `required` to refuse to start unless the policy is enforced, or
`disabled` to skip it. A configuration may carry both sections and be
served on either platform.

```sh
mellomting sandbox check
```

reports the backend for this host, whether it is available, and what the
configured mode would do. Under `required` a host that cannot enforce the
policy does not start the daemon — including a macOS host that is already
running it inside another sandbox, which may not apply a second profile.

Landlock's `minimum_abi` defaults to 6, which needs Linux 6.12 or newer.
On an older kernel the sandbox is not applied at all rather than applied
at a lower ABI, so a host that reports a lower ABI under `best-effort`
runs unconfined and says so at every start.

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

Key commands update the users file offline. Reload the service to apply a
change:

```sh
sudo systemctl reload mellomting
```

`SIGHUP` reloads the key store in place, under every sandbox mode, without
severing in-flight streams. A reload that fails leaves the previous key store
in effect and logs why. Follow the apply instruction printed by the key
command.

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
 "tokens_in":120,"tokens_out":480,"tokens_total":600,"usage_status":"reported",
 "retry_count":0,"error_class":""}
```

`remote` is the client address after `server.trusted_proxies` is applied, and
`unix` for a Unix-socket client. `user_agent` is client-controlled: it is
truncated and stripped of control characters, but not otherwise trusted.
`usage_status` says whether the backend reported the token counts or they are
unknown. To read only the access log:

```sh
sudo journalctl -u mellomting -o cat | jq 'select(.msg == "request")'
```

With accounting enabled, `mellomting usage report` reports per-key token and
request totals. Use the supplied logrotate policy for the JSONL file; it uses
`copytruncate` because the sandbox retains access to the open file. Renaming
the file does not redirect the running writer to a replacement.

## Watching requests in flight

The access log records a request when it finishes. To see what the daemon is
doing right now, give it an admin socket:

```yaml
server:
  admin_socket: /run/mellomting/admin.sock
```

and restart it — the socket is bound at startup, before the sandbox. Then:

```sh
mellomting top
```

```
mellomting 14:02:11 — 2 in flight

AGE   PHASE      KEY    MODEL     TOKENS  IN    OUT   CLIENT          USER-AGENT
8.4s  streaming  ci     my-model  412     712B  4.0K  198.51.100.9    codex-cli/1.2.3
0.9s  waiting    alice  my-model  -       680B  -     198.51.100.14   curl/8.7.1
```

`PHASE` is where the request is: `reading` its body, `routing`, `queued` for a
backend, `waiting` for the first response byte, `streaming` events back, or
`sending` a buffered response. `TOKENS` stays `-` until the backend reports
usage, which for a stream is at the end.

`--interval` sets the refresh (default `1s`), and `--once` prints a single
snapshot without clearing the screen, for scripts and pipes.

The socket is created `0600`, so only the service account and root can read it.
It is deliberately not part of the ingress: it exposes every caller's address,
user agent and token use, which is not something the proxy's own clients should
see. Without `admin_socket` configured, no request tracking happens at all.

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
- Key changes not taking effect: reload the service when instructed by the
  key command.
- `mellomting top` cannot reach the daemon: check that `server.admin_socket` is
  set in the same configuration the daemon was started with, and that the
  daemon has been restarted since it was added.
