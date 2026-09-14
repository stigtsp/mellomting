# Model metadata and client bootstrap

Status: design, not yet implemented. Date: 2026-09-14.

## Goal

A user given a base URL and an API key gets every model that Mellomting
serves, with correct context and output limits, without editing a model list.
Adding a model to Mellomting makes it appear in their client.

The reference client is opencode v2. Sections 1–3 are client-agnostic; §4 is
an opt-in compatibility mode that lets opencode discover Mellomting with no
software shipped from this repo.

## Non-goals

- Tool-calling, vision, reasoning or cost metadata. Mellomting cannot observe
  tool support, and declaring it per model is config that must be kept true by
  hand.
- Clients other than opencode. The `/v1/models` fields follow the convention
  other clients are converging on, but no other client is targeted or tested.
- Shipping an npm package. Deferred, with the trigger recorded in §5.

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

## What opencode v2 provides

- No generic `/v1/models` discovery for custom OpenAI-compatible providers.
  Models must be listed explicitly.
- Built-in discovery for vLLM: probes `/health`, reads `/v1/models`, keeps
  only cards with `owned_by: "vllm"`, and maps `max_model_len` to
  `limit.context`.
- The `@opencode/ai/providers/vllm` package is reusable under any provider ID,
  so several servers can each be their own provider with their own base URL
  and credential.
- Credentials come from `/connect`, stored in
  `~/.local/share/opencode/auth.json`. Multiple instances of one provider are
  supported, with a custom name asked at connect time.
- Discovered vLLM models start with **tools disabled**, because discovery
  cannot infer tool support. No provider-level default to re-enable them was
  found; the override appears to be per model.

## Decisions

| Decision | Chosen | Rejected |
|---|---|---|
| How opencode learns the models | Opt-in vLLM-dialect compatibility | Ship a plugin now; third-party discovery plugin; masquerade unconditionally |
| Context window source | Capture at discovery, config overrides | Hand-written only; live passthrough per request |
| Discovery after init | Read-only `config discover` prints the block | Init-only capture; a command that edits the config |
| Metadata scope | Limits only | Capabilities; full models.dev card |
| Replica disagreement | Minimum of known values | Fail init; omit |
| Credentials | opencode's `/connect` store; nothing in `opencode.json` or the environment | Env vars; a key in the config file |

The compatibility mode is opt-in because `owned_by: "vllm"` is not true in
general: Mellomting fronts whatever the operator configures, which need not be
vLLM. An operator turning on a documented compatibility knob is making that
claim deliberately for their own deployment. Emitting it by default would be
Mellomting lying about itself.

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
carries none. A replica that reports nothing does not veto the others;
the recorded value is the best available information, not a guarantee, and an
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
$ mellomting config discover local=http://10.17.160.10:8000
models:
  deepseek-v4-flash:
    type: generation
    policy:
      context_length: 196608
    servers: [local]
