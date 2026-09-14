# Model Metadata and Client Bootstrap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A user given a base URL and an API key gets every model Mellomting serves, with correct context and output limits, without hand-maintaining a model list.

**Architecture:** Discovery captures each backend's `max_model_len` and reconciles it across replicas; config records it as `policy.context_length`; `/v1/models` emits `context_length` and `max_output_tokens`. opencode v2 discovers Mellomting through an opt-in vLLM-dialect mode; opencode stable gets an authenticated endpoint that emits its provider block. A per-model `policy.body` injects request defaults so reasoning variants can be aliases.

**Tech Stack:** Go standard library only (existing rule, PLAN §88). No new dependencies. Tests are `testing` table tests matching the neighbouring files.

**Spec:** `docs/MODEL_METADATA_DESIGN.md` — read it first; each task cites its section.

## Global Constraints

- Go standard library; no new dependency (AGENTS.md, PLAN §88).
- Match neighbouring code and comment density (AGENTS.md "Writing"). Comments explain why, not what.
- Every change passes `make check` (gofmt, build, vet incl. `-tags integration`, test, `-race`, staticcheck under `GOOS=linux`, govulncheck).
- Commit messages: imperative, lower-case subject, no attribution trailers (memory rule).
- The backend answering discovery is untrusted: every new parsed field is bounded and fails closed (PLAN D7).
- `/v1/models` and the new endpoint never expose a backend URL, backend name, upstream model ID, filesystem path, or the caller's key.
- Nothing rewrites an existing config file. `config discover` prints; it does not write.
- Context window bound: integer in `[1, 1<<31)`. `policy.body` bound: 4096 bytes serialized.
- Owned request keys, rejected in `policy.body`: `model`, `stream`, `stream_options`, `max_tokens`, `max_completion_tokens`, `max_output_tokens`.

---

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `internal/discovery/discovery.go` | Bounded `/v1/models` parse, per-server fetch, replica aggregation | `Model` type; `ParseModels`, `Fetch` return `[]Model`; `Aggregate` takes `map[string][]Model`, fills `Result.ContextLength` |
| `internal/discovery/discovery_test.go` | Discovery tests | Helpers return IDs from `[]Model`; new cases |
| `internal/testsupport/testsupport.go` | Fake servers for tests | `FakeModelsServer` gains a variant that emits `max_model_len` |
| `internal/config/config.go` | Config types | `ModelPolicy.ContextLength`, `ModelPolicy.Body`; `Server.ModelsCompat`, `Server.PublicURL` |
| `internal/config/validate.go` | Validation | New checks in `validateServer`, `validateModels`; `ownedBodyKeys` |
| `internal/config/config_test.go` | Config tests | New rejection cases; show-effective renders `body` |
| `cmd/mellomting/init.go` | `init` | `discoverInitServers` returns `[]Model`; `renderModelsDoc` extracted; writes `context_length` |
| `cmd/mellomting/discover.go` (new) | `config discover` | Parses `-server`, runs discovery, prints `models:` |
| `cmd/mellomting/main.go` | CLI dispatch | `config discover` routed |
| `cmd/mellomting/init_test.go`, `discover_test.go` (new) | CLI tests | Golden config; discover output |
| `internal/httpapi/models.go` | `/v1/models` | Emits limits; compat `owned_by` and `max_model_len` |
| `internal/httpapi/clientconfig.go` (new) | `/client-config/opencode` | Renders the stable provider block |
| `internal/httpapi/server.go` | Routing | `/health` alias under compat; client-config route when `public_url` set; `allowFor` |
| `internal/httpapi/server_test.go`, `clientconfig_test.go` (new) | HTTP tests | Shapes, ACL, routing |
| `internal/proxy/proxy.go` | Request preparation | `policy.body` injection in `prepareOutbound` |
| `internal/proxy/accounting_test.go` | Injection tests | `policy.body` cases beside the cap tests |
| `docs/CONFIGURATION.md`, `docs/OPERATIONS.md`, `README.md` | Docs | New fields, subcommand, both opencode setups |

Task order is the spec's: spike first (decides whether Task 8 is built), then bottom-up from parse to endpoint, docs last. Tasks 2–7 and 9–11 are useful on their own.

---

### Task 1: Spike — verify the opencode assumptions (user runs, throwaway)

**Files:** none committed. Everything below is written under `/tmp/mellomting-spike/` on the machine that runs opencode.

**Interfaces:** Produces answers to the five §4 questions and confirms the §6 block shape. Task 8 is built only if answers 1 and 2 are good.

**Decision it makes:** if v2 discovery does not send the bearer token on `/v1/models`, or a `capabilities` override replaces the discovered card, **skip Task 8** and note it in the design's §4.

- [ ] **Step 1: Write the fake server**

`/tmp/mellomting-spike/main.go`. It requires a bearer key on `/v1/models` and `/v1/chat/completions`, answers `/health` with an empty 200 like vLLM, logs every request's method, path, whether `Authorization` was present, and the chat body, and serves one generation card with `max_model_len` and one without.

```go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

const key = "spike-key"

func main() {
	owned := "vllm"
	if len(os.Args) > 1 {
		owned = os.Args[1] // pass "mellomting" to test the owned_by filter
	}
	auth := func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer "+key
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s auth=%v", r.Method, r.URL.Path, r.Header.Get("Authorization") != "")
		w.WriteHeader(200)
	})
	http.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s auth=%v", r.Method, r.URL.Path, r.Header.Get("Authorization") != "")
		if !auth(r) {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[
{"id":"with-len","object":"model","created":1,"owned_by":%q,"max_model_len":196608},
{"id":"no-len","object":"model","created":1,"owned_by":%q},
{"id":"embed","object":"model","created":1,"owned_by":"mellomting"}]}`, owned, owned)
	})
	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		log.Printf("%s %s auth=%v body=%s", r.Method, r.URL.Path, r.Header.Get("Authorization") != "", body)
		if !auth(r) {
			w.WriteHeader(401)
			return
		}
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"c1","object":"chat.completion","created":1,"model":"with-len","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"with-len","choices":[{"index":0,"delta":{"role":"assistant","content":"pong"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"with-len","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("UNEXPECTED %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
	})
	log.Printf("listening on :8999, key=%s, owned_by=%s", key, owned)
	log.Fatal(http.ListenAndServe("127.0.0.1:8999", nil))
}
```

Run: `cd /tmp/mellomting-spike && go mod init spike && go run . vllm`

- [ ] **Step 2: Stable opencode (1.18.x) — confirm the §6 block shape**

Isolate opencode's state so nothing touches the real config:

```sh
export S=/tmp/mellomting-spike/oc-stable
mkdir -p $S/config/opencode $S/data
export XDG_CONFIG_HOME=$S/config XDG_DATA_HOME=$S/data XDG_CACHE_HOME=$S/cache XDG_STATE_HOME=$S/state
```

Write `$S/config/opencode/opencode.json` — this is exactly what Task 12's endpoint will emit:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "mellomting-spike": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Mellomting (spike)",
      "options": { "baseURL": "http://127.0.0.1:8999/v1" },
      "models": {
        "with-len": { "name": "with-len", "limit": { "context": 196608, "output": 32768 },
          "variants": [ { "id": "think", "body": { "chat_template_kwargs": { "thinking": true } } } ] },
        "no-len":   { "name": "no-len" }
      }
    }
  }
}
```

Then: `opencode auth login` → *Other* → provider id `mellomting-spike` → key `spike-key`. Then `opencode run --model mellomting-spike/with-len "ping"` and `opencode run --model "mellomting-spike/with-len#think" "ping"`.

Record: (a) `auth=true` on the chat request; (b) the body of the `#think` request contains `"chat_template_kwargs":{"thinking":true}`; (c) `opencode models` lists both.

Expected: all three hold. If (b) fails, note the actual body — §5's client-side path depends on it.

- [ ] **Step 3: v2 opencode — the five §4 questions**

