# `mellomting top`: visibility redesign

## Why

During a load test the operator watched `top` show 40 requests `waiting`
and could not see why client concurrency stayed low. The cause was the
per-key concurrency limit: requests it refused with 429 never entered the
in-flight list, so the screen showed a calm table while the client was
being held back. The fix (raise `concurrent_requests` in `users.yaml`,
SIGHUP) was found by guessing.

A second blind spot showed in the same screenshot: the edge had sent 24
requests to one agent, all `waiting`, and nothing said whether vLLM behind
it was running them or queueing them.

`top` must answer **"why is this slow or held back, and where?"** at a
glance: throughput, what is refused or queued and by which limit, and what
the inference servers themselves report.

## Goals

- An interactive, full-screen `top` with btop-style boxed panels, braille
  graphs, colour gradients for load, arrow-key selection and a detail box.
- Aggregate views (per key, per model, per backend) and a detailed view
  (individual requests), with a detail box for the selected row.
- Throughput history: requests, tokens, bytes, errors and refusals over the
  last five minutes, as graphs.
- Refusals made visible with their reason, attributed to the key (or the
  global limit) that caused them.
- Limits shown next to usage: a key's in-flight against its
  `concurrent_requests`, its rate against its `requests_per_second`, a
  backend's admitted against `max_concurrency`.
- Backend metrics from vLLM's `/metrics`, read only when a server offers
  them, for configured servers (read by the edge) and agents (read by the
  agent, relayed to the edge).

## Non-goals

- `top` never changes anything. The admin socket stays read-only. No
  editing limits, revoking keys or detaching agents from `top`.
- No mouse, themes, settings menu or saved layout.
- No history beyond the daemon's in-memory window; no persistence across
  restarts.
- No metrics export (Prometheus endpoint, push), no web dashboard
  (PLAN §3, §90).
- No `top` on the agent host itself; agent-side numbers surface on the
  edge.

## Anti-goals (failure even if it works)

- Any prompt, response, tool argument, API key or backend credential in
  the admin data.
- Admin data reachable from the ingress, or `/metrics` reachable by
  clients through the proxy (PLAN §11.3).
- Unbounded memory from per-key, per-model or per-backend series, or from
  an agent's or backend's metrics page.
- Measurable cost on the request path: recording a sample is a few
  counter updates under a short lock, never allocation per request beyond
  what exists today, never I/O.
- A terminal left in raw mode or the alternate screen after `top` exits,
  crashes or is killed by SIGTERM/SIGINT.
- An existing configuration or users file that stops loading.

## Quality bar

`top` is a high-quality TUI, on a par with btop, not a table that
redraws. Concretely:

- No flicker or tearing: diff-based redraw, one write per frame, cursor
  hidden, synchronized-update escape (`CSI ? 2026 h/l`) where supported.
- Input feels immediate: key presses redraw at once, independent of the
  1 s data poll; holding an arrow key scrolls smoothly.
- Alignment never breaks: widths are measured in terminal cells, not
  bytes or runes; client-supplied text (user agents, key and model names)
  is sanitized of control characters and truncated by cell width.
- Resizing re-lays out immediately and never leaves debris.
- Graphs have labelled scales, smooth braille rendering and a consistent
  colour gradient; meters use the same gradient everywhere.
- A lost daemon connection shows a banner and keeps retrying, rather than
  exiting and dumping the user back to the shell mid-incident.
- Every keybinding is visible in the bottom border.

## Constraints

- Go standard library plus already-approved `golang.org/x/sys` (raw
  terminal mode via termios). No TUI framework (PLAN §88).
- Everything stays behind `server.admin_socket` (0600, bound before the
  sandbox). Without it nothing is tracked, as today.
- Metrics polling must fit the sandbox: the edge already may connect to
  every configured backend's port, and an agent to its servers' ports.
- Old clients keep working: `GET /inflight` keeps its shape, so an old
  `top` binary against a new daemon still works. A new `top` against an
  old daemon says the daemon is too old for the full view and falls back
  to the plain table.
