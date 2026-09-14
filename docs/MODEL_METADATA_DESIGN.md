# Model metadata and client bootstrap

Status: design, approved for planning. Date: 2026-09-14.
Implementation plan: `docs/MODEL_METADATA_PLAN.md`.

## Goal

A user given a base URL and an API key gets every model that Mellomting
serves, with correct context and output limits, without hand-maintaining a
model list. Adding a model to Mellomting makes it appear in their client.

The reference client is opencode. Sections 1–3 and 5 are client-agnostic; §4
and §6 are opencode-specific and ship no software from this repo.

## Non-goals

- Tool-calling, vision, reasoning or cost *metadata* on `/v1/models`.
  Mellomting cannot observe tool support, and declaring capabilities per model
  is config that must be kept true by hand. Request *defaults* are different
  and are in scope (§5).
- Clients other than opencode. The `/v1/models` fields follow the convention
  other clients are converging on, but no other client is targeted or tested.
- Shipping an npm package, for either opencode line. Deferred, with the
  triggers recorded in §7.

## Current state

- `handleModels` (`internal/httpapi/models.go`) emits `id`, `object`,
  `created`, `owned_by` and nothing else.
- `ParseModels` (`internal/discovery/discovery.go`) extracts model IDs and
  discards the rest of each card, including `max_model_len`.
- `config.ModelPolicy` holds `max_output_tokens` only. No field anywhere
  records a context window.
- Health is `/healthz` and `/readyz` (`internal/httpapi/server.go`),
  unauthenticated and exempt from inflight limits.
- `mellomting init` is create-only (`cmd/mellomting/init.go`) and nothing
  else runs discovery. A model added after init is a hand-written YAML block.

## Which opencode

Two lines exist and they differ on everything this design touches.