Install the v2 line into an isolated prefix (dist-tag may be `beta` or `next`; check `npm view opencode-ai dist-tags` and pick the one whose `--version` prints a 2.x or the v2 docs' shape):

```sh
export S=/tmp/mellomting-spike/oc-v2
mkdir -p $S/npm $S/config/opencode $S/data
npm install --prefix $S/npm --cache $S/npmcache opencode-ai@beta
export PATH=$S/npm/node_modules/.bin:$PATH
export XDG_CONFIG_HOME=$S/config XDG_DATA_HOME=$S/data XDG_CACHE_HOME=$S/cache XDG_STATE_HOME=$S/state
opencode --version
```

`$S/config/opencode/opencode.json`:

```json
{
  "providers": {
    "mellomting-a": { "package": "@opencode/ai/providers/vllm", "settings": { "baseURL": "http://127.0.0.1:8999/v1" } },
    "mellomting-b": { "package": "@opencode/ai/providers/vllm", "settings": { "baseURL": "http://127.0.0.1:8999/v1" } }
  }
}
```

Run `opencode`, `/connect`, pick `mellomting-a`, paste `spike-key`; repeat for `mellomting-b`. Then `/models`.

Record, from the fake server's log and the `/models` list:

| # | Question | Where to look | Good answer |
|---|---|---|---|
| 1 | Bearer sent on `/v1/models`? | log line for `/v1/models` shows `auth=true` | yes |
| 2 | Override merges or replaces? | add `"models": {"with-len": {"capabilities": {"tools": true}}}` under `mellomting-a`, restart; is `with-len`'s context still 196608 in `/models`? | merges |
| 3 | Card without `max_model_len`? | is `no-len` listed, and with what context | listed |
| 4 | `/health` body-sensitive? | log shows `/health` hit, then `/v1/models` proceeds | status only |
| 5 | Any variants on discovered models? | `/models` detail for `with-len` | none |

Also: restart with `go run . mellomting` and confirm `/models` shows nothing — that proves the `owned_by` filter and that embedding models stay hidden.

- [ ] **Step 4: Report**

Paste the five answers plus Step 2's three checks into the conversation. If 1 or 2 is bad, the executor skips Task 8 and adds one line to the design's §4 saying which answer sank it and on which opencode build.

---

### Task 2: `discovery.Model` and the bounded `max_model_len` parse

**Files:**
- Modify: `internal/discovery/discovery.go:27-190` (`ParseModels`, `modelEntry`, `parseData`)
- Modify: `internal/discovery/discovery_test.go:22-46` (helpers), new tests
- Test: `internal/discovery/discovery_test.go`

**Interfaces:**
- Produces: `type Model struct { ID string; ContextLength int }`; `func ParseModels(r io.Reader, maxBytes int64, maxModels int) ([]Model, error)` sorted by ID; `const MaxContextLength = 1<<31 - 1`.
- `Fetch` (Task 3) and the helpers change to `[]Model`.

- [ ] **Step 1: Make the existing helpers compile against `[]Model` so the current tests keep passing unchanged**

In `discovery_test.go` replace `parseString`:

```go
func parseString(t *testing.T, s string, maxBytes int64, maxModels int) []string {
	t.Helper()
	models, err := ParseModels(strings.NewReader(s), maxBytes, maxModels)
	if err != nil {
		t.Fatalf("ParseModels: %v", err)
	}
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	return ids
}

// parseModels is parseString keeping the whole Model, for the
// context-length cases.
func parseModels(t *testing.T, s string) []Model {
	t.Helper()
	models, err := ParseModels(strings.NewReader(s), 1<<20, 1024)
	if err != nil {
		t.Fatalf("ParseModels: %v", err)
	}
	return models
}
```

- [ ] **Step 2: Write the failing tests**

Append to `discovery_test.go`:

```go
func TestParseModelsContextLength(t *testing.T) {
	cases := []struct {
		name string
		card string
		want int
	}{
		{"max_model_len", `{"id":"m","object":"model","max_model_len":196608}`, 196608},
		{"context_length fallback", `{"id":"m","object":"model","context_length":4096}`, 4096},
		{"max_model_len wins over context_length", `{"id":"m","object":"model","max_model_len":8,"context_length":9}`, 8},
		{"absent is unknown", `{"id":"m","object":"model"}`, 0},
		{"upper bound", `{"id":"m","object":"model","max_model_len":2147483647}`, 2147483647},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseModels(t, `{"object":"list","data":[`+tc.card+`]}`)
			if len(got) != 1 || got[0].ID != "m" || got[0].ContextLength != tc.want {
				t.Fatalf("got %+v, want ContextLength %d", got, tc.want)
			}
		})
	}
}

func TestParseModelsRejectsContextLength(t *testing.T) {
	cases := []struct {
		name string
		card string
	}{
		{"zero", `{"id":"m","object":"model","max_model_len":0}`},
		{"negative", `{"id":"m","object":"model","max_model_len":-1}`},
		{"float", `{"id":"m","object":"model","max_model_len":4096.0}`},
		{"string", `{"id":"m","object":"model","max_model_len":"4096"}`},
		{"too large", `{"id":"m","object":"model","max_model_len":2147483648}`},
		{"null", `{"id":"m","object":"model","max_model_len":null}`},
		{"bad fallback", `{"id":"m","object":"model","context_length":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parseStringErr(t, `{"object":"list","data":[`+tc.card+`]}`, 1<<20, 1024, "context length")
		})
	}
}
```

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/discovery/ -run 'TestParseModels' -v 2>&1 | tail -20`
Expected: compile error — `Model` undefined, `ParseModels` returns `[]string`.

- [ ] **Step 4: Implement**

In `discovery.go`, after `MaxModelIDBytes`:

```go
// MaxContextLength bounds a discovered context window. The card is
// untrusted, so a value that does not fit a 32-bit signed integer is a
// malformed response, not a large model.
const MaxContextLength = 1<<31 - 1

// Model is one discovered model: its ID and, when the server reports one,
// its context window in tokens. ContextLength is 0 when unknown.
type Model struct {
	ID            string
	ContextLength int
}
```

Change `ParseModels`'s signature and body:

```go
func ParseModels(r io.Reader, maxBytes int64, maxModels int) ([]Model, error) {
	// ... unchanged reads and bounds ...
	models, err := parseModelList(bytes.NewReader(data), maxModels)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(models, func(a, b Model) int { return strings.Compare(a.ID, b.ID) })
	return models, nil
}
```

`modelEntry` grows two raw fields; `parseModelList` and `parseData` return `[]Model`:

```go
type modelEntry struct {
	Object        string          `json:"object"`
	ID            json.RawMessage `json:"id"`
	MaxModelLen   json.RawMessage `json:"max_model_len"`
	ContextLength json.RawMessage `json:"context_length"`
}
```

In `parseData`, after the ID is validated:

```go
		n, err := decodeContextLength(entry.MaxModelLen, entry.ContextLength)
		if err != nil {
			return nil, err
		}
		models = append(models, Model{ID: id, ContextLength: n})
```

And the decoder, next to `decodeJSONString`:

```go
// decodeContextLength reads the card's context window: vLLM's
// max_model_len, else the context_length spelling OpenRouter and LiteLLM
// use. Absent is unknown (0). Present means a JSON integer in
// [1, MaxContextLength]; anything else fails the response, because the
// server is untrusted and coercion would record a window it never claimed.
func decodeContextLength(primary, fallback json.RawMessage) (int, error) {
	raw := primary
	if len(raw) == 0 {
		raw = fallback
	}
	if len(raw) == 0 {
		return 0, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var num json.Number
	if err := dec.Decode(&num); err != nil {
		return 0, errors.New("discovery response: context length is invalid")
	}
	n, err := num.Int64()
	if err != nil || n < 1 || n > MaxContextLength {
		return 0, errors.New("discovery response: context length is invalid")
	}
	return int(n), nil
}
```

`json.Number` decoding of `"4096"` (a JSON string) succeeds in Go — guard it: check `raw[0] == '"'` first and reject. Add before the decoder:

```go
	if raw[0] == '"' || string(raw) == "null" {
		return 0, errors.New("discovery response: context length is invalid")
	}
```

- [ ] **Step 5: Fix `Fetch`'s return and the fuzz target**

`Fetch` (line ~380) returns `([]Model, error)`; its body already just returns `ParseModels`'s result. `FuzzParseModels` ignores the value; it compiles as is.

- [ ] **Step 6: Run the package tests**

Run: `go test ./internal/discovery/ 2>&1 | tail -5`
Expected: PASS. `TestFetch*` compile because they compare IDs through the helpers; if any compares `[]string` directly, map through `.ID` the same way.

- [ ] **Step 7: Commit**

```sh
git add internal/discovery/
git commit -m "capture the context window at discovery

ParseModels returns Model{ID, ContextLength} instead of bare IDs, reading
vLLM's max_model_len and falling back to context_length. The value is
bounded and fails closed like the ID: the server answering discovery is
not trusted, and coercing a float or a string would record a window the
backend never claimed. Absent stays unknown; not every backend reports
one."
```

