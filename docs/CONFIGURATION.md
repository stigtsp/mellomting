# Configuration

Start with `mellomting init --server http://127.0.0.1:8000 --config ./config.yaml`.
It discovers models once; changes at the inference server do not automatically
change the proxy's public models. Edit the generated file to update them.
Use `--dry-run` to print the configuration without creating files. Creating
files with `init` currently requires Linux.

Examples here assume an installed binary. For a local build, use
`bin/mellomting`. YAML examples are additions or edits to the generated file.
Run `mellomting config check --config ./config.yaml` after editing, then restart
the proxy to apply configuration changes. Unknown fields, duplicate keys,
YAML anchors, and multiple YAML documents are rejected.

## Servers and models

Server names are local labels. Model names are the names clients use.
To discover several servers during setup, repeat `--server`:

```sh
mellomting init --config ./config.yaml \
  --server a=http://127.0.0.1:8000 \
  --server b=http://127.0.0.1:8001
```

For a model served by both servers:

```yaml
servers:
  a:
    url: http://127.0.0.1:8000
  b:
    url: http://127.0.0.1:8001
models:
  my-model:
    servers: [a, b]
```

With several servers, the default strategy is `least-inflight`.
To expose a different public name, set `upstream_model`:

```yaml
models:
  chat:
    upstream_model: my-model
    servers: [a, b]
    policy:
      max_output_tokens: 32768
```

The output cap rejects higher client limits and supplies a limit when absent.
It never raises a client-supplied limit. For embedding models, set
`type: embedding`; discovery defaults to `type: generation`.

If a server requires authentication, add `api_key_file` to its entry, pointing
to a protected file containing its credential. Client credentials are stripped
before forwarding; server credentials come from configuration only.

A server that rejects a request answers with its own error message, which
Mellomting replaces with a sanitized one. To pass that message through to the
client as well:

```yaml
servers:
  a:
    url: http://127.0.0.1:8000
    relay_upstream_errors: true
```

Only the message grows; the status and error code do not change. Leave it off
unless the server and every client routed to it are equally trusted, because a
rejection message can quote the request that caused it. See [Operations](OPERATIONS.md) for the operator-only alternative.

## Listener and TLS

Local `init` defaults to `127.0.0.1:8080`. Use `--listen` during initialization
to select another IP address and port, or an absolute Unix socket path.
Choosing a non-loopback TCP address explicitly enables plaintext access on
that address; configure TLS before exposing it to an untrusted network.

To enable TLS on a TCP listener:

```yaml
server:
  tls:
    cert_file: /etc/mellomting/tls/cert.pem
    key_file: /etc/mellomting/tls/key.pem
```

Both files are required. TLS requires version 1.2 or newer. Restart after
replacing a certificate. TLS cannot be configured on a Unix listener.

To enable `mellomting top`, configure an admin socket:

```yaml
server:
  admin_socket: /run/mellomting/admin.sock
```

Use an absolute path different from the client listener. The socket has mode
`0600`; restart after adding it. See
[Operations](OPERATIONS.md).

## Accounting and limits

To record backend-reported token usage:

```yaml
accounting:
  enabled: true
  path: /var/log/mellomting/usage.jsonl
```

The directory must exist and be writable by the service user. The systemd
installer prepares this directory. See [Operations](OPERATIONS.md) for reports
and rotation. Missing backend usage is recorded as unknown, not an exact count.

Optional global rate limits and retries:

```yaml
limits:
  global_requests_per_second: 100
  global_burst: 200
retry:
  max_attempts: 3
```

Retries are disabled by default. Only eligible failures are retried, and no
request is retried after response bytes reach the client. Fallback to another
replica happens when a backend refuses admission, so it waits out the queue
first: with several replicas and retries enabled, a request that has taken a
queue slot on a busy backend waits there rather than moving to an idle one.

Per-key limits are set when creating a key:

```sh
mellomting key create ci --config ./config.yaml --models my-model \
  --requests-per-second 10 --burst 20 --concurrent-requests 4
```

## Security and defaults

Backend connections default to loopback addresses. Remote servers need an
explicit network policy; `init` derives a restricted policy for supplied
remote IP addresses. Keep credentials in protected files, and keep the
generated absolute `auth.users_file` and `auth.pepper_file` paths.

The sandbox uses Landlock on Linux and Seatbelt on macOS. It defaults to
`best-effort`: apply when available and warn otherwise. `required` refuses
startup without it; `disabled` skips it. Run
`mellomting sandbox check --config ./config.yaml` to inspect capability and
configured policy. See [Hardening](../HARDENING.md).

| Setting | Default |
|---------|---------|
| Logging | JSON at `info` |
| Accounting | Disabled |
| Global rate | 100 requests/s, burst 200 |
| Request body / response size | 16 MiB / 64 MiB |
| Global in-flight requests | 64 |
| Backend timeouts | Connect 3s, headers 300s, request 30m, stream idle 120s |
| Backend admission | 8 in flight, queue 32, queue wait 600s |
| Retry attempts | 1 |

Backend timeouts and admission are static: they assume a local inference
server and are not per-server settings. A large request waits tens of
seconds on scheduler admission and prefill before its first response byte,
so the header bound covers that wait instead of reporting it as a backend
failure. Work beyond `max_concurrency` waits in Mellomting's queue, where
`queue_size` bounds it and a client disconnect drains it, rather than in the
inference server's own queue, where neither is true. A queued request keeps
its global and per-key in-flight slots until it is admitted or its client
hangs up, so the queue wait is also how long one stalled server can hold
them.

Use `mellomting config show-effective --config ./config.yaml` to inspect all
effective settings. The [design reference](PLAN.md) describes the complete
schema and policy semantics.
