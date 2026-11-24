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

A decision model is declared the same way, `type: decision`, because the
server has no `/v1/models` for discovery to read. Adding a decision server
is this, and nothing else:

```yaml
servers:
  clef:
    url: http://127.0.0.1:8011

models:
  clef-flash:
    type: decision
    servers: [clef]
```

`policy.max_output_tokens` is refused on a decision model: it has no
generative output to cap. A server takes the decision timeouts when every
model routed to it is `type: decision`; a server shared with generation
models keeps the generation timeouts. An explicit `queue_timeout` above the
server's own request bound is refused, naming the server.

| Setting | Decision server | Generation server |
|---------|-----------------|-------------------|
| Request timeout | 30s | 30m |
| Header timeout | 30s | 300s |
| Queue wait default | 30s | 120s |
| Load report read by `top` | no | yes |

If a server requires authentication, add `api_key_file` to its entry, pointing
to a protected file containing its credential. Client credentials are stripped
before forwarding; server credentials come from configuration only.

A server that rejects a request answers with its own error message, such as
"maximum context length exceeded". Mellomting appends that message, bounded and
stripped of control characters, to its own sanitized one, so the client can
see what to fix. Only the message grows; the status and error code do not
change, and only the statuses that describe the client's request (400, 413,
422, 429) are relayed — a server error's text is the operator's business.

Turn it off for a server you do not trust with your clients, such as a
third-party gateway: its rate-limit message can name the account behind the
credential Mellomting sends it.

```yaml
servers:
  gateway:
    url: http://10.0.0.5:8000
    relay_upstream_errors: false
```

See [Operations](OPERATIONS.md) for how relays are logged.

With `server.admin_socket` set, the daemon reads each server's own load
report for `mellomting top`. A server that serves only decision models is
not read, since decision servers publish no report; `metrics: true` reads it
anyway. To stop reading any other server:

```yaml
servers:
  a:
    url: http://127.0.0.1:8000
    metrics: false
```

Each server admits a bounded number of requests at once and queues the rest.
Set the cap to what the engine runs concurrently — for vLLM, its
`--max-num-seqs`:

```yaml
servers:
  a:
    url: http://127.0.0.1:8000
    max_concurrency: 8   # requests forwarded at once, 1-1024
    queue_size: 32       # requests waiting for a slot, 0-4096; 0 never waits
    queue_timeout: 120s  # longest wait for a slot, at most 30m
```

The limits belong to the server, not the model: every model listed under one
server shares them, so two models on one vLLM instance forward at most
`max_concurrency` requests to it between them. With `queue_size: 0` a busy
server refuses at once, and a model with another replica falls back to it.

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

## Agents

An agent attaches an inference server the proxy cannot reach, for example a
GPU instance without inbound access, by dialing out to the proxy. The models
it advertises are registered automatically under its key's namespace; no
configuration change or restart is needed per model. Enable it with:

```yaml
agents:
  enabled: true
  max_attachments: 16          # concurrently attached agents, all keys
  max_attachments_per_key: 8   # one agent key's share
  max_concurrency: 8           # requests forwarded at once per agent
  queue_size: 32               # requests waiting for a slot, per agent
  queue_timeout: 120s          # longest wait for a slot
  max_models: 16               # models one agent may register; more refuses the attach
```

The values shown are the defaults; `config show-effective` prints what
applies.

With agents enabled, `servers` and `models` may be omitted, and the proxy
serves attached models only. Configured models always take precedence over
attached ones with the same name. See [Operations](OPERATIONS.md#agents) for
keys and setting up an agent host, and PLAN §104 for the design.

## Attach

`attach` makes the daemon the other end: an agent that dials an edge with
an agent key and serves the edge's requests from local inference servers.
`mellomting init --edge` writes it:

```yaml
attach:
  edge: https://edge.example.org
  key_file: /etc/mellomting/agent.key
  servers: ["http://127.0.0.1:8000"]
  # backend_key_file: /etc/mellomting/backend.key   # bearer token for the servers
  # ca_file: /etc/mellomting/edge-ca.pem            # CAs for the edge (default: system roots)
  # insecure_edge: true                             # allow plain http off loopback
  # metrics: false                                  # refuse the edge's reads of the servers' load
```

A daemon that attaches serves no clients, so a configuration with `attach`
may set only `logging`, `shutdown`, and the sandbox modes under `security`;
any other section is refused. `shutdown.grace_period` is the longest a
restart waits for in-flight streams.

## Security and defaults

Backend connections default to loopback addresses. Remote servers need an
explicit network policy; `init` derives a restricted policy for supplied
remote IP addresses. Keep credentials in protected files, and keep the
generated absolute `auth.users_file` and `auth.pepper_file` paths.

The sandbox uses Landlock on Linux and Seatbelt on macOS. It defaults to
`best-effort`: apply when available and warn otherwise. `required` refuses
startup without it; `disabled` skips it. Run
`mellomting sandbox check --config ./config.yaml` to inspect capability and
configured policy. A backend URL with a hostname instead of an IP literal
adds name resolution to the sandbox policy; the `sandbox enforced` log line
lists it. See [Hardening](../HARDENING.md).

| Setting | Default |
|---------|---------|
| Logging | JSON at `info` |
| Accounting | Disabled |
| Global rate | 100 requests/s, burst 200 |
| Request body / response size | 16 MiB / 64 MiB |
| Global in-flight requests | 64 |
| Backend timeouts | Connect 3s, headers 300s, request 30m, stream idle 120s (headers/request 30s/30s on a decision-only server) |
| Server admission | 8 in flight, queue 32, queue wait 120s |
| Retry attempts | 1 |

Backend timeouts assume a local inference server and have no configuration
key. Generation servers get the defaults above; a server whose models are
all `type: decision` gets the decision bounds from the table in
[Servers and models](#servers-and-models) instead. A large request waits
tens of seconds on scheduler admission and prefill before its first
response byte, so the header bound covers that wait instead of reporting it
as a backend failure. `config show-effective` does not show timeouts; the
daemon logs each backend's bounds in a `backend admission` line at startup.

Whichever in-flight limit is lowest decides when work starts queueing. A
key's own `concurrent_requests` is 8 by default, the same as a server's
default in-flight cap, so a single key is refused with `429` at the per-key gate
rather than queued; raise `concurrent_requests` for a deployment where one
key should be able to queue behind itself. Above that point, excess work
waits in Mellomting's queue, where `queue_size` bounds it and a client
disconnect drains it, rather than in the inference server's own queue, where
neither is true. A queued request keeps its global and per-key in-flight
slots until it is admitted or its client hangs up, so the queue wait is also
how long one stalled server can hold them — size `max_inflight_requests`
against that.

Use `mellomting config show-effective --config ./config.yaml` to inspect all
effective settings. The [design reference](PLAN.md) describes the complete
schema and policy semantics.