**Stable, 1.18.x** — npm `latest`, what users run today. Config is
`provider.<id>` with `npm` and `options`. Credentials come from
`opencode auth login` (choose *Other*, give a provider ID, paste the key),
stored in `~/.local/share/opencode/auth.json`. **No model discovery of any
kind**: a custom provider lists its models explicitly. The vLLM discovery
port to this line (#47346) was closed unmerged on 2026-09-04.

**v2** — a branch published on pre-release tags, whose docs live under
`/v2/docs`. Config is `providers.<id>` with `package` and `settings`.
Credentials come from `/connect`, same store. Built-in vLLM discovery
(#43022, merged 2026-08-17): probes `/health`, reads `/v1/models` with bearer
auth, keeps only cards with `owned_by: "vllm"`, maps `max_model_len` to
`limit.context`, and starts discovered models with tools disabled. The
`@opencode/ai/providers/vllm` package is reusable under any provider ID, so
several servers can each be a provider with their own URL and credential.

Both are targeted. Stable gets §6; v2 gets §4; both get §1–§3 and §5.

## Decisions

| Decision | Chosen | Rejected |
|---|---|---|
| Context window source | Capture at discovery, config overrides | Hand-written only; live passthrough per request |
| Discovery after init | Read-only `config discover` prints the block | Init-only capture; a command that edits the config |
| Metadata scope | Limits only | Capabilities; full models.dev card |
| Replica disagreement | Minimum of known values | Fail init; omit |
| opencode v2 | Opt-in vLLM-dialect compatibility | Plugin; third-party discovery plugin; masquerade unconditionally |
| opencode stable | Authenticated endpoint emitting the provider block | Plugin on the v1 `config` hook; a CLI that renders the block; nothing |
| Credentials | opencode's own store; nothing in `opencode.json` | Env vars; a key in the config file; echoing the key in the endpoint |
| Reasoning and other request options | Client-side variants documented; server-side `policy.body` aliases | Variants emitted for a plugin to forward |

The compatibility mode is opt-in because `owned_by: "vllm"` is not true in
general: Mellomting fronts whatever the operator configures, which need not be
vLLM. An operator turning on a documented compatibility knob is making that
claim deliberately for their own deployment. Emitting it by default would be
Mellomting lying about itself.

The stable-line plugin was considered and dropped as too much for now, even
though its `config` hook is mature. The endpoint delivers most of the same
value with no second toolchain: the operator sends a URL, the user pastes.

## 1. Capture the context window at discovery

`ParseModels` returns `[]Model` instead of `[]string`:

```go
type Model struct {
	ID            string
	ContextLength int // 0 when the server reports none
}
```

The card's `max_model_len` supplies it, falling back to `context_length` for
servers following the OpenRouter and LiteLLM spelling. The value is parsed
under the same fail-closed discipline as the ID, because the server answering
discovery is not trusted: accept only a JSON integer in `[1, 1<<31)`, and
reject a float, a string, a negative or an out-of-range value as a malformed
response rather than coercing it. An absent field is unknown, not an error —
not every backend reports one.

`Aggregate` reconciles a model served by several backends by taking the
**minimum of the known values**, because a request may be routed to any
replica and must fit on all of them. The asymmetry matters: a window set too
high produces a backend 400 on an oversized request, which maps to
`errUpstreamRejected` and is not retried, while a window set too low only
makes the client compact early. When no replica reports a window the model
carries none. A replica that reports nothing does not veto the others; the
recorded value is the best available information, not a guarantee, and an
operator who needs certainty sets the field explicitly.

`Result` gains a parallel map rather than changing the shape of `Models`:

```go
type Result struct {
	Models        map[string][]string // model -> server names (unchanged)
	ContextLength map[string]int      // model -> reconciled window, absent when unknown
}
```

## 2. Configuration records it

`ModelPolicy` gains `context_length`:

```yaml
models:
  deepseek-v4-flash:
    type: generation
    policy:
      context_length: 196608
      max_output_tokens: 32768
    servers: [local]
```

`mellomting init` writes what discovery reconciled; an explicit value always
wins, and nothing rewrites an existing config.

Init runs once, so on its own the capture would help exactly once. A new
read-only subcommand covers every model added afterwards:

```
$ mellomting config discover -server local=http://10.17.160.10:8000
models:
  deepseek-v4-flash:
    type: generation
    policy:
      context_length: 196608
    servers: [local]
```

It runs the same discovery and aggregation as init against the servers named
on the command line, prints the `models:` block, and writes nothing — the
operator pastes what they want. It shares init's `-server` parsing and its
derived backend-network policy, and adds no new network behaviour.

Validation: `context_length` must be non-negative, and `max_output_tokens`
must not exceed it when both are set. The two fields are unrelated today, so a
config claiming more output than context passes silently.

## 3. `/v1/models` emits two fields

```json
{
  "id": "deepseek-v4-flash",
  "object": "model",
  "created": 1789355433,
  "owned_by": "mellomting",
  "context_length": 196608,
  "max_output_tokens": 32768
}
```

Each field is omitted when zero, so the response never asserts a limit it does
not know. `context_length` and `max_output_tokens` are the spelling OpenRouter
uses, LiteLLM added to its own `/v1/models`, and agentgateway#3345 proposes as
the convention.

`max_output_tokens` is Mellomting's policy cap, not a claim about the model:
`proxy.prepare` rejects a request asking for more with `output_limit_exceeded`.
From the client's side that coincides with what OpenRouter and LiteLLM mean —
the most it may request — which is why the name is reused. It is omitted for
`type: embedding` models, where no output limit applies.

The ACL filter is unchanged, and the endpoint still exposes no backend URL,
backend name, or upstream model ID. Fields vLLM returns that Mellomting will
not forward under any mode: `root` (leaks the backend's filesystem layout,
that it runs as root, and the model revision), `permission` and `parent`
(deprecated upstream and read by nothing).

## 4. opencode v2: opt-in vLLM-dialect compatibility

```yaml
server:
  models_compat: vllm   # default: "", meaning no compatibility mode
```

The only accepted values are `""` and `vllm`. When set to `vllm`, three things
change:

- `type: generation` cards report `owned_by: "vllm"` instead of
  `"mellomting"`. Embedding models keep `"mellomting"`, so opencode's filter
  drops them instead of registering them as chat models.
- Each card carries `max_model_len` alongside `context_length`, with the same
  value.
- `GET /health` is served as an alias of `/healthz`: unauthenticated, exempt
  from inflight admission, identical body. `allowFor` learns the path so a
  wrong method still answers 405 rather than 404.

Nothing else changes. The mode adds no field carrying information that
`context_length` does not already carry. It does disclose one thing the
default mode does not: `owned_by: "vllm"` tells the client which engine the
operator claims to run. That claim is the operator's, made by turning the
knob on, and it names an engine rather than a host, path or model revision.

`/v1/models` stays authenticated and ACL-filtered under this mode. Discovery
is therefore per user: opencode fetches the list with the key from `/connect`,
and each person sees exactly the models their key allows, with no separate
list to maintain on the client side.

A v2 user then needs no plugin and no shipped software:

```jsonc
{ "providers": {
    "mellomting-home": { "package": "@opencode/ai/providers/vllm",
                         "settings": { "baseURL": "https://home.example/v1" } },
    "mellomting-work": { "package": "@opencode/ai/providers/vllm",
                         "settings": { "baseURL": "https://work.example/v1" } } } }
```

```
$ opencode
> /connect        # pick each server, paste its key
> /models         # both servers' models, correct context limits
```

**Known cost, to be documented rather than worked around:** discovered models
arrive with tools disabled, so each model a user wants to drive as a coding
agent needs a `capabilities.tools: true` entry under that provider's `models`
map. A new model therefore appears automatically but is not immediately
tool-capable. This is one reason §7 exists.

Tool calling is a property of the backend, not of Mellomting: vLLM only emits
structured `tool_calls` when launched with `--enable-auto-tool-choice` and
`--tool-call-parser`, and without them a request carrying `tool_choice: "auto"`
is rejected upstream. Mellomting forwards the `tools` array unchanged and
cannot observe whether those flags were passed, so it cannot answer the
question opencode is being conservative about. Adding a `policy.tools` field
would not help under this mode either: opencode's vLLM discovery reads only
`max_model_len` from the card and would ignore it.

**Open questions for the spike**, any of which can sink §4:

- Whether the vLLM discovery sends the `/connect` credential as a bearer
  token **on the `/v1/models` fetch itself**, not only on completions.
  vLLM's own listing is normally open; Mellomting's answers 401 without a
  key. #43022 claims bearer support; the spike confirms where it applies.
- Whether a `models` entry overriding `capabilities` *merges with* the
  discovered card or *replaces* it. If it replaces, the same line also
  discards the discovered `limit.context`, the operator is back to
  hand-writing context windows, and most of §4's value is lost.
- What discovery does with a card that has no `max_model_len` (context
  unknown, so omitted): skip the model, apply a default, or fail.
- Whether the `/health` probe checks only the status. vLLM answers an empty
  200; `writeHealth` answers JSON.
- Whether the vLLM package's `variants(model)` returns anything for a
  discovered model. Expected: nothing, as Mistral's did before opencode
  added entries in its own source. Informational; §5 does not depend on it.

A bad answer to the first two drops §4 and leaves v2 on §6's endpoint.

## 5. Request defaults: variants without client config

### What clients do

opencode exposes per-model **variants**: named bundles of request options
chosen with `#name` or ctrl+t. Each carries `settings` (options the provider
package translates, such as `reasoningEffort`), `body` (raw fields merged into
the request) and `headers`. Default variants come from a `variants(model)`
function in each provider package's source; a self-hosted server cannot add
to it, and the vLLM package is expected to return none. On both opencode
lines a variant is therefore a per-model block in the client config.

Mellomting already forwards these unchanged: `shallowParse` (`proxy.go`)
preserves unknown fields verbatim, so a variant setting
`chat_template_kwargs` reaches vLLM as sent. For DeepSeek-V4 on vLLM the
reasoning controls live there rather than in a top-level `reasoning_effort`:

```jsonc
"deepseek-v4-flash": { "variants": [
  { "id": "think",     "body": { "chat_template_kwargs": { "thinking": true } } },
  { "id": "think-max", "body": { "chat_template_kwargs": { "thinking": true, "reasoning_effort": "max" } } }
]}
```

This costs no server work and is documented in the plan's final task.

### Server-side aliases

Every public model is already an alias — `upstream_model` rewrites the name
per backend — and `prepareOutbound` already injects fields into the outbound
body (the output cap, `stream_options.include_usage`). `ModelPolicy` gains
`body`, a mapping merged into the request the same way:

```yaml
models:
  deepseek-v4-flash:
    type: generation
    servers: [local]
  deepseek-v4-flash-think:
    type: generation
    servers: [local]             # same server, same upstream model
    upstream_model: deepseek-v4-flash
    policy:
      body:
        chat_template_kwargs: { thinking: true }
```

Both aliases appear on `/v1/models`, and so in any client through any
discovery path, with nothing configured on the client. The trade is that a
variant is a model switch rather than a toggle, and N variants are N aliases.

Semantics:

- **Inject when absent, never override.** A top-level key the client sent is
  left exactly as sent, including its nested content. This is the output
  cap's existing rule — never silently change what the client asked for —
  and it keeps the alias honest: it supplies defaults, not policy.
- Shallow merge at the top level of the JSON object, for every route-by-model
  operation. The operator is responsible for the fields being valid for the
  backend and endpoint they route to; `chat_template_kwargs` is vLLM's, and
  an alias carrying it must route to vLLM.
- `prepareOutbound` currently returns early when neither the cap nor usage
  injection applies (embeddings, usage injection off). Body injection must
  run before that return.

Validation, at config load:

- `policy.body` must be a mapping whose contents encode as JSON. Values are
  otherwise arbitrary.
- Keys Mellomting owns are rejected: `model`, `stream`, `stream_options`,
  and the cap fields (`max_tokens`, `max_completion_tokens`,
  `max_output_tokens`). Letting config set these would silently break
  routing, accounting or the cap.
- Serialized size is bounded (4 KiB). This is a defaults map, not a prompt.

`config show-effective` renders it. Nothing about it is emitted on
`/v1/models`: it is request policy, not model metadata.

## 6. opencode stable: the config endpoint

Stable opencode cannot discover anything, so Mellomting hands the user the
provider block instead:

```
$ curl -H "Authorization: Bearer $KEY" https://home.example/client-config/opencode
```

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "mellomting-home": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Mellomting (home)",
      "options": { "baseURL": "https://home.example/v1" },
      "models": {
        "deepseek-v4-flash": {
          "name": "deepseek-v4-flash",
          "limit": { "context": 196608, "output": 32768 }
        }
      }
    }
  }
}
```

The user pastes that into `opencode.json`, runs `opencode auth login`,
chooses *Other*, enters the provider ID from the block, pastes the key, and
is done. Two servers are two blocks with two IDs, merged by hand.

Design:

- `GET /client-config/opencode`, authenticated exactly like `/v1/models`:
  after the source and global rate limits, with the key's ACL applied, so
  the block lists only the models that key may use.
- The key is **never echoed**. The block is safe to paste into a config
  file that is committed; the credential goes where opencode keeps it.
- `baseURL` comes from a new `server.public_url` (scheme, host, optional
  port, no path). Without it the route is not allow-listed at all and
  answers 404 like any unknown path: the default surface is unchanged, and
  Mellomting never reflects a client's `Host` header into a URL.
- The provider ID derives from `public_url`'s host — first label, lowercased,
  characters outside `[a-z0-9-]` replaced by `-`, prefixed `mellomting-` —
  and the display name is `Mellomting (<label>)`. An operator with two
  servers gets two distinct IDs without configuring anything further.
- Each model carries `limit.context` from `context_length` and
  `limit.output` from `max_output_tokens`, each key omitted when the value
  is unknown; the whole `limit` object is omitted when both are.
- The block is static: a new model needs the user to fetch again. That is the
  accepted cost of no plugin, and the URL is short enough to re-run.

The v2 shape (`providers`, `package`, `settings`) is not emitted. v2 users
have §4; if the spike sinks §4, adding `?format=v2` here is the fallback.

## 7. Deferred: a first-party plugin

For **v2**, an `opencode-mellomting` plugin would register each server via
`ctx.provider.transform`, resolve credentials through
`ctx.integration.connection`, and **omit** the capabilities block — which
triggers opencode's assume-tools fallback and removes the per-model edit that
§4 requires. It would also drop the dependence on another provider's
`owned_by` filter, give the providers real names, and could register proper
variants — toggled with ctrl+t rather than switched as aliases — from a hint
Mellomting emits.

For **stable**, a plugin on the mature `config` hook would fetch §6's block
and merge it into `config.provider`, so new models appear without a re-paste.
It is a few dozen lines against a stable API; it was dropped for now as one
toolchain too many, not on merit.

Revisit when any holds:

- The per-model `capabilities.tools` edit on v2 becomes a routine annoyance.
- opencode changes the `owned_by` filter, the `/health` probe, or the
  `max_model_len` mapping, breaking §4.
- Re-fetching §6's block after every model change becomes the complaint.
- Alias-per-variant (§5) proves too clumsy, and users want ctrl+t.

## Data flow

```
vLLM /v1/models (max_model_len)
  -> discovery.ParseModels      bounded parse, per server
  -> discovery.Aggregate        min across replicas
  -> mellomting init            writes policy.context_length
     mellomting config discover prints it for a model added later
  -> config                     operator may override; policy.body per alias
  -> handleModels               context_length + max_output_tokens, ACL-filtered
                                (+ max_model_len, owned_by: vllm under compat)
  -> /client-config/opencode    same data, shaped as a stable provider block
  -> opencode                   v2: vllm discovery   stable: pasted block
  -> request                    client variant body, or nothing
  -> prepareOutbound            policy.body injected where absent
  -> backend
