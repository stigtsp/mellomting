# SystemOne decision endpoint

Status: plan, decisions settled; awaiting go-ahead. Date: 2026-10-06.

## Goal

A key can call a decision model such as Cloudflare Clef-Flash through
Mellomting with `POST /v1/systemone`, under the same authentication, limits,
accounting and live view as every other inference call. The first user is a
CPAN triage pipeline: Clef-Flash scores a distribution cheaply, and the
uncertain or high-risk cases escalate to the resident generation model. Both
calls go through one authenticated endpoint.

## Non-goals

- Validating `state` or `questions`. The decision server owns its schema and
  rejects what it cannot score; Mellomting reads `model` and nothing else, as
  it does for every other endpoint (PLAN §12).
- The qualifier framework (PLAN §45). There Mellomting itself calls a
  classifier to gate other traffic. Here a client calls the decision model
  directly, so all that is needed is routing.
- Media input. The probed server disables it; it is revisited when a server
  enables it, because media changes body sizes by orders of magnitude.
- Splitting or truncating input that exceeds the model's context. That is
  the client's job; Mellomting relays the rejection (§D6).

## What the probe established

`clef-probe.sh` against the Clef-Flash wrapper at `127.0.0.1:8011`,
2026-10-06. Raw responses were kept out of the repository.

| Observation | Consequence here |
|---|---|
| Only `POST /v1/systemone` and `GET /health`; `/v1/models`, `/metrics`, `/docs`, `/openapi.json`, `/v1/chat/completions` and `GET /v1/systemone` are 404 | One new route. Discovery cannot find this server (§D9) |
| Request is exactly `model`, `state`, `questions`; any other top-level key, including `stream` and `max_tokens`, is a 400 | Mellomting must forward the body with nothing added (§D3) |
| `model` must equal the server's own id (`clef-flash`) | The existing upstream-model rewrite covers it |
| Response: `model`, `answers` keyed by question id, `usage: {"input_tokens": N, "output_tokens": 0}`; HTTP/1.0 with Content-Length, never streamed | Usage is already parsed by the Responses-shape branch of `accounting.ParseUsage` (§D5) |
| Every error is a 400 with `{"error": "<string>"}` | The backend-diagnostic parser does not read this shape today, so the reason never reaches the operator or the client (§D6) |
| Body limit 65536 bytes, rejected as 400, not 413 | §D4 |
| Input limit 4096 tokens including the question schema; a 15 KB Perl file already exceeds it | Rejections will be common at first, so their reason has to reach the client (§D6) |
| `Authorization` is ignored; the wrapper has no authentication | Mellomting is its only access control; the server must not be reachable except through it |
| Valid calls took 133–158 ms with another workload resident | Defaults sized for generation are far too lax here (§D8) |