---

### Task 3: `Aggregate` reconciles context windows

**Files:**
- Modify: `internal/discovery/discovery.go:553-625` (`Result`, `Aggregate`)
- Modify: `cmd/mellomting/init.go:203-217` (`discoverInitServers`)
- Test: `internal/discovery/discovery_test.go`

**Interfaces:**
- Consumes: `Model` from Task 2.
- Produces: `func Aggregate(ordered []Server, discovered map[string][]Model) (Result, error)`; `Result.ContextLength map[string]int` (absent key = unknown); `discoverInitServers` returns `map[string][]discovery.Model`.

- [ ] **Step 1: Update the existing `TestAggregate` inputs**

Every `map[string][]string{"a": {"x", "y"}, ...}` literal becomes `map[string][]Model{"a": {{ID: "x"}, {ID: "y"}}, ...}`. Add a helper at the top of the file:

```go
func ids(s ...string) []Model {
	out := make([]Model, len(s))
	for i, id := range s {
		out[i] = Model{ID: id}
	}
	return out
}
```

and write the literals as `"a": ids("x", "y")`.

- [ ] **Step 2: Write the failing test**

```go
func TestAggregateContextLength(t *testing.T) {
	ordered := []Server{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	got, err := Aggregate(ordered, map[string][]Model{
		"a": {{ID: "m", ContextLength: 8192}, {ID: "n"}, {ID: "o", ContextLength: 100}},
		"b": {{ID: "m", ContextLength: 4096}, {ID: "n"}},
		"c": {{ID: "m"}, {ID: "o", ContextLength: 50}},
	})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	want := map[string]int{"m": 4096, "o": 50}
	if !reflect.DeepEqual(got.ContextLength, want) {
		t.Fatalf("ContextLength = %v, want %v (min of known; all-unknown omitted; unknown replica does not veto)", got.ContextLength, want)
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/discovery/ -run TestAggregate -v 2>&1 | tail -10`
Expected: compile error on `map[string][]Model` argument.

- [ ] **Step 4: Implement**

```go
type Result struct {
	Models        map[string][]string
	ContextLength map[string]int // reconciled window per model; absent when no replica reports one
}
```

In `Aggregate`, change the parameter type and the inner loop:

```go
	models := make(map[string][]string)
	ctx := make(map[string]int)
	for _, name := range serverOrder {
		entries := discovered[name]
		if len(entries) > MaxModels {
			return Result{}, fmt.Errorf("discovery: server %q exceeds %d models", name, MaxModels)
		}
		seenInServer := make(map[string]bool, len(entries))
		for _, m := range entries {
			if m.ID == "" {
				return Result{}, fmt.Errorf("discovery: server %q has an empty model ID", name)
			}
			if seenInServer[m.ID] {
				return Result{}, fmt.Errorf("discovery: server %q has duplicate model %q", name, m.ID)
			}
			seenInServer[m.ID] = true
			models[m.ID] = append(models[m.ID], name)
			// Minimum of the known windows: a request may land on any
			// replica and must fit on all of them. A replica that
			// reports none does not veto the others.
			if m.ContextLength > 0 {
				if cur, ok := ctx[m.ID]; !ok || m.ContextLength < cur {
					ctx[m.ID] = m.ContextLength
				}
			}
		}
	}
	// ... MaxUniqueModels check unchanged ...
	return Result{Models: models, ContextLength: ctx}, nil
```

Update `discoverInitServers` in `init.go`: `results := make(map[string][]discovery.Model, len(servers))` and the return type.

- [ ] **Step 5: Run tests for both packages**

Run: `go build ./... && go test ./internal/discovery/ ./cmd/mellomting/ 2>&1 | tail -5`
Expected: PASS. `TestRenderInitArtifacts` still passes because `discovery.Result{Models: ...}` literals leave `ContextLength` nil.

- [ ] **Step 6: Commit**

```sh
git add internal/discovery/ cmd/mellomting/init.go
git commit -m "reconcile discovered context windows across replicas

Aggregate takes the minimum of the windows replicas report, because a
request may be routed to any of them and must fit on all. A replica that
reports none does not veto the rest; a model no replica sizes carries no
window. Too high fails a request with an unretried backend 400, too low
only compacts early, so the minimum is the safe side."
```

---

### Task 4: `policy.context_length` in config

**Files:**
- Modify: `internal/config/config.go:279-282` (`ModelPolicy`)
- Modify: `internal/config/validate.go:667-669`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `ModelPolicy.ContextLength int` with tag `yaml:"context_length,omitempty"`.

- [ ] **Step 1: Write the failing tests**

Add cases to `TestParseRejectsInvalidConfig`'s table (it loads full YAML; copy the shape of the existing `multi-document` case for the boilerplate):

```go
		{
			name: "negative context_length",
			yaml: validConfigWithPolicy("context_length: -1"),
			wantErr: "models.m1.policy.context_length: must be >= 0",
		},
		{
			name: "output exceeds context",
			yaml: validConfigWithPolicy("context_length: 100\n      max_output_tokens: 101"),
			wantErr: "models.m1.policy.max_output_tokens: must not exceed context_length",
		},
```

and the helper, next to the test:

```go
// validConfigWithPolicy is the smallest loadable config with the given
// lines under models.m1.policy.
func validConfigWithPolicy(policy string) string {
	return `
version: 1
server:
  listen:
    network: unix
    address: /run/mellomting/mellomting.sock
    mode: "0660"
auth:
  users_file: /etc/mellomting/users.yaml
  pepper_file: /etc/mellomting/pepper
servers:
  qa:
    url: http://127.0.0.1:8001
models:
  m1:
    servers:
    - qa
    policy:
      ` + policy + `
`
}
```

Also a positive case in whichever test loads a valid config and inspects it (e.g. the effective-source test): `context_length: 4096` round-trips and appears in `show-effective` output as `context_length: 4096`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/config/ -run TestParseRejectsInvalidConfig -v 2>&1 | grep -E 'context|FAIL' | head`
Expected: FAIL — unknown field `context_length` (strict decoder), not the wanted message.

- [ ] **Step 3: Implement**

`config.go`:

```go
// ModelPolicy is per-model request policy.
type ModelPolicy struct {
	// ContextLength is the model's window in tokens, as discovered or
	// as the operator states it. 0 means unknown; it is then omitted
	// from /v1/models rather than guessed.
	ContextLength   int `yaml:"context_length,omitempty"`
	MaxOutputTokens int `yaml:"max_output_tokens,omitempty"`
}
```

`validate.go`, replacing the single `MaxOutputTokens < 0` check:

```go
		if m.Policy.ContextLength < 0 {
			errs = append(errs, prefix+".policy.context_length: must be >= 0")
		}
		if m.Policy.MaxOutputTokens < 0 {
			errs = append(errs, prefix+".policy.max_output_tokens: must be >= 0")
		}
		if m.Policy.ContextLength > 0 && m.Policy.MaxOutputTokens > m.Policy.ContextLength {
			errs = append(errs, prefix+".policy.max_output_tokens: must not exceed context_length")
		}
```

- [ ] **Step 4: Run**

Run: `go test ./internal/config/ 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/config/
git commit -m "add policy.context_length

