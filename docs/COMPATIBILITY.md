# Mellomting — API compatibility notes

This file records Mellomting's compatibility behaviour with the OpenAI
and vLLM APIs, so operators and client authors know exactly what the
proxy changes and what it passes through unchanged (PLAN §12, §30, §36,
§38).

The source of truth for intended behaviour is `PLAN.md`. Where an
upstream API has changed since PLAN.md was written, this file is updated
to record the observed/expected compatibility contract (PLAN §30).

## General strategy: shallow parse, broad pass-through (PLAN §12)

Mellomting decodes the top-level request object into
`map[string]json.RawMessage` and inspects only the routing/policy fields:

```text
model
stream
stream_options
previous_response_id
max_tokens
max_completion_tokens
max_output_tokens
```

Every other field is preserved **byte-for-byte** and forwarded verbatim
to the backend. Mellomting never uses strict endpoint structs that would
silently discard new fields, so tool calling, structured output,
reasoning fields, new OpenAI fields, vLLM extensions, and
coding-agent-specific parameters pass through unchanged.

The only field Mellomting rewrites is `model`, which is replaced with the
backend's configured `upstream_model` on the outbound request (PLAN §13).
The client always sees its own public model name.

## Generative output caps (PLAN §36)

Each generative public model can define a policy cap:

```yaml
policy:
  max_output_tokens: 32768
```

The per-endpoint field is:

| Endpoint          | Cap field                   |
|-------------------|-----------------------------|
| Chat Completions  | `max_completion_tokens` (or `max_tokens`) |
| Completions       | `max_tokens`                |
| Responses         | `max_output_tokens`         |

Behaviour:

- If a client asks for **more** than the configured cap, the request is
  rejected (400 `output_limit_exceeded`); the backend is not reached.
- If a client supplies **no** output limit, Mellomting injects the
  configured cap into the forwarded request (so a runaway generation is
  bounded even when the client does not bound it).
- Mellomting **never** silently raises a client-supplied limit.

This is a deliberate divergence from raw OpenAI/vLLM behaviour: an
unbounded request becomes bounded at the proxy. Clients that already
supply their own `max_tokens` / `max_completion_tokens` /
`max_output_tokens` are unaffected as long as they stay within the cap.

## Streaming token usage (PLAN §38)

When `accounting.ensure_stream_usage` is enabled (default when
accounting is on), Mellomting injects:

```json
"stream_options": { "include_usage": true }
```

into known OpenAI-compatible Chat/Completions stream requests that did
not already request usage, preserving any other existing `stream_options`
fields.

- If the client **already** requested usage, the chunk is relayed
  unchanged.
- If Mellomting injected the option on the client's behalf, the synthetic
  final usage-only chunk (usage present, no `choices`) is **consumed for
  accounting and not relayed**, so the client sees no semantic change to
  the stream.
- The Responses API emits usage in-band; no injection is performed there.

If exact stream usage cannot be obtained, the inference completes
normally and accounting records `usage_status: unknown` (never a made-up
exact count).

## Token quotas vs. accounting persistence

Per-key token budgets (`limits.tokens_per_hour` / `tokens_per_day`) are
enforced **regardless** of `accounting.enabled`. The in-memory quota
tracker always runs, so `accounting.enabled: false` only disables JSONL
usage records and startup replay — it never silently voids a token
quota.

Consequence: with accounting disabled, token quotas are enforced
in-memory only, so a daemon restart resets the current windows and no
usage is ever recorded. Mellomting warns about exactly this at startup
when accounting is disabled and at least one key carries a token budget.
Operators who need durable, replay-able quotas must leave accounting
enabled.

## Accounting log rotation (FIX-11/N7)

The accounting writer opens `usage.jsonl` once at startup and holds the
file descriptor forever; v1 has no in-process reopen path. Rotate the log
with `copytruncate`, **not** with rename: rename leaves the daemon
appending to the rotated inode while `mellomting usage report` reads an
empty current file, and under `security.landlock.mode: required` the
renamed file is a new inode the policy does not grant, so the daemon
cannot reopen it anyway.

The shipped policy is `deploy/mellomting.logrotate` (install at
`/etc/logrotate.d/mellomting`). It rotates `/var/log/mellomting/usage.jsonl`
(the default `accounting.path`) in place with `copytruncate`, preserving
the inode the Landlock policy granted at startup, so rotation works
without a restart in every mode. `copytruncate` truncates in place; in
the rare window between the copy and the truncate a line may be
duplicated or lost, which is acceptable for token-accounting JSONL.

## Endpoint coverage

Mellomting forwards only the allow-listed inference endpoints and 404s
everything else, including backend admin paths (PLAN §11.3):

```text
POST /v1/chat/completions
POST /v1/completions
POST /v1/embeddings
POST /v1/responses
GET  /v1/responses/{id}
POST /v1/responses/{id}/cancel
GET  /v1/models
```

## Tested upstream versions

The compatibility suite is exercised against a fake backend that emulates
the OpenAI-compatible contract. Real-vLLM compatibility tests (PLAN §85)
record the tested vLLM version here once they run against a live
deployment.