Not established by the probe: concurrent valid inference (both parallel
requests were rejected for size), `score` answers, and the 8-question and
128-option limits (known only from the wrapper's source).

## Decisions

### D1. One explicit route

`POST /v1/systemone`, added to the allow-list in `internal/httpapi/server.go`
next to the existing routes, and to the method classification that turns
other methods on a known path into 405. The path matches the upstream's, so
an existing SystemOne client only needs a new base URL and a key.

In the proxy it is one more row in the operation table
(`internal/proxy/proxy.go:87`):

```go
opDecide = operation{path: "/v1/systemone", method: "POST", route: routeByModel, endpoint: "systemone"}
```

It has no `capField`, so it is non-generative. That one fact already gives
the right behaviour: no output cap, no stream-usage injection, and
`"stream": true` refused with `errNoStream` before anything is forwarded.

### D2. A `decision` model type, and type gating by allow-list

A model gains `type: decision` alongside `generation` and `embedding`
(`internal/config/validate.go:660`). A key's model ACL works unchanged.

Type gating today is written as two exclusions
(`internal/proxy/proxy.go:606-615`): an embedding model is refused on a
generative endpoint, a generation model on the embeddings endpoint. A third
type falls through both checks, so a `decision` model would be served on
`/v1/chat/completions` and a generation model on `/v1/systemone`. That
fails open.

Replace it with an allow-list: each operation names the one model type it
serves, and any other type is refused with the existing 400
`model_type_mismatch`. A new message, "the requested model is not a decision
model", joins `errNotGenerate` and `errNotEmbed`. With an allow-list, a fourth
type later is refused everywhere until an operation names it.

### D3. Forward the body with nothing added

The server rejects any key it does not know. Mellomting therefore forwards
exactly what the client sent, with only `model` rewritten. The shallow
parse already does this; the risk is a future injection path that does not
check the operation. The test in §Tests pins it: the forwarded body's key
set equals the client's.

A client that sends `"stream": false` is forwarded, and the server rejects
it. That rejection is the server's rule, not Mellomting's, and it reaches
the client through §D6.

### D4. Body size: the server's limit, relayed for now

The global `max_buffered_request_bytes` (64 MiB) is three orders of
magnitude above what this server accepts. Over-size bodies are buffered,
forwarded, and rejected upstream with a 400 instead of a 413.

A per-model `max_request_bytes` would refuse them locally with the correct
413. It is deferred: the relayed 400 already says "Body must be 1–65536
bytes", and the token limit (4096) binds long before the byte limit for
source code. Revisit if over-size bodies show up as load.

### D5. Accounting: no code change

`{"input_tokens": N, "output_tokens": 0}` is the Responses usage shape, which
`fromUsageJSON` already recognizes by its key names
(`internal/accounting/record.go:174`). Total is computed as input plus
output. Records carry `endpoint: "systemone"`, so `mellomting usage report`
separates decision traffic from generation without a new field. Quota
reservation follows the non-generative path, as for embeddings. Tests pin
all three.

### D6. Read the `{"error": "<string>"}` shape

`upstreamDetail` (`internal/backend/diag.go`) recognizes two error shapes:
the OpenAI triple nested under `error`, and the flat triple next to
`object: "error"`. Clef's body is neither: the decode of a string into the
nested struct fails, and the diagnostic is empty. The operator's debug line
and the opt-in client relay are then silent for exactly the failures this
model produces most: input too long, a bad question type, an unknown key.

Add a third shape: when `error` is a JSON string, it becomes `Message`, with
`Type` and `Code` empty. It goes through the same bound, sanitizer and parse
cap. A string is a scalar, so this does not widen what is read past PLAN
§43's "scalar-only" rule. It does widen the recognized shapes, and §43 says
which shapes are read, so §43 is amended in the same change. Ollama sends
this shape too, so this also closes the gap noted in the branch review.

With relay on, which §D12 makes the default for every server, a client
sees, for example, `upstream rejected the request: Complete input exceeds 4096 tokens;
shorten state/schema`. Every message observed is about the client's
request. One is not: on a request missing `state`, the server says "model
must be clef-flash and state is required", which names the upstream model.
That is harmless when the public name equals the upstream name. Under
§D11 that is also the configuration that needs the fewest lines.

### D7. Retries: unchanged

Every observed rejection is a 400, which is not retried. Connection failures
and 502/503/504 retry under the existing budget. A decision call is
idempotent, so a retry before any response byte has no side effect.

### D8. Admission: its own server entry, the usual defaults

The Clef wrapper is a separate process, so it is a separate `servers:`
entry with its own admission pool, and a burst of triage calls cannot take
Qwen's slots. Its admission limits are the ordinary defaults. The operator
already sets `max_concurrency`, `queue_size` and `queue_timeout` per
server, and is better placed than Mellomting to know what a given decision
server sustains; `top` and the `queue_ms` field show when the limits are
wrong.

The timeouts are another matter, because no configuration key reaches
them. The generation `request_timeout`, which is the bound a non-streaming
call waits on, is 30 minutes, so a wedged decision server would hold each
slot for half an hour. §D11 gives a decision server its own timeouts.

### D9. Discovery and init

The wrapper has no `/v1/models`, so `mellomting init --server` cannot list
its models, and the model block is written by hand. Init is not changed.
`upstream_model` defaults to the public name, so a model named `clef-flash`
needs no `upstream_model` line.

`/v1/models` on Mellomting lists a decision model like any other the key may
use. The model-metadata work (`docs/MODEL_METADATA_PLAN.md`) hides embedding
models from the opencode-facing view, and must hide decision models the same
way, or opencode offers Clef as a chat model. That plan gets a one-line
note.

### D10. Everything else applies as-is

- Request body never logged; `state` will carry package source.
- Global and per-key in-flight caps and RPS limiters.
- `max_response_bytes`.
- Passive health; the wrapper's `/health` is not used.
- The `request` access line; `top` shows the endpoint `POST /v1/systemone`.

### D11. Defaults that need no tuning

The aim is that adding Clef is this, and nothing else:

```yaml
servers:
  clef:
    url: http://127.0.0.1:8011

models:
  clef-flash:
    type: decision
    servers: [clef]
```

Each line is information Mellomting cannot work out for itself. With no
`/v1/models` on the server, `type: decision` cannot be discovered, and
without it the model is a generation model. That fails safely: a call to
`/v1/systemone` gets "the requested model is not a decision model".
Everything else comes from defaults. Admission limits are the ordinary ones
(§D8); only what an operator cannot configure differs:

| Setting | Decision server | Generation server | Why |
|---|---|---|---|
| `upstream_model` | the public name | same | Already the rule. Naming the model `clef-flash` also keeps the one relayed message that names the upstream model harmless (§D6). |
| `request_timeout` | 30s | 30 min | The bound a non-streaming call waits on. 200 times the observed latency still refuses a wedged server in seconds, not half an hour. |
| `header_timeout` | 30s | 300s | Unused while streaming is refused, but kept no larger than the request bound. |

A server takes the decision timeouts when every model routed to it has
`type: decision`. A server shared by decision and generation models keeps
the generation timeouts: the larger bounds hurt a decision call only by
waiting longer, while the smaller ones would cut generation off.
Normalization resolves this where it already builds each server's
backends; it needs the types of the models that reference the server,
which it has. The startup `backend admission` line prints the timeouts in
force. Timeouts gain no configuration key, so `show-effective` gains no
field and its output still parses back to the same config.

`queue_timeout` must not exceed `request_timeout`: a longer wait would
outlive the request it is waiting to send. Today the check compares
against the generation constant. It moves to the server's own request
bound, and the default 120s wait, which is above a decision server's 30s,
is lowered to that bound rather than refused. Only a `queue_timeout` the
operator wrote above it is an error.

Keys: a key with the `*` model wildcard can call Clef as soon as it is
configured. A key with an explicit model list needs `clef-flash` added,
which is intended: granting a new model is an operator decision.

### D12. Relay backend error messages by default, on every server

Decided 2026-10-06. Without relay, every Clef rejection reads "upstream
rejected the request", and a client cannot tell an input that is too long
from a malformed question. The same is true of generation backends: a
context-window overflow and a chat-template mismatch look identical. So
`relay_upstream_errors` defaults to on for every server, and an explicit
`false` turns it off for a backend the operator does not trust with its
clients.

This amends a PLAN §43 MUST, which today requires an explicit per-server
opt-in, and the PLAN schema line that documents the default as false. What
stays as it is:

- The envelope: status, `type` and `code` are always Mellomting's own.
- The status filter: 400, 413, 422 and 429 are relayed. 401/403 (the
  injected credential), 404 (the upstream model name) and 5xx (tracebacks)
  never are.
- The bound and sanitizer on the message.

Two things were weighed for the amendment:

1. **429 stays relayable.** A local vLLM or SGLang 429 is about load and
   harmless to relay. A third-party gateway's 429 is about the credential
   Mellomting injected, and OpenAI's names the organization; an operator
   fronting such a gateway sets `relay_upstream_errors: false` on that
   server. CONFIGURATION.md says so where it documents the flag.
2. **The record of what was relayed moves off the default log level.**
   §43 requires every relayed message to be logged regardless of level, so
   that something records what was disclosed. As a default, that would
   put backend text, which can quote the prompt, into the default
   operational log for all traffic, against the rule that prompts never
   appear there (PLAN §24, AGENTS.md). The disclosure is to the client that
   sent the request, about its own request, so no other tenant learns
   anything from it. The Info line keeps request ID, backend, status and
   the message's byte length; the text goes on the debug line next to it.

`RelayUpstreamErrors` becomes a pointer in the source schema, as the
admission fields already are, so an explicit `false` can be told from
unset. `config show-effective` prints the resolved value.

## Implementation

In this order. Each step leaves `make check` passing.

1. **Relay default** (§D12). Pointer field and default true; the Info line
   carries the message length and the debug line its text. Amend PLAN §43
   and the schema line at PLAN:759; update CONFIGURATION.md, with the
   third-party-gateway advice, and the relay tests:
   - relayed by default;
   - not relayed with an explicit `false`;
   - the Info line holds no message text;
   - `show-effective` prints the resolved value.
   Independent of SystemOne, and changes behaviour for existing servers,
   so it is its own commit.
2. **Diagnostic string shape** (§D6). `internal/backend/diag.go`, plus
   tests in `diag_test.go`:
   - string `error` read and sanitized;
   - control and bidi characters in it stripped;
   - over-long string truncated on a rune boundary;
   - empty string yields the zero Detail;
   - `error: null` and `error: 42` yield nothing.

   Add a string-form seed to the existing fuzz corpus. Amend PLAN §43.
   This step stands alone and is useful to Ollama backends without the
   rest.
3. **Model type and its defaults** (§D2, §D11). Add `decision` to the config
   validation and to the source schema; reject `max_output_tokens` on a
   decision model as meaningless. Resolve a server's timeouts from the
   types of the models it serves. Tests:
   - the minimal block in §D11 parses with the decision timeouts, the
     ordinary admission defaults, and the queue wait lowered to 30s;
   - a mixed server keeps the generation timeouts;
   - an explicit `queue_timeout` above the server's request bound is
     refused, naming the server;
   - `show-effective` output parses back to the same config.

   Rewrite type gating as an allow-list, with a table test covering every
   operation × model type pair: the three serving pairs succeed, every
   other pair is a 400 `model_type_mismatch`.
4. **Route and operation** (§D1). Add `opDecide`, `Proxy.Decide`, and the
   httpapi route and method classification.
5. **Tests for the route.** Against a fake SystemOne backend that, like the
   real one, rejects unknown keys:
   - the forwarded body has exactly the client's keys, with `model`
     rewritten (§D3);
   - `"stream": true` is refused locally with 400 and never forwarded;
   - GET is a 405, and nothing under `/v1/systemone/` is routed;
   - a key without the model gets the same 404 as an unknown model;
   - a generation model on this route, and the decision model on chat,
     are 400;
   - usage is accounted with `endpoint: "systemone"`, `input_tokens`
     counted and total equal to input;
   - a 400 `{"error": "..."}` is relayed by default and kept generic with
     `relay_upstream_errors: false`, with the debug line carrying the
     message either way.
6. **Docs.**
   - PLAN: the endpoint in §11.1, a short subsection with the D1–D3
     rules, and the §43 shape amendment from step 2.
   - `docs/COMPATIBILITY.md`: a SystemOne section naming the wrapper
     revision probed, its strict key set, usage shape and error shape.
   - `docs/CONFIGURATION.md`: `type: decision`, the §D11 minimal block,
     and the decision-default table.
   - AGENTS.md status, one sentence.
7. **Live check.** Point a test configuration at the real wrapper. Rerun
   `clef-probe.sh` through Mellomting with a key, and compare against
   today's direct run. Same answers and usage; size and schema rejections
   relayed; all other routes 404.

## Settled

Decided 2026-10-06:

- A decision server uses the ordinary admission defaults; tuning them is
  the operator's (§D8). Only the timeouts, which have no configuration key,
  differ (§D11).
- Decision tokens count against the key's existing token quota, input
  only. No new configuration.
- Scope stays minimal: a configurable `header_timeout`, a per-model
  `max_request_bytes`, and hiding decision models from `/v1/models` are
  all deferred (§D4, §D9). §D11 makes the timeout key unnecessary for
  Clef; it is still wanted for generation servers.
- Adding Clef should need only the §D11 block (2026-10-06).
- Relay is on by default for every server; 429 stays relayable; the Info
  record carries the message length and the debug line its text (§D12).
