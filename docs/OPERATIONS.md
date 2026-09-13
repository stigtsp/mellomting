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
 "retry_count":0,"error_class":"ok"}
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

To watch active requests, configure an admin socket:

```yaml
server:
  admin_socket: /run/mellomting/admin.sock
```

Restart the daemon, then run:

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

The socket has mode `0600`, restricting access to the service account and
root. It exposes client addresses, user agents, and token usage. Request
tracking is disabled when no admin socket is configured.

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