Records a model's window so it can be published to clients. An explicit
value always wins over what discovery found. max_output_tokens may not
exceed it: nothing related the two fields before, so a config claiming
more output than context passed silently."
```

---

### Task 5: `init` writes the discovered window

**Files:**
- Modify: `cmd/mellomting/init.go:586-650` (`renderInitConfig`)
- Test: `cmd/mellomting/init_test.go:511-560`

**Interfaces:**
- Produces: `func renderModelsDoc(discovered discovery.Result) map[string]any` — the `models:` mapping, reused by Task 6.

- [ ] **Step 1: Write the failing test**

Add a subtest to `TestRenderInitArtifacts`, modelled on `loopback config fixture`:

```go
	t.Run("discovered context length", func(t *testing.T) {
		withInitPepper(t, deterministicPepper)
		dir := newCommitDir(t)
		cfgPath := filepath.Join(dir, "config.yaml")
		args, err := parseInitArguments(cfgPath, defaultInitListen, "required", false, false, []string{"http://127.0.0.1:8000"})
		if err != nil {
			t.Fatal(err)
		}
		discovered := discovery.Result{
			Models:        map[string][]string{"alpha": {"local"}, "beta": {"local"}},
			ContextLength: map[string]int{"alpha": 196608},
		}
		got, err := renderInitArtifacts(args, discovered)
		if err != nil {
			t.Fatal(err)
		}
		want := `models:
    alpha:
        policy:
            context_length: 196608
        servers:
            - local
        type: generation
    beta:
        servers:
            - local
        type: generation
`
		if !strings.Contains(string(got.Config), want) {
			t.Fatalf("config =\n%s\nwant to contain\n%s", got.Config, want)
		}
	})
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/mellomting/ -run 'TestRenderInitArtifacts/discovered' -v 2>&1 | tail -15`
Expected: FAIL — `policy:` absent.

- [ ] **Step 3: Implement**

Extract the models mapping from `renderInitConfig` into:

```go
// renderModelsDoc builds the models: mapping init writes and config
// discover prints: every discovered model as a generation model over its
// replicas, carrying the reconciled context window when one is known.
func renderModelsDoc(discovered discovery.Result) map[string]any {
	models := make(map[string]any, len(discovered.Models))
	for _, name := range discovered.SortedModels() {
		replicas := append([]string(nil), discovered.Models[name]...)
		m := map[string]any{
			"type":    "generation",
			"servers": replicas,
		}
		if n, ok := discovered.ContextLength[name]; ok && n > 0 {
			m["policy"] = map[string]any{"context_length": n}
		}
		models[name] = m
	}
	return models
}
```

and in `renderInitConfig` replace the loop with `models := renderModelsDoc(discovered)`.

- [ ] **Step 4: Run**

Run: `go test ./cmd/mellomting/ -run TestRenderInitArtifacts 2>&1 | tail -3`
Expected: PASS, both subtests.

- [ ] **Step 5: Commit**

```sh
git add cmd/mellomting/
git commit -m "write the discovered context window from init"
```

---

### Task 6: `mellomting config discover`

**Files:**
- Create: `cmd/mellomting/discover.go`
- Modify: `cmd/mellomting/main.go:73-118` (`configCmd`)
- Test: `cmd/mellomting/discover_test.go` (new)

**Interfaces:**
- Consumes: `parseInitServers`, `discovery.DerivePolicy`, `discoverInitServers`, `discovery.Aggregate`, `renderModelsDoc`.
- Produces: `func runDiscover(ctx context.Context, w io.Writer, rawServers []string) error` — the testable core; `configCmd` routes `discover` to it.

- [ ] **Step 1: Write the failing test**

`cmd/mellomting/discover_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"mellomting/internal/testsupport"
)

func TestRunDiscover(t *testing.T) {
	t.Run("prints the models block", func(t *testing.T) {
		srv := testsupport.FakeModelsServerWithContext(t, map[string]int{"alpha": 196608, "beta": 0})
		var out bytes.Buffer
		if err := runDiscover(context.Background(), &out, []string{"local=" + srv.URL}); err != nil {
			t.Fatalf("runDiscover: %v", err)
		}
		want := `models:
    alpha:
        policy:
            context_length: 196608
        servers:
            - local
        type: generation
    beta:
        servers:
            - local
        type: generation
`
		if out.String() != want {
			t.Fatalf("output =\n%s\nwant\n%s", out.String(), want)
		}
	})

	t.Run("unreachable server names itself and prints nothing", func(t *testing.T) {
		var out bytes.Buffer
		err := runDiscover(context.Background(), &out, []string{"gone=http://127.0.0.1:1"})
		if err == nil || !strings.Contains(err.Error(), "server gone") {
			t.Fatalf("err = %v, want it to name the server", err)
		}
		if out.Len() != 0 {
			t.Fatalf("printed %q on failure", out.String())
		}
	})
}
```

Add to `internal/testsupport/testsupport.go`, beside `FakeModelsServer`:

```go
// FakeModelsServerWithContext is FakeModelsServer whose cards carry
// max_model_len when the value is non-zero, for discovery tests that read
// the context window.
func FakeModelsServerWithContext(t *testing.T, models map[string]int) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var b strings.Builder
		b.WriteString(`{"object":"list","data":[`)
		first := true
		for _, id := range slices.Sorted(maps.Keys(models)) {
			if !first {
				b.WriteString(",")
			}
			first = false
			if n := models[id]; n > 0 {
				fmt.Fprintf(&b, `{"id":%q,"object":"model","max_model_len":%d}`, id, n)
			} else {
				fmt.Fprintf(&b, `{"id":%q,"object":"model"}`, id)
			}
		}
		b.WriteString(`]}`)
		_, _ = fmt.Fprint(w, b.String())
	}))
	t.Cleanup(ts.Close)
	return ts
}
```

(`maps` and `slices` from the standard library; add the imports.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/mellomting/ -run TestRunDiscover -v 2>&1 | tail -5`
Expected: compile error — `runDiscover` undefined.

- [ ] **Step 3: Implement**

`cmd/mellomting/discover.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"mellomting/internal/discovery"
)

// runDiscover is `config discover`: the discovery half of init, printed
// instead of written. It exists because init is create-only, so a model
// added after the first bootstrap would otherwise have its window looked
// up by hand.
func runDiscover(ctx context.Context, w io.Writer, rawServers []string) error {
	servers, err := parseInitServers(rawServers)
	if err != nil {
		return err
	}
	policy, err := discovery.DerivePolicy(servers)
	if err != nil {
		return err
	}
	results, err := discoverInitServers(ctx, servers, policy)
	if err != nil {
		return err
	}
	aggregate, err := discovery.Aggregate(servers, results)
	if err != nil {
		return err
	}
	out, err := yaml.Marshal(map[string]any{"models": renderModelsDoc(aggregate)})
	if err != nil {
		return fmt.Errorf("render models: %w", err)
	}
	_, err = w.Write(out)
	return err
}

// discoverCmd parses `config discover -server NAME=URL ...` and runs it.
func discoverCmd(rest []string) int {
	fs := commandFlags("config discover", "Discover models on inference servers and print the models: block.")
	var servers serverList
	fs.Var(&servers, "server", "inference server as NAME=URL (repeatable)")
	if err := parseCommandFlags(fs, rest); err != nil {
		return flagExitCode(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "mellomting: config discover: unexpected arguments %q\n", fs.Args())
		return 2
	}
	if len(servers) == 0 {
		fmt.Fprintln(os.Stderr, "mellomting: config discover: at least one -server is required")
		return 2
	}
	if err := runDiscover(context.Background(), os.Stdout, servers); err != nil {
		fmt.Fprintf(os.Stderr, "mellomting: config discover: %v\n", err)
		return 1
	}
	return 0
}
```

`discoverInitServers` already wraps errors as `server %s: %w`, which satisfies the "names the server" test. Check `serverList` is the flag type `init` uses for `-server` (`init.go:54-60`); reuse it unchanged.

In `main.go`'s `configCmd`, route before the existing switch:

```go
	sub, rest := subcommand(args, "check")
	if sub == "discover" {
		return discoverCmd(rest)
	}
```

and extend the help text: `"  discover        Discover models on servers and print the models: block"`, and the unknown-subcommand message to `(want check, show-effective or discover)`.

- [ ] **Step 4: Run**

Run: `go test ./cmd/mellomting/ -run 'TestRunDiscover|TestConfigCmd' 2>&1 | tail -3`
Expected: PASS. If a `TestConfigCmd` asserts the exact help text, update its expectation.

- [ ] **Step 5: Try it by hand against a fake, then commit**

Run: `go run ./cmd/mellomting config discover -server local=http://127.0.0.1:1` → expect `mellomting: config discover: server local: ...` on stderr, exit 1, nothing on stdout.

```sh
git add cmd/mellomting/ internal/testsupport/
git commit -m "add config discover

init is create-only, so the window it captures helps exactly once. This
runs the same discovery and aggregation against the servers named on the
command line and prints the models: block; the operator pastes what they
want. It writes nothing."
```

---

### Task 7: `/v1/models` emits `context_length` and `max_output_tokens`

**Files:**
- Modify: `internal/httpapi/models.go`
- Test: `internal/httpapi/server_test.go` (beside `TestModelsACLFiltering`)

**Interfaces:**
- Consumes: `config.ModelPolicy.ContextLength`, `.MaxOutputTokens`; `config.Model.Type`.
- Produces: `func modelCard(name string, m config.Model, compat string, created int64) map[string]any` — Task 8 adds the compat branch.

- [ ] **Step 1: Write the failing test**

