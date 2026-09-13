# Mellomting

Mellomting is a small OpenAI-compatible proxy for inference servers such as
vLLM. It manages API keys, routes requests, and applies limits. It runs as one
binary, without a database or telemetry.

## Quick start

You need Linux, Go, and a running inference server. This example uses a server
at `127.0.0.1:8000`. On other platforms, `init --dry-run` previews the
configuration without creating files.

Build and generate a local configuration:

```sh
make build
bin/mellomting init --server http://127.0.0.1:8000 --config ./config.yaml
```

`init` discovers the server's models and creates `config.yaml`, `users.yaml`,
and `auth.pepper`. It refuses to overwrite existing files.

Create a key and save it securely; it is shown only once:

```sh
bin/mellomting key create local --config ./config.yaml
```

The key allows all models. Add `--models MODEL` to restrict it to a name from
the discovery output. Then start the proxy:

```sh
bin/mellomting serve --config ./config.yaml
```

In another terminal, replace `YOUR_API_KEY` with the key you saved:

```sh
curl http://127.0.0.1:8080/v1/models \
  -H 'Authorization: Bearer YOUR_API_KEY'
```

Use `http://127.0.0.1:8080/v1` as the base URL in your OpenAI-compatible client,
with that key and a model returned by `/v1/models`. Streaming is supported.

Keep `--config ./config.yaml` when using these local files. Commands that read
configuration otherwise use `/etc/mellomting/config.yaml`.

## Install as a service

On Linux with systemd, run from the repository after building:

```sh
sudo bin/mellomting install --systemd
sudo editor /etc/mellomting/config.yaml
sudo /usr/local/bin/mellomting config check
sudo /usr/local/bin/mellomting key create production
sudo systemctl enable --now mellomting
```

Fill in the server and model entries before checking the configuration.
Use `--models MODEL` to restrict a key's model access.
The service uses a Unix socket at `/run/mellomting/mellomting.sock` by default.

The installer prepares the service account, configuration, auth files, unit,
and log rotation. Existing configuration and auth files are preserved.
See [Operations](docs/OPERATIONS.md) for installation options and maintenance.

Alternatively, `make deb` builds a Debian package that installs the binary,
unit, service account, and directories. `init` then creates the configuration
and auth files in place:

```sh
sudo apt install ./dist/mellomting_<version>_<arch>.deb
sudo mellomting init --server http://127.0.0.1:8000 --config /etc/mellomting/config.yaml
sudo mellomting key create production
sudo systemctl enable --now mellomting
```

## Configuration

The sandbox defaults to `best-effort`: Mellomting applies it when available
and warns otherwise. Use `--sandbox required` with `init` to require it.
See [Hardening](HARDENING.md) for platform support and deployment controls.

The generated configuration lists inference servers and the models they serve:

```yaml
servers:
  local:
    url: http://127.0.0.1:8000
models:
  my-model:
    servers: [local]
```

This is a fragment of the generated file. See [Configuration](docs/CONFIGURATION.md)
for multiple servers, aliases, TLS, accounting, and limits.

To validate your local file or inspect its defaults:

```sh
bin/mellomting config check --config ./config.yaml
bin/mellomting config show-effective --config ./config.yaml
```

## Key-file updates

Valid key additions, removals, and policy changes apply automatically within
about a second. Invalid updates leave the previous store active; existing
requests continue. SIGHUP (or `systemctl reload mellomting`) triggers an
immediate check. Other configuration changes require a restart.

## Further reading

- [Operations](docs/OPERATIONS.md): keys, installation, logs, and troubleshooting.
- [API compatibility](docs/COMPATIBILITY.md): endpoints, streaming, and usage accounting.
- [Hardening](HARDENING.md) and [threat model](THREAT_MODEL.md): security controls and boundaries.
- [Security reporting](SECURITY.md): how to report a vulnerability.

## Development

```sh
make check
```

This runs formatting, build, vet, tests, and race checks, plus staticcheck and
govulncheck when installed. `make release` builds release artifacts; pushing a
release tag publishes them as a GitHub release (docs/OPERATIONS.md "Releases").
See [the design](docs/PLAN.md) and [contributor guidance](AGENTS.md).