```

It runs the same discovery and aggregation as init against the servers named
on the command line, prints the `models:` block, and writes nothing — the
operator pastes what they want. It shares init's server-argument parsing and
its derived backend-network policy, and adds no new network behaviour.

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

## 4. Opt-in vLLM-dialect compatibility

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

A user then needs no plugin and no shipped software:

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
tool-capable. This is the reason §5 exists.

Tool calling is a property of the backend, not of Mellomting: vLLM only emits
structured `tool_calls` when launched with `--enable-auto-tool-choice` and
`--tool-call-parser`, and without them a request carrying `tool_choice: "auto"`
is rejected upstream. Mellomting forwards the `tools` array unchanged and
cannot observe whether those flags were passed, so it cannot answer the
question opencode is being conservative about. Adding a `policy.tools` field
would not help under this mode either: opencode's vLLM discovery reads only
`max_model_len` from the card and would ignore it. Only §5 removes the
per-model line.

**Open questions for the spike**, any of which can sink §4:

- Whether the vLLM discovery sends the `/connect` credential as a bearer
  token **on the `/v1/models` fetch itself**, not only on completions.
  vLLM's own listing is normally open; Mellomting's answers 401 without a
  key. If discovery fetches anonymously, §4 cannot work at all.
- Whether a `models` entry overriding `capabilities` *merges with* the
  discovered card or *replaces* it. If it replaces, the same line also
  discards the discovered `limit.context`, the operator is back to
  hand-writing context windows, and most of §4's value is lost.
- What discovery does with a card that has no `max_model_len` (context
  unknown, so omitted): skip the model, apply a default, or fail.
- Whether the `/health` probe checks only the status. vLLM answers an empty
  200; `writeHealth` answers JSON.

A bad answer to the first two promotes §5 from deferred to required.

## 5. Deferred: a first-party plugin

An `opencode-mellomting` plugin would register each server as a provider via
`ctx.provider.transform`, resolve credentials through
`ctx.integration.connection`, and **omit** the capabilities block — which
triggers opencode's assume-tools fallback and removes the per-model edit that
§4 requires. It would also drop the dependence on another provider's
`owned_by` filter and give the providers real names.

It is deferred because it costs a TypeScript package published from a Go repo,
against a v2 plugin API that is new and whose auth flow this design could not
verify against a running opencode.

Revisit when either holds:

- The per-model `capabilities.tools` edit becomes a routine annoyance, or
  users hit it without understanding why a model will not call tools.
- opencode changes the `owned_by` filter, the `/health` probe, or the
  `max_model_len` mapping, breaking §4.

## Data flow

```
vLLM /v1/models (max_model_len)
  -> discovery.ParseModels      bounded parse, per server
  -> discovery.Aggregate        min across replicas
  -> mellomting init            writes policy.context_length
  -> config                     operator may override
  -> handleModels               context_length + max_output_tokens, ACL-filtered
                                (+ max_model_len, owned_by: vllm under compat)
  -> opencode vllm provider     limit.context, per provider
  -> /models                    user picks a model
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
| Client unauthenticated at `/v1/models` | Unchanged: 401, no model list |
| Mellomting unreachable from opencode | That provider contributes no models |

## Testing

- `ParseModels`: absent, zero, negative, float, string, `1<<32`, valid;
  `context_length` fallback; a malformed card fails that server.
- `Aggregate`: minimum across disagreeing replicas; all-unknown omits; one
  unknown does not veto the rest.
- `renderInitConfig`: golden config carrying `context_length`.
- `config discover`: prints the same block init would write, writes no file,
  and fails with the server name when one server is unreachable.
- `validateModels`: negative rejected; output exceeding context rejected.
- `validateServer`: `models_compat` accepts unset and `vllm`, rejects others.
- `handleModels`: both fields present; each omitted when zero;
  `max_output_tokens` omitted for embedding models; ACL filtering unchanged;
  no `root`/`permission`/`parent`; under compat, generation models report
  `owned_by: vllm`, embedding models keep `mellomting`, and `max_model_len`
  matches `context_length`.
- Routing: `/health` is 404 by default, 200 under compat, 405 on POST under
  compat, and never counts against inflight admission.
- One end-to-end check: opencode v2 against a running `mellomting serve` with
  compat on, confirming models and limits arrive.

## Risks

**§4 depends on undocumented client behaviour.** The `owned_by` filter, the
`/health` probe and the `max_model_len` mapping are opencode implementation
details, not a published contract, and can change in any release. Step 1 of
implementation verifies them against a real opencode before the knob is built;
§5 is the escape hatch if they move.

**The tools gap may make §4 unsatisfying in practice.** It is accepted
deliberately, and §5 records what to do about it.

**`models_compat` invites growth.** It is one enum with one value, not a
general compatibility framework. A second client wanting a third dialect is a
reason to revisit §5, not to add a value.

## Implementation order

1. Spike: point a real opencode v2 at a hand-faked `/v1/models` that
   requires a bearer key and carries `owned_by: "vllm"` and `max_model_len`,
   with two provider instances and `/connect`. Answer the four open questions
   in §4 in order; the first two decide between §4 and §5. Throwaway.
2. `ParseModels` and `Aggregate` capture and reconcile the window (§1).
3. `ModelPolicy.context_length`, validation, `init` rendering, and
   `config discover` (§2).
4. `handleModels` emits both fields (§3).
5. `server.models_compat` and the `/health` alias (§4).
6. Documentation: `docs/CONFIGURATION.md` for both new fields and the
   subcommand, `README.md` and `docs/OPERATIONS.md` for the opencode setup
   including the tools caveat.

Steps 2–4 are useful on their own: they make the context window visible in
`config show-effective` and to any client, whether or not step 5 ships.