```go
func TestModelsLimits(t *testing.T) {
	t.Parallel()
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}, func(c *config.Config) {
		m := c.Models["model-a"]
		m.Policy = config.ModelPolicy{ContextLength: 196608, MaxOutputTokens: 32768}
		c.Models["model-a"] = m
		e := c.Models["model-b"]
		e.Type = "embedding"
		e.Policy = config.ModelPolicy{ContextLength: 8192, MaxOutputTokens: 5}
		c.Models["model-b"] = e
	}, auth.KeyLimits{})
	w := e.do(t, http.MethodGet, "/v1/models", "bearer2", "")
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("body: %v", err)
	}
	byID := map[string]map[string]any{}
	for _, d := range list.Data {
		byID[d["id"].(string)] = d
	}
	a := byID["model-a"]
	if a["context_length"] != float64(196608) || a["max_output_tokens"] != float64(32768) {
		t.Fatalf("model-a = %v", a)
	}
	if a["owned_by"] != "mellomting" {
		t.Fatalf("owned_by = %v", a["owned_by"])
	}
	b := byID["model-b"]
	if b["context_length"] != float64(8192) {
		t.Fatalf("embedding context = %v", b["context_length"])
	}
	if _, ok := b["max_output_tokens"]; ok {
		t.Fatalf("embedding model must not report max_output_tokens: %v", b)
	}
	for _, k := range []string{"root", "permission", "parent", "max_model_len"} {
		if _, ok := a[k]; ok {
			t.Fatalf("field %q must not be emitted by default", k)
		}
	}
}

func TestModelsOmitsUnknownLimits(t *testing.T) {
	t.Parallel()
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}, nil, auth.KeyLimits{})
	w := e.do(t, http.MethodGet, "/v1/models", "bearer2", "")
	if strings.Contains(w.Body.String(), "context_length") || strings.Contains(w.Body.String(), "max_output_tokens") {
		t.Fatalf("unset limits must be omitted: %s", w.Body.String())
	}
}
```

Check the model names `buildEnv` creates (`server_test.go:40-100`): the ACL test uses `model-a` and a second model; use those two names.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/httpapi/ -run 'TestModelsLimits|TestModelsOmits' -v 2>&1 | tail -8`
Expected: `TestModelsLimits` FAIL on `context_length`; `TestModelsOmitsUnknownLimits` passes already.

- [ ] **Step 3: Implement**

`models.go`:

```go
func (s *Server) handleModels(w http.ResponseWriter, key *auth.Key) {
	names := s.router.List() // sorted by the router
	data := make([]map[string]any, 0, len(names))
	for _, name := range names {
		if !key.Allows(name) {
			continue
		}
		data = append(data, modelCard(name, s.cfg.Models[name], s.cfg.Server.ModelsCompat, s.startedUnix))
	}
	// ... rest unchanged ...
}

// modelCard is one /v1/models entry. Limits are omitted when unknown so
// the response never asserts a window it does not have; max_output_tokens
// is the policy cap prepare enforces, and does not apply to embeddings.
func modelCard(name string, m config.Model, compat string, created int64) map[string]any {
	card := map[string]any{
		"id":       name,
		"object":   "model",
		"created":  created,
		"owned_by": "mellomting",
	}
	if n := m.Policy.ContextLength; n > 0 {
		card["context_length"] = n
	}
	if n := m.Policy.MaxOutputTokens; n > 0 && m.Type != "embedding" {
		card["max_output_tokens"] = n
	}
	return card
}
```

`ModelsCompat` does not exist yet; pass `""` literally for now and wire the field in Task 8. Add the `config` import.

- [ ] **Step 4: Run**

Run: `go test ./internal/httpapi/ 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/httpapi/models.go internal/httpapi/server_test.go
git commit -m "publish context and output limits on /v1/models

context_length and max_output_tokens, the spelling OpenRouter and
LiteLLM use. Each is omitted when unknown; the output cap is the policy
prepare enforces, and is omitted for embedding models where it does not
apply. Nothing from the backend's own card is forwarded."
```

---

### Task 8: `server.models_compat: vllm` and `/health` — only if the spike passed

**Files:**
- Modify: `internal/config/config.go` (`Server`), `internal/config/validate.go:222` (`validateServer`)
- Modify: `internal/httpapi/models.go` (`modelCard`), `internal/httpapi/server.go:229-240, 352-363`
- Test: `internal/config/config_test.go`, `internal/httpapi/server_test.go`

**Interfaces:**
- Produces: `config.Server.ModelsCompat string` tag `yaml:"models_compat,omitempty"`, values `""` or `"vllm"`.

- [ ] **Step 1: Write the failing config tests**

In `TestParseRejectsInvalidConfig`, add (reusing the shape of `validConfigWithPolicy` but under `server:`; write a sibling `validConfigWithServer(lines string)` helper the same way):

```go
		{name: "unknown models_compat", yaml: validConfigWithServer("models_compat: ollama"), wantErr: `server.models_compat: "ollama" must be empty or vllm`},
```

and a positive: `models_compat: vllm` loads.

- [ ] **Step 2: Write the failing HTTP tests**

```go
func TestModelsVLLMCompat(t *testing.T) {
	t.Parallel()
	e := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}, func(c *config.Config) {
		c.Server.ModelsCompat = "vllm"
		m := c.Models["model-a"]
		m.Policy.ContextLength = 4096
		c.Models["model-a"] = m
		b := c.Models["model-b"]
		b.Type = "embedding"
		c.Models["model-b"] = b
	}, auth.KeyLimits{})
	w := e.do(t, http.MethodGet, "/v1/models", "bearer2", "")
	var list struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	for _, d := range list.Data {
		switch d["id"] {
		case "model-a":
			if d["owned_by"] != "vllm" || d["max_model_len"] != float64(4096) || d["context_length"] != float64(4096) {
				t.Fatalf("generation card under compat = %v", d)
			}
		case "model-b":
			if d["owned_by"] != "mellomting" {
				t.Fatalf("embedding must keep owned_by mellomting so the filter drops it: %v", d)
			}
		}
	}
}

func TestHealthAliasUnderCompat(t *testing.T) {
	t.Parallel()
	off := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {}, nil, auth.KeyLimits{})
	if w := off.do(t, http.MethodGet, "/health", "", ""); w.Code != 404 {
		t.Fatalf("/health without compat = %d, want 404", w.Code)
	}
	on := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {}, func(c *config.Config) {
		c.Server.ModelsCompat = "vllm"
	}, auth.KeyLimits{})
	if w := on.do(t, http.MethodGet, "/health", "", ""); w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("/health under compat = %d %q, want 200 ok", w.Code, w.Body.String())
	}
	if w := on.do(t, http.MethodPost, "/health", "", ""); w.Code != 405 {
		t.Fatalf("POST /health under compat = %d, want 405", w.Code)
	}
}
```

Confirm what `e.do` sends when the key argument is `""` — it must send no `Authorization` header for the health case; if it always sends one, use whatever the existing `/healthz` test does (search `"/healthz"` in `server_test.go`).

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/config/ ./internal/httpapi/ -run 'models_compat|VLLMCompat|HealthAlias' 2>&1 | tail -8`
Expected: compile error on `ModelsCompat`.

- [ ] **Step 4: Implement**

`config.go`, in `Server` after `TrustedProxies`:

```go
	// ModelsCompat makes /v1/models speak another server's dialect so a
	// client with built-in discovery for it can find Mellomting. "vllm"
	// reports generation models as owned_by vllm, adds max_model_len
	// beside context_length, and serves /health as an alias of /healthz.
	// It is opt-in because the claim is the operator's: Mellomting
	// fronts whatever is configured, which need not be vLLM.
	ModelsCompat string `yaml:"models_compat,omitempty"`
```

`validate.go`, in `validateServer`:

```go
	switch s.ModelsCompat {
	case "", "vllm":
	default:
		errs = append(errs, fmt.Sprintf("server.models_compat: %q must be empty or vllm", s.ModelsCompat))
	}
```

`models.go`, in `modelCard` after the limits:

```go
	if compat == "vllm" && m.Type != "embedding" {
		// opencode's vLLM discovery keeps only cards owned by vllm and
		// reads max_model_len; embeddings stay unclaimed so it drops
		// them instead of listing them as chat models.
		card["owned_by"] = "vllm"
		if n := m.Policy.ContextLength; n > 0 {
			card["max_model_len"] = n
		}
	}
```

and replace the `""` placeholder from Task 7 with `s.cfg.Server.ModelsCompat`.