- Agents and edges upgrade independently. An edge polling an old agent
  gets 404 and shows no backend metrics for it; an old edge never polls.

## Design

### 1. Daemon: rolling statistics (`internal/stats`)

A new package keeps per-second buckets for the last 300 seconds in a ring,
for a bounded set of series:

- one global series;
- one per key (by key ID; name carried for display);
- one per public model;
- one per backend (configured backend names and agent attachment names).

Each dimension holds at most 256 series; beyond that, new names fold into
an `(other)` series, so a churn of agent attachments or model names cannot
grow memory. Series idle for the whole window are dropped.

Each bucket counts:

- completed requests by status class (2xx, 4xx, 5xx);
- errors by `error_class` (the existing sanitized classes);
- refusals by reason (below);
- bytes in, bytes out;
- tokens in, tokens out (backend-reported usage, as accounted today);
- sum and count of time to first byte, and of total duration, so the view
  can show means; plus a fixed log-scale histogram of duration (12
  buckets) for p50/p95.

Samples are recorded:

- at the end of `proxy.dispatch`, from the same `result` the `request` log
  line uses (one call, global + key + model + backend series);
- at every refusal before the proxy, in `httpapi.routeBody`.

Refusal reasons, each a fixed string:

| reason | where | attributed to |
|---|---|---|
| `global_inflight` | global in-flight cap (503) | global |
| `source_rate` | pre-auth per-source bucket (429) | global |
| `global_rate` | global request rate (429) | global |
| `auth` | bad or missing key (401) | global |
| `key_rate` | per-key `requests_per_second` (429) | key |
| `key_concurrency` | per-key `concurrent_requests` (429) | key |
| `quota` | token quota (429) | key, model |
| `backend_queue` | backend queue full or queue timeout | key, model, backend |
| `draining` | daemon draining (503) | key |
| `body_budget` | request-body memory budget exhausted (503) | key |

A per-key refusal also counts in the global series.

### 2. Daemon: limits and live state

The admin snapshot adds, per key that has a series or is in flight:
configured `concurrent_requests`, `requests_per_second`, `burst` (from the
key record) and current in-flight (from the limiter's per-key state; add a
read accessor). Globally: `max_inflight_requests` and current in-flight,
global rate and burst. Per backend: admitted, queued, `max_concurrency`,
`queue_size`, passive-health state (cooldown until).

`inflight.Record` gains `phase_ms` (time in the current phase) and
`ttfb_ms` (time to first response byte, 0 until then), so the request
detail can say "queued 0.2s, waiting 2m31s".

### 3. Backend metrics (`internal/backendmetrics`)

A bounded reader for vLLM's Prometheus text format. It keeps only an
allow-list, summed across label sets:

- `vllm:num_requests_running`
- `vllm:num_requests_waiting`
- `vllm:kv_cache_usage_perc` (falls back to `vllm:gpu_cache_usage_perc`)
- `vllm:prompt_tokens_total`, `vllm:generation_tokens_total` (counters;
  rates derived from successive reads)
- `vllm:num_preemptions_total`

Bounds: 1 MiB body, 2 s timeout, lines over 4 KiB skipped, unknown names
ignored without allocation, non-finite or negative values dropped.

**Configured servers.** The edge polls `GET <base_url>/metrics` every 5 s
per server (one poll per server, not per model), sending the server's
configured credential if it has one. A 404, a non-Prometheus body, or a
page with none of the allow-listed names marks the server "no metrics" and
backs off to one retry every 5 minutes; nothing is logged above debug.
This covers another Mellomting as a backend (404) and non-vLLM servers.
A per-server `metrics: false` turns polling off. The poller starts only
when `server.admin_socket` is set: no one can see the result otherwise.

**Agents.** The agent polls each of its local servers the same way and
serves the latest summary on its reversed HTTP/2 server at
`GET /mellomting/backend-metrics` (JSON, fixed fields), handled by the
agent itself and never relayed to the local server. The edge polls it
every 5 s per attachment, outside admission and outside the attachment's
request and failure counters, only while an admin socket is configured.
The edge treats the reply as untrusted: 16 KiB cap, strict decode, every
number clamped to a sane range, missing fields absent. An old agent
answers 404: no metrics.