```

## Error handling

| Condition | Behaviour |
|---|---|
| Backend omits the field at discovery | Model recorded without a window |
| Backend sends a malformed value | That server's discovery fails, naming the server |
| Replicas disagree | Minimum of known values recorded |
| `config discover` cannot reach a server | Non-zero exit naming the server; nothing printed |
| `max_output_tokens` > `context_length` | Config validation error |
| `models_compat` set to an unknown value | Config validation error |
| `public_url` has a path, query, userinfo, or a non-http scheme | Config validation error |
| `policy.body` names a Mellomting-owned key, is not a mapping, does not encode as JSON, or exceeds 4 KiB | Config validation error |
| Client sends a key `policy.body` also sets | Client value forwarded untouched |
| Client unauthenticated at `/v1/models` or `/client-config/opencode` | 401, no model list |
| `/client-config/opencode` without `public_url` | 404, as for any unknown path |
| Mellomting unreachable from opencode | That provider contributes no models |

## Testing

- `ParseModels`: absent, zero, negative, float, string, `1<<31`, valid;
  `context_length` fallback; a malformed card fails that server.
- `Aggregate`: minimum across disagreeing replicas; all-unknown omits; one
  unknown does not veto the rest.
- `renderInitConfig`: golden config carrying `context_length`.
- `config discover`: prints the same block init would write, writes no file,
  and fails with the server name when one server is unreachable.
- `validateModels`: negative rejected; output exceeding context rejected;
  `policy.body` rejects a non-mapping, each owned key, an unencodable value,
  and an oversized map.
- `validateServer`: `models_compat` accepts unset and `vllm`, rejects others;
  `public_url` accepts `https://h`, `http://h:8080`, rejects a path, a query,
  userinfo, and `ftp://`.
