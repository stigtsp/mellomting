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

Both default to `required` and take `best-effort` (warn and continue) or
`disabled` instead. A configuration may carry both sections and be
served on either platform.

```sh
mellomting sandbox check
```

reports the backend for this host, whether it is available, and what the
configured mode would do. Under `required` a host that cannot enforce
the policy does not start the daemon — including a macOS host that is
already running it inside another sandbox, which may not apply a second
profile.

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

Key commands update the users file offline. With the default required
Landlock sandbox, restart the service after a change:

```sh
sudo systemctl restart mellomting
```

The sandbox pins the users file to its startup inode, so replacing that file
prevents reload. A process running without that restriction can reload it on
`SIGHUP`. Follow the apply instruction printed by the key command.

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

With accounting enabled, `mellomting usage report` reports per-key token and
request totals. Use the supplied logrotate policy for the JSONL file; it uses
`copytruncate` because the sandbox retains access to the open file. Renaming
the file does not redirect the running writer to a replacement.

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
- Key changes not taking effect: restart the service when instructed by the
  key command.