### 3a. Accounting

Two sources, for two horizons:

- **Live quota state, from the daemon.** The admin snapshot adds, per key
  with token windows configured, `tokens_per_hour`/`tokens_per_day`, the
  tokens settled in the current UTC hour and day, and when each window
  resets (read accessor on `accounting.Quota`; no new state). The Keys
  view shows them as meters beside the concurrency and rate meters, so a
  key held back by quota reads the same way as one held back by
  concurrency. Also: accounting enabled or not, records dropped by the
  writer (`Writer.Dropped`), so a disk stall shows as a banner.
- **History, from `usage.jsonl`, read by `top` itself** as
  `mellomting usage report` does: a sixth view, **Usage**, with requests,
  input/output/cached tokens and unknown-usage counts per key, model and
  backend over 1 h, 24 h and 7 d, plus an hourly bar chart for the
  selected item. `top` reads `accounting.path` from the configuration,
  scans `usage.jsonl` and an uncompressed `usage.jsonl.1` if present
  (bounded: the last 7 days, at most 512 MiB scanned, the existing
  bounded line scanner), then tails the file every 10 s from the last
  offset, restarting when the file shrinks or its inode changes
  (rotation). The view states the time range it actually covers. Key IDs
  are shown by name using the admin snapshot's key list, falling back to
  the ID. If the file is unreadable (`top` run without permission) or
  accounting is off, the view says so; the other views are unaffected.
  The daemon is not involved: no new admin data, no change to the
  writer.

### 4. Admin API

New route `GET /stats?since=<seq>` beside `GET /inflight` (unchanged):

```json
{
  "seq": 12345,                  // newest bucket sequence number
  "now_ms": ..., "bucket_ms": 1000,
  "global": {...limits and live counts...},
  "series": [ {"dim":"key","id":"...","name":"...","buckets":[{"seq":...,...}]} ],
  "keys": [...], "backends": [...], "agents": [...], "requests": [...]
}
```

