# Hardening

## Authentication and keys

- API keys have 256-bit random secrets. Clients send them in
  `Authorization: Bearer` or `X-Api-Key`, never query parameters.
- The users file stores HMAC-SHA-256 hashes. The pepper is stored separately.
  Key comparisons use constant-time checks.
- Manage keys with `mellomting key`. Valid users-file changes apply
  automatically within about a second, without interrupting active requests.
- Protect secret files with mode `0600`, or `0640` and the service group.
  The systemd installer creates its auth files as `root:mellomting`, mode
  `0640`.

## Requests and configuration

- Only the supported inference endpoints are forwarded. Backend admin paths,
  including `/metrics`, are inaccessible through the proxy.
- Client credentials and proxy identity headers are stripped before forwarding.
  Backend credentials come from configured files.
- Requests, responses, headers, SSE events, connections, concurrency, queues,
  and retries have size or count limits. Requests are never retried after
  response bytes reach the client.
- Unknown request fields pass through. Routing and policy fields are checked
  before forwarding.
- Configuration files are size-limited and strictly validated. Unknown fields,
  duplicate keys, YAML aliases and anchors, and unsupported versions are rejected.
- Backend URLs cannot contain credentials, paths other than `/`, queries, or
  fragments. Resolved addresses are checked against `security.backend_network`
  on every new connection.
- Non-loopback plaintext TCP requires
  `allow_plaintext_non_loopback: true` and produces a startup warning.

## TLS

Static TLS requires `server.tls.cert_file` and `server.tls.key_file` on a TCP
listener. TLS 1.2 is the minimum. Restart after replacing either file.

Certificates and keys are loaded before sandbox activation. They must be
regular files no larger than 1 MiB; the final path component cannot be a
symlink. Private keys must have restricted permissions.

Automatic certificate issuance (ACME) is not supported. A trusted reverse
proxy can terminate TLS and forward to a Unix socket or loopback listener.

## Sandbox

Linux uses Landlock; macOS uses Seatbelt. Both default to `best-effort`:
apply the full policy when available, otherwise log a warning and continue.
Use `required` to refuse startup without confinement, or `disabled` to skip it.

Check what the configured mode would do:

```sh
mellomting sandbox check
```

The sandbox is applied after startup files are loaded and before requests
are accepted. It restricts the daemon to the resources it still needs:

- Read the directory containing the users file, allowing reloads after atomic
  file replacement. Put that file in a separate directory to narrow access.
- Write the accounting log when enabled.
- Connect to configured backend TCP ports.
- Accept requests on listeners opened at startup.

No execution rights are granted. Landlock also restricts signals and abstract
Unix sockets to processes outside its domain. TCP rules restrict ports;
backend address checks are enforced separately by the proxy.

Landlock requires ABI 6 by default. The highest ABI supported by both kernel
and library is used. Enforcement covers all runtime threads, including threads
created later. Multipath TCP is disabled on the proxy's listeners and dialers.

Seatbelt may be unavailable when the process is already inside another
sandbox. In `required` mode, this prevents startup.

The sandbox limits the impact of a compromised proxy. It cannot protect
resources the proxy is allowed to access. See [Threat model](THREAT_MODEL.md).

## Logs and operator access

Operational logs are structured JSON. Prompts, responses, API keys, backend
credentials, and raw backend errors are not logged. Client errors use the
OpenAI JSON error format without stack traces, paths, or backend hostnames.

The optional admin socket exposes active request metadata, including client
addresses and token usage. Its mode is `0600`; only the service account and
root can read it.

## Deployment and builds

Use the supplied systemd unit to run the proxy as an unprivileged service.
Keep inference servers under separate unprivileged users and disable their
admin and development endpoints.

The unit sets no `IPAddressDeny`/`IPAddressAllow` filter: it cannot see
`security.backend_network`, and its denial is a silent packet drop that makes a
blocked backend look like an upstream outage. Egress is enforced at dial time
instead. To add an address filter anyway, use `systemctl edit mellomting`
rather than editing the packaged unit.

Release builds use `CGO_ENABLED=0` and `-trimpath`, with version and commit
metadata embedded. Primary targets are `linux/amd64` and `linux/arm64`.

Run `make check` for formatting, build, vet, tests, race checks, staticcheck,
and govulncheck when installed. See [Operations](docs/OPERATIONS.md) for setup
and [the design](docs/PLAN.md) for the full security requirements.