`server.go`, health switch in `routeBody`:

```go
		switch r.URL.Path {
		case "/healthz":
			writeHealth(w, http.StatusOK, "ok")
			return
		case "/health":
			if s.cfg.Server.ModelsCompat == "vllm" {
				writeHealth(w, http.StatusOK, "ok")
				return
			}
		case "/readyz":
```

and in `allowFor`, the health branch:

```go
	case path == "/healthz", path == "/readyz":
		return "GET"
	case path == "/health":
		if s.cfg.Server.ModelsCompat == "vllm" {
			return "GET"
		}
		return ""
```

(Restructure the existing combined `case` so the inference paths and the health paths are separate cases; keep the POST answers for the inference paths.)

- [ ] **Step 5: Run**

Run: `go test ./internal/config/ ./internal/httpapi/ 2>&1 | tail -4`
Expected: PASS.

- [ ] **Step 6: Commit**

```sh
git add internal/config/ internal/httpapi/
git commit -m "add opt-in vLLM-dialect compatibility for /v1/models

opencode v2 discovers vLLM servers by probing /health, reading
/v1/models, keeping cards owned by vllm, and taking max_model_len as the
context limit. server.models_compat: vllm makes Mellomting answer that
way for generation models; embeddings stay owned by mellomting so the
filter drops them rather than listing them as chat models. Opt-in,
because the claim is the operator's to make."
```

---

### Task 9: `policy.body` — validation

**Files:**
- Modify: `internal/config/config.go` (`ModelPolicy`), `internal/config/validate.go` (`validateModels`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `ModelPolicy.Body map[string]any` tag `yaml:"body,omitempty"`; `const MaxPolicyBodyBytes = 4096`; `var ownedBodyKeys`.

- [ ] **Step 1: Write the failing tests**

```go
func TestPolicyBodyValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"scalar", "body: 42", "models.m1.policy.body: must be a mapping"},
		{"owned key model", "body:\n        model: x", `models.m1.policy.body: "model" is set by mellomting`},
		{"owned key stream_options", "body:\n        stream_options: {}", `models.m1.policy.body: "stream_options" is set by mellomting`},
		{"owned key max_tokens", "body:\n        max_tokens: 1", `models.m1.policy.body: "max_tokens" is set by mellomting`},
		{"too large", "body:\n        pad: " + strings.Repeat("x", 4100), "models.m1.policy.body: exceeds 4096 bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(validConfigWithPolicy(tc.body)))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
	t.Run("valid body round-trips through show-effective", func(t *testing.T) {
		_, out, err := LoadEffectiveBytes([]byte(validConfigWithPolicy("body:\n        chat_template_kwargs:\n          thinking: true")))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), "thinking: true") {
			t.Fatalf("effective config lacks body:\n%s", out)
		}
	})
}
```

Use whichever byte-level entry points `config_test.go` already uses for parsing and effective rendering (search for how `TestParseRejectsInvalidConfig` calls into the package and for the show-effective test); substitute those names for `Parse` / `LoadEffectiveBytes`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/config/ -run TestPolicyBodyValidation -v 2>&1 | tail -8`
Expected: FAIL — unknown field `body`.

- [ ] **Step 3: Implement**

`config.go`:

```go
type ModelPolicy struct {
	ContextLength   int `yaml:"context_length,omitempty"`
	MaxOutputTokens int `yaml:"max_output_tokens,omitempty"`
	// Body is merged into the outbound request at the top level, each key
	// only when the client did not send it. It lets a public model alias
	// carry request defaults — a thinking variant as its own model — for
	// every client, with nothing configured on the client.
	Body map[string]any `yaml:"body,omitempty"`
}
```

`validate.go`:

```go
// MaxPolicyBodyBytes bounds policy.body serialized as JSON. It is a
// defaults map, not a prompt.
const MaxPolicyBodyBytes = 4096

// ownedBodyKeys are request fields the proxy itself sets or reads;
// letting config inject them would silently break routing, the output
// cap, or usage accounting.
var ownedBodyKeys = []string{"model", "stream", "stream_options", "max_tokens", "max_completion_tokens", "max_output_tokens"}

func validatePolicyBody(prefix string, body map[string]any) []string {
	if body == nil {
		return nil
	}
	var errs []string
	for _, k := range ownedBodyKeys {
		if _, ok := body[k]; ok {
			errs = append(errs, fmt.Sprintf("%s.policy.body: %q is set by mellomting", prefix, k))
		}
	}
	enc, err := json.Marshal(body)
	if err != nil {
		errs = append(errs, prefix+".policy.body: must encode as JSON")
	} else if len(enc) > MaxPolicyBodyBytes {
		errs = append(errs, fmt.Sprintf("%s.policy.body: exceeds %d bytes", prefix, MaxPolicyBodyBytes))
	}
	return errs
}
```

called from `validateModels` after the policy checks: `errs = append(errs, validatePolicyBody(prefix, m.Policy.Body)...)`.

The "must be a mapping" case: the strict YAML decoder already refuses a scalar into `map[string]any` — check the message it produces and either match it in the test or wrap the decode error in `parse` to the wanted text. Do not weaken the decoder.

- [ ] **Step 4: Run**

Run: `go test ./internal/config/ 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/config/
git commit -m "add policy.body with its validation

A mapping merged into the outbound request where the client sent
nothing, so a public alias can carry request defaults. Keys the proxy
sets or reads are refused at load: injecting model, stream, the cap
fields or stream_options from config would silently break routing,
the cap, or accounting. Bounded at 4 KiB."
```

---

### Task 10: `policy.body` — injection

**Files:**
- Modify: `internal/proxy/proxy.go:642-670` (`prepare`), `:1197-1250` (`prepareOutbound`)
- Test: `internal/proxy/accounting_test.go` (beside `TestOutputCapInjectWhenAbsent`)

**Interfaces:**
- Consumes: `config.ModelPolicy.Body`.
- Changes: `prepareOutbound(fields, o, cap, body map[string]any, stream, ensureUsage, configuredReservation)` — one added parameter; every caller (grep `prepareOutbound(`) passes the model's `Policy.Body` or `nil`.

- [ ] **Step 1: Write the failing tests**

```go
func TestPolicyBodyInjectedWhenAbsent(t *testing.T) {
	f := newFakeVLLM(t, okJSON)
	cfg := testConfig(f.server.URL)
	cfg.Models["gen-1"] = config.Model{
		Type: "generation", Strategy: "single",
		Policy:   config.ModelPolicy{Body: map[string]any{"chat_template_kwargs": map[string]any{"thinking": true}, "temperature": 0.2}},
		Backends: []config.BackendRef{{Name: "b1", Weight: 1}},
	}
	router, _ := routing.New(cfg, nil)
	client, _ := backend.New(backend.Options{
		Name: "b1", Cfg: cfg.Backends["b1"], Network: backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: testsupport.DiscardLogger(),
	})
	p, _ := New(cfg, router, map[string]*backend.Client{"b1": client}, testsupport.DiscardLogger(), nil, nil, nil)

	// Absent: injected.
	w := run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","messages":[{"role":"user","content":"hi"}]}`, testKey())
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var got map[string]json.RawMessage
	_ = json.Unmarshal(f.lastBody, &got)
	if string(got["chat_template_kwargs"]) != `{"thinking":true}` || string(got["temperature"]) != `0.2` {
		t.Fatalf("injected body = %s", f.lastBody)
	}

	// Present: client's value, byte for byte, nested content included.
	w = run(t, p, http.MethodPost, "/v1/chat/completions",
		`{"model":"gen-1","messages":[{"role":"user","content":"hi"}],"chat_template_kwargs":{"thinking":false,"x":1}}`, testKey())
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(f.lastBody, &got)
	if string(got["chat_template_kwargs"]) != `{"thinking":false,"x":1}` {
		t.Fatalf("client value overridden: %s", got["chat_template_kwargs"])
	}
}

func TestPolicyBodyInjectedForEmbeddings(t *testing.T) {
	f := newFakeVLLM(t, okJSON)
	cfg := testConfig(f.server.URL)
	cfg.Models["emb-1"] = config.Model{
		Type: "embedding", Strategy: "single",
		Policy:   config.ModelPolicy{Body: map[string]any{"encoding_format": "float"}},
		Backends: []config.BackendRef{{Name: "b1", Weight: 1}},
	}
	router, _ := routing.New(cfg, nil)
	client, _ := backend.New(backend.Options{
		Name: "b1", Cfg: cfg.Backends["b1"], Network: backend.Policy{Mode: "loopback-only"},
		MaxResponseBytes: cfg.Server.MaxResponseBytes, Log: testsupport.DiscardLogger(),
	})
	p, _ := New(cfg, router, map[string]*backend.Client{"b1": client}, testsupport.DiscardLogger(), nil, nil, nil)
	w := run(t, p, http.MethodPost, "/v1/embeddings", `{"model":"emb-1","input":"hi"}`, testKey())
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(string(f.lastBody), `"encoding_format":"float"`) {
		t.Fatalf("embeddings did not get the default: %s", f.lastBody)
	}
}
```

Check `okJSON` suits an embeddings response in this test file; if the embeddings path validates the upstream shape, use whatever fixture the existing embeddings tests use.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/proxy/ -run 'TestPolicyBody' -v 2>&1 | tail -8`
Expected: FAIL — `chat_template_kwargs` absent from the backend body.

- [ ] **Step 3: Implement**

`prepare`:

```go
	cap := 0
	var body map[string]any
	if m, ok := p.cfg.Models[t.model]; ok {
		cap = m.Policy.MaxOutputTokens
		body = m.Policy.Body
	}
	reservation, injected, err := prepareOutbound(
		t.fields, o, cap, body, t.stream, p.ensureStreamUsage(q.Key), p.cfg.Accounting.UnknownUsageReservation)
```

`prepareOutbound`, new parameter and the injection *before* the early return:

```go
func prepareOutbound(fields map[string]json.RawMessage, o operation, cap int, body map[string]any, stream, ensureUsage bool, configuredReservation int64) (reservation int64, injectedUsage bool, err error) {
	// Model defaults (policy.body): each key only when the client did
	// not send it, and never a key the proxy owns — validation refused
	// those at load. Runs first so embeddings and usage-injection-off
	// requests get their defaults too.
	for k, v := range body {
		if _, present := fields[k]; present {
			continue
		}
		enc, merr := json.Marshal(v)
		if merr != nil {
			return 0, false, errNotNormal
		}
		fields[k] = enc
	}

	if o.capField == "" && !(stream && ensureUsage) {
		// ... unchanged ...
```

Every other caller of `prepareOutbound` (tests included; `grep -n 'prepareOutbound(' internal/proxy/`) gains a `nil` argument in the new position.

- [ ] **Step 4: Run**

Run: `go test ./internal/proxy/ 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/proxy/
git commit -m "inject policy.body defaults into the outbound request

Each key only when the client did not send it, before the early return
so embeddings get theirs too. A client-sent key is forwarded byte for
byte, nested content included: the alias supplies defaults, not policy,
the same rule the output cap already follows."
```

---

### Task 11: `server.public_url`

**Files:**
- Modify: `internal/config/config.go` (`Server`), `internal/config/validate.go` (`validateServer`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Server.PublicURL string` tag `yaml:"public_url,omitempty"`; validated as `http`/`https`, host, optional port, nothing else. `func PublicURLLabel(u string) string` — first host label sanitized, for Task 12's provider ID.

- [ ] **Step 1: Write the failing tests**

```go
func TestPublicURLValidation(t *testing.T) {
	bad := map[string]string{
		"path":     "public_url: https://h.example/v1",
		"query":    "public_url: https://h.example?x=1",
		"userinfo": "public_url: https://u:p@h.example",
		"scheme":   "public_url: ftp://h.example",
		"no host":  "public_url: https://",
	}
	for name, line := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(validConfigWithServer(line)))
			if err == nil || !strings.Contains(err.Error(), "server.public_url") {
				t.Fatalf("err = %v, want server.public_url error", err)
			}
		})
	}
	for _, line := range []string{"public_url: https://h.example", "public_url: http://10.0.0.1:8080", "public_url: https://h.example/"} {
		if _, err := Parse([]byte(validConfigWithServer(line))); err != nil {
			t.Fatalf("%s: %v", line, err)
		}
	}
}

