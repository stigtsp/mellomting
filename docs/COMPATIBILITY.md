# API compatibility

Mellomting accepts OpenAI-compatible requests and forwards them to configured
inference servers. This page describes the changes clients can observe.

## Endpoints

```text
POST /v1/chat/completions
POST /v1/completions
POST /v1/embeddings
POST /v1/responses
GET  /v1/responses/{id}
POST /v1/responses/{id}/cancel
GET  /v1/models
```

Other routes, including backend admin endpoints, are not forwarded.
`/v1/models` lists the public models available to the authenticated key.

## Request fields

Mellomting checks the top-level routing and policy fields: `model`, `stream`,
`stream_options`, `previous_response_id`, and output token limits. Other
fields pass through, including tools, structured output, reasoning parameters,
and server extensions. JSON formatting may change when the body is re-encoded.

The outbound `model` is replaced with the configured `upstream_model`.
Response bodies are relayed without rewriting model names; a backend may
return its own model name.

## Output limits

Set a per-model cap with `policy.max_output_tokens`:

```yaml
policy:
  max_output_tokens: 32768
```

| Endpoint | Request field |
| --- | --- |
| Chat Completions | `max_completion_tokens` or `max_tokens` |
| Completions | `max_tokens` |
| Responses | `max_output_tokens` |

Requests above the cap receive HTTP 400 with code `output_limit_exceeded`.
When no limit is supplied, Mellomting inserts the cap. Lower client limits
are preserved.

## Streaming usage

When accounting or a key's token quota needs usage, Mellomting defaults to
setting `stream_options.include_usage: true` for Chat and Completions streams.
This also overrides an explicit `false`. Other stream options are preserved.

If the client requested usage, its usage chunk is forwarded. Otherwise,
Mellomting reads the final usage-only chunk for accounting and omits it from
the client's stream. Responses streams report usage directly and need no
injected option.

`accounting.ensure_stream_usage: false` disables injection. Missing usage is
recorded as `unknown`; quota accounting uses the configured fallback
reservation. A missing usage report alone does not interrupt the response.

## Responses and quotas

Response IDs are tied to the key and backend that created them. Retrieval,
cancellation, and continuations require a matching entry in the proxy's
bounded affinity table. Entries expire and are cleared on restart.

Retrieval and cancellation do not count the response's original usage toward
quota again. They still produce request records.

Token quotas apply even when accounting is disabled. Without accounting,
usage is held in memory and quota windows reset on restart. Enable accounting
for JSONL records and startup replay.

## Response size

`server.max_response_bytes` limits both buffered responses and bytes emitted
by SSE streams. The default is 64 MiB. A stream exceeding the limit is
terminated; headers already sent to the client cannot be changed. The request
log records `backend_stream_error`.

## Log rotation

Use the supplied `deploy/mellomting.logrotate` policy for the accounting log.
It uses `copytruncate` because the daemon keeps the log open until shutdown.
Renaming the log would leave the daemon writing to the old file.

A line can be duplicated or lost between copying and truncating. Token
accounting is intended for usage tracking and quota enforcement.

## Test coverage

Automated compatibility tests use a simulated OpenAI-compatible backend.
No real-vLLM version is recorded as tested here yet.

See [Configuration](CONFIGURATION.md) for settings and
[the design](PLAN.md) for the full contract.