- `handleModels`: both fields present; each omitted when zero;
  `max_output_tokens` omitted for embedding models; ACL filtering unchanged;
  no `root`/`permission`/`parent`; under compat, generation models report
  `owned_by: vllm`, embedding models keep `mellomting`, and `max_model_len`
  matches `context_length`.
- Routing: `/health` is 404 by default, 200 under compat, 405 on POST under
  compat, and never counts against inflight admission.
  `/client-config/opencode` is 404 without `public_url`, 401 without a key,
  405 on POST, and 200 with a key.
- Client config: golden JSON for one generation and one embedding model;
  ACL-filtered like `/v1/models`; provider ID derived from a hostname, from
  an IP literal, and from a host with uppercase and underscores; `limit`
  omitted when both values are unknown; the bearer token appears nowhere in
  the body.
- `prepareOutbound`: `policy.body` keys injected when absent; a client-sent
  key, including a nested object under it, forwarded byte-for-byte; injection
  runs for embeddings and with usage injection off; nothing injected when
  `policy.body` is unset. `config show-effective` renders `policy.body`.
- End to end, by hand, in the spike and again at the end: stable opencode
  with a pasted block; v2 with compat on.

## Risks

**§4 depends on undocumented client behaviour.** The `owned_by` filter, the
`/health` probe and the `max_model_len` mapping are opencode implementation
details on a pre-release branch, not a published contract. The spike
verifies them before the knob is built; §6 serves v2 too if they fail.

**v2 may change shape before it is `latest`.** Everything here except §4 is
indifferent to that. §4 is one enum value and one route alias, cheap to
revise or remove.

**The tools gap may make §4 unsatisfying in practice.** It is accepted
deliberately, and §7 records what to do about it.

**`models_compat` invites growth.** It is one enum with one value, not a
general compatibility framework. A second client wanting a third dialect is a
reason to revisit §7, not to add a value.

**`public_url` is one more thing to set.** It is the only way to emit a
`baseURL` without trusting a request header, and the endpoint is off until it
is set, so forgetting it is visible rather than wrong.
