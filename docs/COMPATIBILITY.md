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
POST /v1/systemone
GET  /v1/models
```

Other routes, including backend admin endpoints, are not forwarded.
`/v1/models` lists the public models available to the authenticated key.

## SystemOne decision endpoint

`POST /v1/systemone` serves a decision model (`type: decision` in the
configuration). The contract below was probed against the Clef-Flash
wrapper at `127.0.0.1:8011` on 2026-10-06 with `clef-probe.sh`; no finer
wrapper revision is recorded.

The request is exactly `model`, `state`, and `questions`. Any other
top-level key, including `stream` and `max_tokens`, is rejected with a 400,
so Mellomting forwards the client's body unchanged apart from the `model`
rewrite and refuses `"stream": true` itself. A valid call answers with
`model`, `answers` keyed by question id, and
`usage: {"input_tokens": N, "output_tokens": 0}`; the response is buffered,
never streamed. Every error is a 400 with `{"error": "<string>"}`; the
reason is relayed to the client by default (see `relay_upstream_errors`).

Limits observed on the wrapper: request bodies of 65536 bytes (rejected
with a 400, not a 413) and 4096 input tokens including the question
schema.

Other SystemOne servers accept more than Clef-Flash (the Brev pilot,
2026-11-24): optional `images` and `assets` keys, `state` as a string or
an object, bodies up to 32 MiB, `multipart/form-data` bodies with a
`request` JSON part, and `Content-Encoding: gzip`, `x-gzip` or `zstd`.
Through Mellomting:

- The body must be one uncompressed JSON object, because `model` is read
  from it and checked against the key before anything is forwarded. A
  multipart body is refused with 400 `invalid_json`, and a compressed one
  with 415 `unsupported_media_type`; neither reaches the server. Send
  assets inline in the JSON.
- `server.max_body_bytes` (16 MiB by default) bounds the body, for every
  endpoint; raise it for large images. The server's own limit still
  applies after it.
- Every top-level key other than `model` is forwarded unchanged, so
  `images`, `assets` and any later key pass through. `model` is rewritten
  to the model's `upstream_model`, which defaults to the public name. A
  server that accepts only its own aliases needs `upstream_model` set to
  one; a server that ignores `model` needs nothing. There is no way to
  omit `model` from the forwarded body.
- `latency_ms`, `id` and other response keys are relayed unchanged. Usage
  is read from `usage.input_tokens`; a response without it is charged
  `accounting.unknown_usage_reservation`.

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

## Server load reports

`mellomting top` reads these, and nothing else, from each server (PLAN §43.2);
unknown names are ignored, so a renamed series shows as "no metrics" rather
than breaking anything. The allow-list is in `internal/backendmetrics/parse.go`.

- vLLM `/metrics`: `vllm:num_requests_running`, `vllm:num_requests_waiting`,
  `vllm:kv_cache_usage_perc` (older releases: `vllm:gpu_cache_usage_perc`),
  token and preemption counters.
- SGLang `/metrics`, only when started with `--enable-metrics`:
  `sglang:num_running_reqs`, `sglang:num_queue_reqs`, `sglang:token_usage`,
  token counters.
- Ollama has no `/metrics`; `/api/ps` gives loaded models, their VRAM and
  when each unloads. It reports no queue.

## Test coverage

Automated compatibility tests use a simulated OpenAI-compatible backend.
No real-vLLM version is recorded as tested here yet.

See [Configuration](CONFIGURATION.md) for settings and
[the design](PLAN.md) for the full contract.