func TestPublicURLLabel(t *testing.T) {
	cases := map[string]string{
		"https://home.example":       "home",
		"https://Work_GPU.corp:8443": "work-gpu",
		"http://10.17.160.10:8080":   "10-17-160-10",
		"https://[::1]:8080":         "--1",
	}
	for in, want := range cases {
		if got := PublicURLLabel(in); got != want {
			t.Fatalf("PublicURLLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/config/ -run 'TestPublicURL' 2>&1 | tail -5`
Expected: FAIL — unknown field / undefined `PublicURLLabel`.

- [ ] **Step 3: Implement**

`config.go`, in `Server`:

```go
	// PublicURL is the origin clients reach this proxy at, for the
	// client-config endpoint to put in a baseURL. Scheme and host only.
	// Unset means the endpoint is not served: Mellomting never derives
	// its own address from a request's Host header.
	PublicURL string `yaml:"public_url,omitempty"`
```

`validate.go`, in `validateServer`:

```go
	if s.PublicURL != "" {
		u, err := url.Parse(s.PublicURL)
		switch {
		case err != nil, u.Scheme != "http" && u.Scheme != "https":
			errs = append(errs, "server.public_url: must be an http or https origin")
		case u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/"):
			errs = append(errs, "server.public_url: must be scheme://host[:port] with no path, query, or userinfo")
		}
	}
```

and, exported, next to it:

```go
// PublicURLLabel is the first label of public_url's host, lowercased,
// with anything outside [a-z0-9-] replaced by '-'. It names the provider
// a client config derives from this server, so two servers with different
// hosts get different IDs without further configuration.
func PublicURLLabel(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}
```

(`u.Hostname()` strips brackets from an IPv6 literal, so `[::1]` gives `::1` → `--1`, matching the test; the test pins that behaviour rather than pretending IPv6 hosts make good labels.)

- [ ] **Step 4: Run and commit**

Run: `go test ./internal/config/ 2>&1 | tail -3` → PASS.

```sh
git add internal/config/
git commit -m "add server.public_url

The origin clients reach the proxy at, for a client config to name as
its baseURL. Scheme and host only. Unset means the endpoint that needs
it is not served: the alternative is reflecting a request's Host header
into a URL, which the proxy never does."
```

---

### Task 12: `GET /client-config/opencode`

**Files:**
- Create: `internal/httpapi/clientconfig.go`
- Modify: `internal/httpapi/server.go:306-309` (dispatch), `:352-363` (`allowFor`)
- Test: `internal/httpapi/clientconfig_test.go` (new)

**Interfaces:**
- Consumes: `config.PublicURLLabel`, `config.Server.PublicURL`, `modelCard`-adjacent data (`ContextLength`, `MaxOutputTokens`, `Type`), `key.Allows`.
- Produces: `func (s *Server) handleClientConfigOpencode(w http.ResponseWriter, key *auth.Key)`; `func opencodeProviderBlock(publicURL string, names []string, models map[string]config.Model) map[string]any`.

- [ ] **Step 1: Write the failing tests**

```go
package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"mellomting/internal/auth"
	"mellomting/internal/config"
)

func TestClientConfigOpencodeGolden(t *testing.T) {
	names := []string{"deepseek-v4-flash", "embed", "nolimits"}
	models := map[string]config.Model{
		"deepseek-v4-flash": {Type: "generation", Policy: config.ModelPolicy{ContextLength: 196608, MaxOutputTokens: 32768}},
		"embed":             {Type: "embedding", Policy: config.ModelPolicy{ContextLength: 8192, MaxOutputTokens: 9}},
		"nolimits":          {Type: "generation"},
	}
	got, _ := json.Marshal(opencodeProviderBlock("https://home.example", names, models))
	want := `{"$schema":"https://opencode.ai/config.json","provider":{"mellomting-home":{"models":{"deepseek-v4-flash":{"limit":{"context":196608,"output":32768},"name":"deepseek-v4-flash"},"embed":{"limit":{"context":8192},"name":"embed"},"nolimits":{"name":"nolimits"}},"name":"Mellomting (home)","npm":"@ai-sdk/openai-compatible","options":{"baseURL":"https://home.example/v1"}}}}`
	if string(got) != want {
		t.Fatalf("block =\n%s\nwant\n%s", got, want)
	}
}

func TestClientConfigOpencodeRouting(t *testing.T) {
	t.Parallel()
	off := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {}, nil, auth.KeyLimits{})
	if w := off.do(t, http.MethodGet, "/client-config/opencode", "bearer2", ""); w.Code != 404 {
		t.Fatalf("without public_url = %d, want 404", w.Code)
	}
	on := buildEnv(t, func(w http.ResponseWriter, r *http.Request) {}, func(c *config.Config) {
		c.Server.PublicURL = "https://home.example"
	}, auth.KeyLimits{})
	if w := on.do(t, http.MethodGet, "/client-config/opencode", "", ""); w.Code != 401 {
		t.Fatalf("without key = %d, want 401", w.Code)
	}
	if w := on.do(t, http.MethodPost, "/client-config/opencode", "bearer2", ""); w.Code != 405 {
		t.Fatalf("POST = %d, want 405", w.Code)
	}
	w := on.do(t, http.MethodGet, "/client-config/opencode", "bearer", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GET = %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	if !strings.Contains(body, `"model-a"`) || strings.Contains(body, `"model-b"`) {
		t.Fatalf("ACL not applied: %s", body)
	}
	if strings.Contains(body, "bearer") {
		t.Fatalf("key echoed: %s", body)
	}
}
```

The ACL assertion assumes `bearer` (the first test key) may use only `model-a`, as in `TestModelsACLFiltering`; confirm the second model's name in `buildEnv`. The "without key" case sends `""` — as in Task 8, match how the existing tests send an unauthenticated request.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/httpapi/ -run TestClientConfig 2>&1 | tail -5`
Expected: compile error — `opencodeProviderBlock` undefined.

- [ ] **Step 3: Implement**

`clientconfig.go`:

```go
package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"mellomting/internal/auth"
	"mellomting/internal/config"
)

// handleClientConfigOpencode answers GET /client-config/opencode: the
// provider block a stable (1.x) opencode needs, for the models this key
// may use. Stable opencode has no discovery, so this is the paste that
// replaces a hand-written model list. It carries the same data as
// /v1/models and the operator's public_url, and never the key.
func (s *Server) handleClientConfigOpencode(w http.ResponseWriter, key *auth.Key) {
	var names []string
	for _, name := range s.router.List() {
		if key.Allows(name) {
			names = append(names, name)
		}
	}
	buf, err := json.Marshal(opencodeProviderBlock(s.cfg.Server.PublicURL, names, s.cfg.Models))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "api_error", "internal", "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

// opencodeProviderBlock renders the stable opencode provider block.
// The provider ID derives from public_url's host so two servers get two
// IDs; limit keys are omitted when unknown, and the limit object when
// both are; the output limit does not apply to embeddings.
func opencodeProviderBlock(publicURL string, names []string, models map[string]config.Model) map[string]any {
	label := config.PublicURLLabel(publicURL)
	entries := make(map[string]any, len(names))
	for _, name := range names {
		m := models[name]
		entry := map[string]any{"name": name}
		limit := map[string]any{}
		if n := m.Policy.ContextLength; n > 0 {
			limit["context"] = n
		}
		if n := m.Policy.MaxOutputTokens; n > 0 && m.Type != "embedding" {
			limit["output"] = n
		}
		if len(limit) > 0 {
			entry["limit"] = limit
		}
		entries[name] = entry
	}
	return map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{
			"mellomting-" + label: map[string]any{
				"npm":     "@ai-sdk/openai-compatible",
				"name":    "Mellomting (" + label + ")",
				"options": map[string]any{"baseURL": strings.TrimSuffix(publicURL, "/") + "/v1"},
				"models":  entries,
			},
		},
	}
}
```

`server.go`, dispatch — add a case beside `/v1/models`:

```go
	case r.Method == http.MethodGet && r.URL.Path == "/client-config/opencode" && s.cfg.Server.PublicURL != "":
		s.handleClientConfigOpencode(w, key)
		return
```

`allowFor`:

```go
	case path == "/client-config/opencode":
		if s.cfg.Server.PublicURL != "" {
			return "GET"
		}
		return ""
```

Check how `routeBody` reaches the 404 for unknown paths when `PublicURL` is empty — the route must fall through to the existing "Everything else: 404" so the surface is unchanged.

- [ ] **Step 4: Run**

Run: `go test ./internal/httpapi/ 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/httpapi/
git commit -m "serve the opencode provider block at /client-config/opencode

Stable opencode has no model discovery, so a user pastes a provider
block instead. This emits it for the models the caller's key may use,
with limits from config and the baseURL from server.public_url; the
key itself is never echoed. Off, and 404 like any unknown path, until
public_url is set."
```

---

### Task 13: Documentation and the full gate

**Files:**
- Modify: `docs/CONFIGURATION.md` (fields: `policy.context_length`, `policy.body`, `server.models_compat`, `server.public_url`; subcommand `config discover`)
- Modify: `docs/OPERATIONS.md` (new section "Connecting opencode")
- Modify: `README.md` (one paragraph + link)

**Interfaces:** none.

- [ ] **Step 1: `docs/CONFIGURATION.md`**

Add each field where its section lives, matching the surrounding entries' format (look at how `max_output_tokens` is documented and mirror it). Content, compressed to that style:

- `policy.context_length` — window in tokens; discovered by `init`/`config discover` from `max_model_len`, minimum across replicas; explicit value wins; published on `/v1/models`; `max_output_tokens` may not exceed it.
- `policy.body` — mapping injected into the request where the client sent nothing; the alias example from the design's §5; owned keys refused; 4 KiB.
- `server.models_compat: vllm` — what changes, that it is opt-in, and why.
- `server.public_url` — origin only; enables `/client-config/opencode`.
- `config discover -server NAME=URL` — prints the `models:` block, writes nothing.

- [ ] **Step 2: `docs/OPERATIONS.md` — "Connecting opencode"**

Two subsections. **Stable (1.x):** set `server.public_url`; the user runs `curl -H "Authorization: Bearer $KEY" $URL/client-config/opencode`, pastes into `opencode.json`, runs `opencode auth login` → Other → the provider ID from the block → key. New models: fetch again. Variants: the DeepSeek `chat_template_kwargs` example from the design's §5, and that `policy.body` aliases are the no-client-config alternative. **v2:** set `models_compat: vllm`; the `providers` block from the design's §4; `/connect`; the tools caveat with the one-line `capabilities.tools: true` fix and the backend-flags precondition (`--enable-auto-tool-choice --tool-call-parser`). If Task 8 was skipped, write only the stable section and say v2 uses the same block under `providers`/`package`/`settings` until discovery is verified.

- [ ] **Step 3: `README.md`**

One paragraph under whatever section introduces clients: models and their limits are published on `/v1/models`; opencode users paste `/client-config/opencode` (stable) or turn on `models_compat: vllm` (v2). Link to the operations section.

- [ ] **Step 4: Full gate**

Run: `make check 2>&1 | tail -15`
Expected: every line `ok`, staticcheck silent, `No vulnerabilities found.`

- [ ] **Step 5: Commit**

```sh
git add docs/ README.md
git commit -m "document model limits, request defaults, and the opencode setups"
```

---

## Self-review

**Spec coverage.** §1 → Tasks 2–3. §2 → Tasks 4–6. §3 → Task 7. §4 → Task 8 (gated by Task 1). §5 client side → Task 13; server side → Tasks 9–10. §6 → Tasks 11–12. §7 is deferred by design and has no task. Error-handling rows: each maps to a validation case (4, 8, 9, 11), a routing case (8, 12), or `discoverInitServers`'s existing wrapping (6). Testing list: every bullet has a test in the task that owns it.

**Type consistency.** `discovery.Model{ID, ContextLength}` (Task 2) is what `Aggregate` (3) and `FakeModelsServerWithContext` (6, via `Fetch`) produce and consume. `Result.ContextLength map[string]int` (3) is what `renderModelsDoc` (5) reads. `ModelPolicy.ContextLength`/`.MaxOutputTokens`/`.Body` (4, 9) are what `modelCard` (7, 8), `prepareOutbound` (10) and `opencodeProviderBlock` (12) read. `Server.ModelsCompat` (8) and `Server.PublicURL` (11) are read by `server.go` in 8 and 12. `PublicURLLabel` (11) is called in 12.

**Placeholders.** None: every test and implementation step carries its code. Three steps tell the executor to check a neighbouring name before relying on it (`buildEnv` model names, how `e.do` sends no key, the byte-level parse entry point in `config_test.go`) — those are lookups, not gaps.

**Order.** Task 1 gates 8 only. Tasks 2→3→5→6 are a dependency chain; 4 before 5; 7 before 8; 9 before 10; 11 before 12; 13 last.