`since=0` returns the whole window; later polls send the last `seq` seen
and get only newer buckets, so a 1 Hz poll stays small. `top` keeps the
history it has received. The response is capped (4 MiB, already the
client's read limit).

### 5. `top` (`internal/topview`, `cmd/mellomting/top.go`)

**Terminal handling.** Raw mode via termios (`golang.org/x/sys/unix`;
Linux and macOS ioctls differ, so build-tagged), alternate screen, hidden
cursor; restored on every exit path including panic, SIGINT and SIGTERM;
redraw on SIGWINCH. Frames are drawn into a buffer and written once, with
only changed lines rewritten, to avoid flicker. Minimum size 80×24; below
it, a "terminal too small" message.

**Non-interactive.** When stdout is not a terminal, or with `--once`,
`top` prints plain text without escape sequences, as today, extended with
the summary line. `NO_COLOR` disables colour.

**Layout** (adapts to the terminal):

```
┌ mellomting ─ edge ─ 13:23:34 ─ up 17h09m ───────── 1 Overview 2 Keys 3 Models 4 Backends 5 Requests ┐
│ req/s  ⣀⣠⣤⣴⣶⣾⣿⣷  42/s   tok out/s ⣀⣤⣶⣿⣿⣷  3.1k/s   in  12 MB/s  out 0.4 MB/s           │
│ errors ⠀⠀⠀⣀⠀⠀⠀   0.2/s  refused   ⠀⠀⣠⣾⣿⣷  9/s ← key_concurrency                        │
├ Keys ─ sort: refused ▾ ─ filter: ─────────────────────────────────────────────────────────────────┤
│ KEY                  INFLIGHT  RATE      REFUSED/min           TOK/S  P50    LAST 5m             │
│▶coding_agents   ████ 8/8  3.1/10    212 key_concurrency   1.9k   41s    ⣀⣤⣶⣿⣿⣷             │
│ doc_indexer     ███░ 7/8  0.4/10      0                   0.6k   38s    ⣀⣀⣤⣀⣀⣤             │
├ coding_agents ──────────────────────────────────────────────────────────────────────────────┤
│ concurrent_requests 8 (at limit 97% of last 5m) · requests_per_second 10 · 212 refused/min        │
│ models: chat-large (17), chat-fast (6) · backends: agent-…-6, agent-…-5     │
└ ↑↓ select  ⏎ detail  ←→/1-5 view  s sort  / filter  r redact  p pause  q quit ────────────────────────────┘
```

- **Header box:** braille area graphs (2×4 dots per cell) for requests/s,
  tokens out/s, bytes in/out, errors/s and refusals/s, each with its
  current value and scale; the largest current refusal reason is named.
- **Overview is the primary view**, the one `top` opens on: a dashboard
  that gives the gist of what is happening in one glance, without
  switching views. Top to bottom:
  - the header graphs (offered vs completed req/s, tokens out/s, bytes,
    errors, refusals);
  - an **attention** box: one line per thing currently holding traffic
    back or going wrong, worst first, each naming the limit and the item,
    such as `coding_agents refused 212/min by concurrent_requests (8)`,
    `agent-…-6 vLLM queueing 18 (6 running, KV 94%)`,
    `gpu1 in cooldown 40s (backend_connect)`,
    `accounting dropping records`. Empty, it says `nothing held back`;
  - side by side: the busiest keys, servers and models (five each), each
    with its meter and sparkline;
  - a one-line live summary: requests by phase, oldest request's age.
  Selecting any item on the dashboard and pressing Enter opens its detail
  box, as in the other views.
- **Keys / Models / Backends:** one row per item with live and limit
  meters (green → yellow → red as usage approaches the limit), rates,
  refusals with dominant reason, p50 duration and a sparkline.
  Backends lists configured servers and agents together, with a kind
  column, edge admission (`SLOTS`, `QUEUE`) beside backend metrics
  (`vLLM 6 run / 18 wait · KV 94%`) when available.
- **Requests:** today's table plus phase time and TTFB.
- **Detail box** (Enter; Esc closes): the item's limits and live values,
  refusals by reason and errors by class for the last 5 minutes, graphs
  for its own series, and the items it relates to (a key's models and
  backends; a backend's models and keys; a request's phase timeline).
- **Keys:** `↑↓`/`j k` select, `PgUp PgDn Home End`, `Enter` detail,
  `Esc` close, `←→`/`Tab`/`1-5` view, `s` cycle sort, `/` filter by
  substring, `r` redact addresses, `p` pause updates, `q`/`Ctrl-C` quit.
- Selection follows the item (by id), not the row index, across refreshes.
- **Address redaction**, for screenshots and screen sharing: `r` toggles
  it, `--redact` starts with it on (also for `--once` and piped output).
  Redacted, every IP address (client addresses, agent remotes, and any
  IP literal inside other displayed text) is replaced by a stable label
  for the session (`ip-1`, `ip-2`, …), so rows from one client still
  group together without revealing the address. The header shows
  `REDACTED` while it is on. Redaction happens in `top`, after the data
  arrives; the daemon's data is unchanged.

### 6. Configuration and docs

- `servers.<name>.metrics: false` (optional; default on). Old configs are
  unchanged.
- PLAN §43.2 extended (stats, refusals, limits, backend metrics), §104
  (agent metrics path; edge polling outside admission), §11.3 (the edge
  reading `/metrics` itself is not forwarding it).
- OPERATIONS.md `top` section rewritten; CONFIGURATION.md documents
  `metrics`.

## Left to the builder

- Exact colours (256-colour palette with a 16-colour fallback), box
  characters, column widths and how the layout degrades at small sizes.
- Histogram bucket edges, sort orders per view, the Overview's exact
  composition.
- How `internal/topview` is split into files, and test strategy for the
  renderer (golden frames at fixed sizes are expected).
- Poll jitter and the exact backoff for servers without metrics.

## Decisions after review

Settled after a builder review of this document; each is the
implementer's call, not the operator's, unless marked.

1. **The backend dimension is the server**, not the synthetic
   `auto-<server>-<hex>` backend. Admission, `/metrics` and `metrics:
   false` are per server, so the Backends view has one row per configured
   server (and one per agent attachment), with SLOTS and the vLLM reading
   once. Passive health, which is per synthetic backend, is listed per
   model in the detail box. Stats map `result.backend` to its server via
   `config.Backend.Server`; an attachment is its own server.
2. **Agent metrics are on demand, with an agent-side opt-out.** The agent
   reads its server's `/metrics` only when the edge asks
   (`GET /mellomting/backend-metrics`), caching the reading for 2 s, so an
   idle edge costs the agent nothing. `attach.metrics: false` (new
   optional key, default on) makes the agent answer 404, for an agent
   operator who does not want the edge to see their load (PLAN §104.7).
   `/metrics` is read at the server URL's root; attach server URLs have
   an empty path.
3. **The daemon keeps gauge history.** A sampler goroutine, off the
   request path, records once per second per series: in-flight, and for
   keys whether in-flight was at `concurrent_requests`; for servers,
   admitted and queued; for metric-reporting servers, vLLM running,
   waiting and KV use. So "at limit 97% of the last 5 minutes" and the
   backend graphs have history the moment `top` opens.
4. **Bounded memory and replies.** At most 64 series per dimension
   (key, model, server), `(other)` beyond. Buckets are compact fixed
   structs (32-bit counters); error classes and refusal reasons are stored
   per series only as totals over the window plus the dominant one, while
   the global series keeps every class per bucket. Worst case is a few MB.
   JSON omits zero fields and empty buckets. If a `since=0` reply would
   pass 3 MiB, the least busy series lose their buckets first (totals
   kept) and the reply says `"truncated": true`; `top` shows that.
5. **Usage view.** `top` also reads rotated `usage.jsonl.N.gz` files
   (stdlib `compress/gzip`, same 512 MiB total bound), matching the
   shipped daily `compress`/`delaycompress` rotation, so 7 d covers 7 days
   on a default install. The admin snapshot lists every key in the store
   (ID and name only), so history names resolve. The Usage aggregator is
   new code: per hour × (key, model, server). Agent attachment names
   (`agent-<key>-<n>`) fold to `agent-<key>`, since `<n>` changes on every
   reconnect.
6. **Refusals are their own category.** A refusal is terminal: the
   request ended with 429/503 because of one of the listed limits. It is
   not counted as an error or as a completed request. req/s counts
   completed requests; a separate "offered" rate counts completed plus
   refused, and the gap between them is what the operator was missing.
   `quota`, `backend_queue` and `draining`, which end inside dispatch, are
   classified there from `error_class`. A queue refusal that falls back to
   another replica and succeeds is not a refusal (it shows as a fallback
   count in the server's detail).
7. **Tokens are what was charged.** Stats and the Usage view use the
   accounted (charged) total. For trusted backends input/output are as
   reported. For untrusted agents the reported split is scaled to the
   charged total, so a lying agent cannot inflate the graphs beyond what
   accounting bounded.
8. **Redaction rewrites every IP literal in displayed text** (IPv4 and
   IPv6, including inside user agents and names), with stable `ip-N`
   labels. Backend URLs are not in the admin data and are not shown, so
   there is nothing more to redact there. Hostnames are left visible.

9. **Engines: vLLM, SGLang and Ollama** (operator's request). vLLM and
   SGLang publish Prometheus `/metrics` under their own names (SGLang only
   with `--enable-metrics`); both map to running, waiting, cache use and
   token totals. Ollama has no `/metrics`; its `/api/ps` gives the loaded
   models, their VRAM and when each unloads, and no queue depth, so an
   Ollama row shows loaded models instead of running/waiting.
