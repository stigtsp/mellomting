# Mellomting Simplification Plan

Status: **draft for review**. Nothing in this document has been applied.

## 1. Objective

Make the codebase modern, simple, and elegant on Go 1.27 without weakening a
single security property. Three outcomes, in priority order:

1. **Delete what is not needed** — dead code, dead parameters, dead packages,
   and duplication that has already drifted.
2. **Say each thing once** — one redaction function, one error envelope, one
   atomic-write recipe, one error taxonomy, one request outcome.
3. **Adopt the modern idiom** where it is genuinely clearer, and record the
   places where the older form is deliberately kept.

The measure of success is not lines removed. It is that a security reviewer can
read a control once and know it applies everywhere.

## 2. Non-negotiable constraints

These bound every task below. A task that cannot be done within them is not
done.

- The security invariants (PLAN §4) and the fail-closed posture are unchanged.
- **Exactly one JSON re-encode of the request body must survive.** Verified:
  `{"model":"forbidden","stream":true,"model":"allowed"}` decodes last-wins and
  re-encodes to a single `"model":"allowed"`. Forwarding the body verbatim would
  ship both keys to a backend that may resolve them differently — a duplicate-key
  ACL bypass. Any body-handling change keeps one canonicalizing marshal.
- **Exactly one quota settle per request.** Today this is guaranteed by a
  boolean and correct placement across nine return paths. It holds, but the
  structure does not enforce it; task D2 makes it structural. No task may make
  it easier to settle twice or not at all.
- No `sync.Pool`, no cross-request buffer reuse. In a multi-tenant proxy a
  partially-overwritten pooled buffer hands one key's prompt bytes to another.
  The existing no-pool stance is correct and stays.
- No speculative generality: no plugin systems, no interfaces with one
  implementation, no frameworks (PLAN §79, AGENTS.md).
- Operator-facing error strings are part of the contract. Where a task changes
  one, it says so and updates the tests deliberately.

## 3. Method and evidence base

Findings come from four parallel review passes (reuse, simplification,
efficiency, altitude), a mechanical scan against the Go 1.27 guideline set, and
direct verification. Claims marked **[verified]** were reproduced against the
tree at `495ca1c`; everything else is a proposal to be confirmed when the task
is picked up.

Two whole-tree trials were run in a scratch copy, not in the repo:

- **[verified]** `go.mod` `1.26` → `1.27`: `go build`, `go vet`, and the full
  test suite pass unchanged.
- **[verified]** `modernize -fix ./...` on top of that: 96 findings applied
  across 33 files; `go build`, `go vet`, `gofmt -l`, and **the entire test suite
  pass**. This is the evidence that Phase A is mechanical.

## 4. Go 1.27 guideline mapping

From the JetBrains modern-Go guideline set for 1.27. Applicable:

| Guideline | Sites | Task |
|---|---|---|
| `errors_as_type` (`errors.AsType[T]`) | 5 non-test, 2 test | A2/A3 |
| `min_max` | 12 | A2/A3 |
| `range_over_int` | 45 | A2/A3 |
| `loopvar_capture` (redundant copies) | 18 | A3 |
| `slices_contains`, `slices_sort`, `slices_backward` | 4 | A2 |
| `strings_split_seq`, `strings_cut`, `strings_cut_prefix_suffix` | 6 | A2/A3 |
| `sync_waitgroup_go`, `atomic_types`, `testing_t_context` | 5 | A3 |
| `testing_b_loop` (`b.Loop()`) | 12 benchmarks | **A4** — not covered by the tool |
| `new_expression` (`new(true)`) | 4 | **A6** — not covered by the tool |
| `cmp_or` | 49 in `applyDefaults` | **A5** — not covered by the tool |
| `maps_keys_values_iter` + `slices_sorted` | 5 sorted-key loops | A5 |

Deliberately **not** adopted, with reasons to record so they are not revisited:

- **`http_servemux_patterns`** — `httpapi` deliberately uses exact `==` path
  matching and no `ServeMux`, to avoid ServeMux pattern semantics (trailing-slash
  subtree matching, `%2F` handling) on an authorization boundary. Keep.
- **`json_v2`** — the guideline itself says not to migrate existing code unless
  explicitly requested, because an import change alters wire behaviour. The
  accounting JSONL is a documented operator-facing format and the proxy relays
  third-party JSON. **Recommendation: do not migrate.** See §8 decision 1.
- `stdlib_uuid`, `url_clone`, `generic_methods`, `promoted_field_literals`,
  `strings_bytes_cut_last` — **[verified]** no applicable sites exist.

`cmp.Or` caveat, **[verified]** by test: it is a variadic function, so **all
arguments are evaluated eagerly** — no short-circuit. Safe for the constant
defaults in `applyDefaults`; never use it with an expensive or side-effecting
fallback.

## 5. Execution ledger

Complete one row at a time. Do not start a later row while the current row has
uncommitted changes or a failing check.

| Task | Deliverable | Risk | Status |
|---|---|---|---|
| A1 | adopt Go 1.27 in `go.mod` | low | complete |
| A2 | apply `modernize` to non-test code | low | complete |
| A3 | apply `modernize` to test code | low | complete |
| A4 | benchmarks use `b.Loop()` | low | complete |
| A5 | `applyDefaults` via `cmp.Or`; sorted map keys via `maps.Keys` | low | complete |
| A6 | `new(expr)` for pointer defaults | low | complete |
| B1 | fix `retry_count` accounting defect, with tests | low | complete |
| B2 | delete the unreachable qualifier machinery and `internal/guard` | low | complete |
| B3 | delete the dead `dryRun` parameter and `daemon.listen` field | low | complete |
| B4 | reject trailing arguments in `config` and `sandbox` | low-med | complete |
| C1 | one `redactURL` | low | complete |
| C2 | one client-error envelope | low | complete |
| C3 | one atomic file replace | med | complete |
| C4 | one egress-policy builder and one set of listener predicates | low | complete |
| C5 | one `ensureFile` in `internal/systemd` | low | complete |
| C6 | one pepper generator | low | complete |
| C7 | shared `internal/testsupport` | low-med | pending |
| D1 | one backend-error classification | low | pending |
| D2 | one request outcome, settled once | med | pending |
| D3 | split `dispatch` into named phases | med | pending |
| D4 | immutable `operation`; stop logging the response ID | low | pending |
| D5 | table-driven client errors | low | pending |
| D6 | extract the startup consistency checks from `buildDaemon` | low-med | pending |
| E1 | decode and encode the request body once | med | pending |
| E2 | parse each SSE event once | low | pending |
| E3 | reuse the SSE parser buffers | low | pending |
| E4 | one reader goroutine per stream | med | pending |

Per task: read the named files and their tests; implement only that
deliverable; `gofmt`; run the task's verification command; run `go test ./...`
and `go test -race` on affected packages; inspect the full diff; commit; mark
the row complete.

---

## Phase A — Foundation

Mechanical and verified as a whole. A2 and A3 are one tool invocation split
into two commits so the non-test diff is reviewable on its own.

### A1. Adopt Go 1.27

`go.mod`: `go 1.26` → `go 1.27`. **[verified]** clean.

### A2/A3. Apply `modernize`

```sh
go run golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest -fix ./...
```

96 findings, 25 in non-test code and 71 in tests, across `internal/proxy` (6),
`cmd/mellomting` (6), `routing`/`limiter`/`httpapi`/`backend` (3 each),
`accounting` (2), and one each in `systemd`, `logging`, `landlock`,
`discovery`, `config`, `auth`. Commit non-test and test separately.

Note the tool correctly leaves `b.N` loops alone — those are A4.

### A4. `b.Loop()` in benchmarks

12 sites across `accounting`, `proxy`, `limiter`, `auth`, `backend`, `routing`
bench files. `for i := 0; i < b.N; i++` → `for b.Loop()`. Not a mechanical
rename: `b.Loop()` also removes the need for manual timer control, so any
`b.ResetTimer` around the loop should be re-examined.

### A5. `cmp.Or` and `maps.Keys`

`internal/config/validate.go:76` `applyDefaults` is **190 lines** containing
**49** `if x == zero { x = default }` stanzas — a 4:1 ratio of boilerplate to
information. Each becomes one line:

```go
c.Auth.UsersFile = cmp.Or(c.Auth.UsersFile, DefaultUsersFile)
```

**[verified]** `cmp.Or` works with the named `Duration` type, with strings, and
with `*bool` when combined with `new(true)`. Expect ~190 → ~75 lines.

The genuinely conditional blocks stay as they are: the `a.Enabled` gate
(`:141`), the strategy inference (`:252`), and the unix-mode gate (`:119`).

Separately, five hand-rolled "collect map keys then sort" loops
(`cmd/mellomting/main.go:563`, `internal/config/source.go:157`,
`internal/discovery/discovery.go`, `internal/routing/router.go:148`) become
`slices.Sorted(maps.Keys(m))`.

### A6. `new(expr)`

4 sites of `func() *bool { b := true; return &b }()` in proxy tests become
`new(true)`. Go 1.26+.

---

## Phase B — Defect and dead code

### B1. `retry_count` is always zero on failure paths

**[verified] — this is a live defect, not a style issue.**

`out.retries` is assigned only at `internal/proxy/proxy.go:555` and `:568`,
both success paths, but is read by the deferred logger at `:177`. Every failure
path therefore logs `retry_count: 0` even after the retry budget was exhausted.

The two terminal-failure paths also disagree in `usage.jsonl`:

- upstream-status failures (429/5xx/4xx) hit the `X7` early `return` at `:657`
  and fall through to the deferred fallback at `:187`, which passes a
  **hardcoded `0`**;
- connect/timeout/queue-full failures reach `:681` and pass the real `retried`.

**[verified]** zero tests assert `retry_count` or `Record.Retries` anywhere in
the repo, which is why this has been invisible.

Fix the assignment and add the missing assertions **before** D2 restructures
this code, so the refactor has a regression test to preserve.

### B2. Delete the unreachable qualifier machinery and `internal/guard`

**[verified] unreachable by construction.** `sourceDoc`
(`internal/config/source.go:38`) has no `qualifiers` field, so the strict
decoder rejects one as an unknown field; and `normalizeSource` unconditionally
sets `Qualifiers: map[string]Qualifier{}` (`source.go:88`). `len(cfg.Qualifiers)
> 0` can never be true.

Dead as a result: `config.Qualifier`/`QualifierIO`/`Config.Qualifiers`/
`Model.Qualifier`, the qualifier loop in `applyDefaults` (`validate.go:233-245`),
`validateQualifiers` + `qualifierMode` (`:633-689`), the qualifier arm of
`validateModels` (`:746-750`), the `defaultQualifier*` constants, and the
qualifier half of `rejectShelvedFeatures` (`serve.go:225-235`). ~135 lines.

This is worse than merely dead: an auditor reading `validateQualifiers` sees a
real-looking loopback-egress guard at `validate.go:670` and a fail-closed
startup refusal, neither of which can run. `serve_test.go:1107` appears to test
the refusal but actually asserts the *decoder's* `field qualifiers not found`
message.

**[verified]** `internal/guard/doc.go` contains only `package guard` and has
**zero importers**, yet is listed in AGENTS.md and PLAN §78. Delete it.

Keep `source_test.go`'s unknown-field rejection test — it is the live control —
and add a line to PLAN/AGENTS recording that shelved features are refused by the
source schema, not by a runtime check.

### B3. Dead parameter and dead field

**[verified]** `cmd/mellomting/serve.go:101` assigns `d.listen = ln` and
nothing ever reads it. Delete the field and the assignment.

**[verified with a correction to the original finding]**
`commitInitArtifacts`'s `dryRun` parameter (`cmd/mellomting/init.go:626`) is
`false` at the production call site (`:160`) and at 15 of 16 test call sites.
It is **not** entirely unexercised: `init_test.go:1353` ("dry-run writes
nothing") passes `true` and asserts the tree stays empty. Removing the parameter
therefore also removes that test. That is correct — it tests a branch no caller
can reach — but the task must delete both together and say so in the commit.

### B4. `config` and `sandbox` silently ignore trailing arguments

**[verified] by running the built binary:**

```
$ mellomting config check /nonexistent/typo/path.yaml
mellomting: config check failed: /etc/mellomting/config.yaml: ...
```

The path argument is discarded and the **default** config is validated instead.
`init`, `install`, `key`, `serve`, and `usage` all check `NArg()`; `configCmd`
and `sandboxCmd` do not. For a fail-closed validation command this is actively
misleading.

This **changes behaviour** — those two commands start rejecting trailing
arguments — so it needs its own tests and a note in the release notes. `serve`
should also gain the standard `unexpected arguments %q` message; it currently
exits 2 silently.

---

## Phase C — Say each thing once

Import-graph facts that constrain these (verified by `go list -deps`):
`landlock`, `securefile`, `logging`, `version`, `accounting` are leaves;
`config` → `landlock`; `backend` → `config`, `landlock`, `securefile`;
`proxy` → `backend`, `config`, `auth`, `routing`, `accounting`; `httpapi` →
`proxy`. So `config` can never import `backend`/`proxy`, and `proxy` can never
import `httpapi`.

### C1. One `redactURL`

**[verified]** duplicated character-for-character between
`internal/landlock/policy.go:95` and `internal/config/validate.go:790` — and
**already diverged in output**: `***@` versus `<redacted>@`. An operator gets a
different redaction marker for the same malformed `base_url` depending on which
validator reported it.

The test coverage diverged too: `landlock/ports_test.go:29` pins the
`@`-in-path guard (`http://127.0.0.1/a@b` must pass through unchanged);
`config_test.go:1693` does not test that case at all. A regression in config's
guard would *fabricate* a credential marker where none exists, untested.

`url.URL.Redacted()` is not a substitute — it needs a successful parse and masks
only the password; this helper must work on strings that failed to parse.

Two viable homes — see §8 decision 2. Standardise on `<redacted>@` either way
and merge both test tables.

### C2. One client-error envelope

`internal/proxy/proxy.go:1315` `writeError` and `internal/httpapi/errors.go:12`
`writeErr` are behaviourally identical, and between them emit **every**
client-facing error in the product (22 call sites in proxy, 4 in httpapi).

PLAN §72 — never return stack traces, paths, backend hostnames, or secrets — is
enforced by two independent copies. There is no divergence today, which is
precisely why to fix it now. A new leaf `internal/apierr` holding `Write`, the
standard message constants, and the `Retry-After` ceiling, imported by both.

### C3. One atomic file replace

Four hand-rolled implementations, each missing a *different* guarantee:

| | refuses symlink | refuses non-regular | fsync data | fsync dir | chmod via fd |
|---|---|---|---|---|---|
| `auth/store.go:296` | no | no | yes | **yes** | yes |
| `systemd/systemd.go:487` | **yes** | **yes** | yes | no | yes |
| `install.go:133` | **yes** | dir only | yes | no | **no** (by path) |
| `init_commit_linux.go` | yes | yes | yes | yes | yes |

None of the gaps is exploitable today — `rename(2)` does not follow a symlink at
the destination — but "each copy forgot a different thing" is the signature of
duplication, and the next copy will forget a fifth. Notably the systemd unit,
logrotate config, pepper, and users stub can survive a crash as a dangling
rename, because only `auth` fsyncs the parent directory.

Add `securefile.Replace(path, mode, write func(io.Writer) error) error`
implementing the union. `auth.Update` keeps its flock and ownership clamp
*around* the call; `systemd.writeFileAtomic` becomes a wrapper; `installBinary`
keeps its size-verify and moves only the publish half.

**Do not fold in `init_commit_linux.go`** — its contract is genuinely different
(multi-file, create-only `O_EXCL` with rollback, dirfd-relative) and its
`commitOps` seam exists for testability. Deliberate; keep.

### C4. One egress-policy builder, one set of listener predicates

`cmd/mellomting/serve.go:458` `buildNetworkPolicy` and
`internal/discovery/discovery.go:522` `parseNetwork` are the same 11 lines,
except discovery ends with `backend.ParsePolicy` and serve does not. Not a live
hole — `backend.New` validates at `backend.go:150` — but the asymmetry becomes
one the moment a caller skips `backend.New`. Replace both with
`backend.PolicyFromConfig`, which parses *and* validates.

Also: `serve.go:669` `isNonLoopbackListenAddr` reimplements
`config.isNonLoopbackHost` (`validate.go:767`) — serve's own comment says so —
and `serve.go:682` `staticTLSConfigured` is a verbatim copy of
`config.tlsConfigured` (`validate.go:367`). These two predicates jointly gate
the plaintext-non-loopback refusal (PLAN §8.2); maintaining both halves twice
invites `config check` and `serve` disagreeing. Export the `config` versions
(the `netip` ones, which gate acceptance) and delete serve's copies.

### C5. One `ensureFile`

`internal/systemd` `ensureConfig`/`ensurePepper`/`ensureUsers` (`:190`, `:216`,
`:245`) share an identical 8-line prologue and 6-line epilogue; only content and
mode differ, and drift has started (one takes `mode` as a parameter, two
hard-code `0640`). This is a create-only-if-absent contract in a root
provisioning path, written three times. One
`ensureFile(path, mode, uid, gid, content func() (string, error))`. The content
closure matters: the pepper must not be generated when the file already exists.

### C6. One pepper generator

`cmd/mellomting/init.go:455` and `internal/systemd/systemd.go:227` each define
their own 64-byte constant and base64+newline encoding. The HMAC pepper length
is a security parameter (PLAN §27) defined twice; raising one leaves the other
behind silently. `auth.GeneratePepper(rand io.Reader)` next to the existing
`LoadPepper`/`ValidatePepper`, keeping init's injectable-reader seam.

### C7. Shared `internal/testsupport`

~330 duplicated lines across ~12 test files. All internal tests are whitebox
(`package proxy`, `package backend`, …), so shared helpers must live in a normal
package. Hard constraint: `internal/testsupport` must not import any package
whose whitebox tests use it — it may import `config` and `auth`; it must not
import `proxy`, `backend`, `httpapi`, or `tlsconfig`.

Do the byte-identical ones first: `discardLogger` (3 copies), the unix-socket
HTTP client (2), `fakeModelsServer` (2).

Then, with care, the **drifted** ones — self-signed cert generation has three
copies with *different SAN coverage* (one sets DNS names and IPs, one only DNS
names and mislabels the PEM block `PKCS8 PRIVATE KEY`, one only IPs). Each
supports a different subset of hostname/IP verification. The integration copy
sits behind `//go:build integration`, so `go test ./...` never compiles it and
its drift is invisible to the default gate — an argument for sharing, but
unifying means picking one behaviour and re-verifying each caller.

Explicitly leave alone: `testConfig` and `mustKey` (false positives — different
signatures and layers), `startServe` (the cmd copy `Kill`s, the integration copy
sends SIGTERM with a grace period and is *testing the drain*), and package-local
builders that touch unexported fields.

---

## Phase D — Structure

All of D1–D5 land in `internal/proxy/proxy.go`. Sequence them as written; each
makes the next smaller.

### D1. One backend-error classification

`retryableBackendError` (`:691`), `connectionLevelError` (`:716`), and the
terminal error→response switch (`:626-678`) each switch independently over the
same sentinel set. One taxonomy, three readings, three `default`s. Adding a
sentinel means editing three switches, and forgetting one is silent because
every default is safe-but-wrong.

Replace with a single `classifyBackendError(err) failure` returning
`{retryable, poisonsHealth, status, typ, code, msg, class}` — one row per
sentinel, so each error's whole behaviour reads as one line, and the X6
question ("can a header timeout cascade a cooldown across every key sharing this
backend?") is answerable by reading one table. ~90 lines → ~50.

### D2. One request outcome, settled once

The per-request outcome is split between the `result` struct and a set of
loop-locals (`retried`, `reservation`, `usage`, `usageStatus`, `publicModel`),
and whichever of nine return paths fires must reconcile them and flip
`out.accounted`. B1 is the bug this shape already produced.

Widen `result` to carry what the accounting record needs, delete the `accounted`
boolean and all four `p.account` call sites, and account exactly once in the
existing `defer`. Every path then only assigns fields; none can forget to settle
or settle twice. `p.account`'s 10 parameters collapse to `(q, o, start, out)` —
four of the ten (`model`, `status`, `backendName`, `retries`) are already
duplicated in `result` today.

**This touches the quota-settle path**, so it is the highest-risk task in the
plan. Do B1 first; run the accounting tests hardest here.

### D3. Split `dispatch`

**[verified]** `dispatch` is **525 lines** — 2.7× the next largest function in
the repo (`applyDefaults` at 190). Every local is live across the whole span.
The numbered comments already mark the seams:

- `readBody(q, o) (body, release, apiErr)` — `:208-266`
- `resolveTarget(q, o, body) (target, apiErr)` — `:268-368`
- `prepare(q, target, body) (prepared, reservation, injectedUsage, apiErr)` — `:370-415`
- `forward(q, o, target, prepared, …) (result, apiErr)` — `:417-622`
- `writeBuffered(w, body, idle)` — `:591-615`, the non-stream twin of `pump`

The deferred logging/accounting block and the byte-budget `defer` stay in
`dispatch`; their lifetime is the whole request.

### D4. Immutable `operation`; stop logging the response ID

**[verified]** `o.path` is mutated at `:364` to append the client's response ID,
and the deferred logger at `:170` logs `o.method+" "+o.path`. So every
`/v1/responses/{id}` retrieve and cancel logs
`endpoint: "GET /v1/responses/resp_abc123"` instead of the route template. The
ID is charset- and length-bounded by `isValidResponseID`, so this is not an
injection — but a field meant to be a fixed enum becomes high-cardinality client
input, which breaks aggregation by endpoint. Use a local `outPath`.

Also: `needsModel` and `respID` are exact complements (one bit stored as two
fields), and `generative` is the same predicate as `capField != ""` — with a
third spelling, `o.endpoint == "embeddings"`, at `:316`. Collapse to a
two-valued `route` field and one spelling.

**Explicitly not proposed:** replacing the flag struct with per-operation
methods or an interface. Six operations, one implementation each; the value of
`dispatch` for security review is that admission → parse → ACL → cap → quota →
forward reads top-to-bottom as one linear fail-closed sequence. Scattering it
would make that ordering *harder* to verify.

### D5. Table-driven client errors

33 `fail(status, typ, code, msg, class)` call sites with five unlabelled
positional strings, ~10 tuples repeated verbatim. The `class` argument drives
log and accounting classification and is the easiest to get wrong because it is
last and untyped. A package-level table of named `apiErr` values turns each call
site into one line and makes the entire sanitized client-facing error surface a
single auditable block — which strengthens review of a security surface rather
than weakening it. Pairs naturally with D1, which produces the same type.

### D6. Extract the startup consistency checks

`buildDaemon` (`serve.go`, 144 lines) mixes wiring with two fail-closed
*validations* (`:574-602`): the `ensure_stream_usage`-requires-a-quota rule and
the quota-can-never-advance rule added recently. Both are pure functions of
`(cfg, users)` and are the hardest part to test because they sit behind live
file loads and client construction. Extract `checkQuotaEnforceable(cfg, users)`
and call it before any client is built — which also removes the
`_ = writer.Close()` cleanup asymmetry. Note this changes *which* error an
operator sees first when two things are wrong at once.

Also **[verified]** `quotaKeysConfigured(users.Keys)` is recomputed four times
in `buildDaemon` (`:570`, `:580`, `:591`, `:604`); hoist it.

---

## Phase E — Performance

Framing: proxy latency is dominated by the backend (hundreds of ms to seconds),
so none of this moves p50. What it moves is **CPU and allocation per byte
proxied** — how many concurrent streams one box carries. E1 is also a genuine
simplification; E2 and E3 are small; E4 is the largest per-event win but the
trickiest.

### E1. Decode and encode the body once

**[verified]** `shallowParse` (`:272`), `prepareOutbound` (`:385`), and
`rewriteModel` (`:481`) each independently `json.Unmarshal` the whole body into
`map[string]json.RawMessage`; `prepareOutbound` and `rewriteModel` each marshal
it back. `stringField` (`:323`) adds a fourth decode on the Responses path. And
`rewriteModel` runs **inside the retry loop**, so it repeats per attempt.

`json.RawMessage`'s unmarshaler copies every field's bytes, so each decode
allocates roughly the whole body again. Measured on a 20 KB chat body:

| | ns/op | B/op | allocs |
|---|---|---|---|
| current (3 decode + 2 encode) | 124,936 | 105,972 | 43 |
| single decode + single encode | 39,806 | 42,818 | 19 |

3.1× faster, 2.5× less garbage, scaling with body size (a 200 KB coding-agent
body: ~1.25 ms → ~0.4 ms).

Decode once in `shallowParse`, return the map, thread it through;
`stringField` becomes a map lookup; `prepareOutbound` mutates the map; the retry
loop sets `fields["model"]` and marshals **once per attempt**. This is strictly
simpler — one parse point instead of four, and the `body`/`prepared`/`bd`
confusion disappears.

**Constraint §2 applies**: keep exactly one canonicalizing marshal.

Aside for §8 decision 3: that marshal **[verified]** also HTML-escapes
`<`, `>`, and `&` into their JSON unicode-escape forms, and reorders top-level
keys alphabetically. So the README's "passed through byte-for-byte" claim is
already not literally true for request bodies.

### E2. Parse each SSE event once

`ParseStreamChunk(data)` parses the chunk, then `isUsageOnlyChunk(data)` parses
**the same chunk again**, for every delta event — and `dataField(ev)` is called
twice per event on Responses streams. Gate the second parse on the first's
result (a chunk with no `usage` field can never be usage-only) and hoist
`dataField`. 1107 ns / 640 B / 9 allocs → 582 ns / 408 B / 5 allocs per event.
Behaviour-preserving; the `usage: null` / FIX-08 case still works because
`u.Present` is false for a null usage.

### E3. Reuse the SSE parser buffers

`internal/proxy/sse.go:105` `reset()` sets `p.line = nil; p.event = nil`, so
every event regrows from zero capacity, and `readLine` appends one byte at a
time. The repo's own `BenchmarkSSEParse` measures 7 allocs / 1160 B per event
purely for framing. Truncate instead (`p.line[:0]`) — `nextEvent` already
returns an independent copy, and the parser is per-stream, so there is no
cross-request reuse. Update the comment on `reset` to say the retention is
deliberate.

Not proposed: switching `readLine` to `ReadSlice('\n')`. ~10× on framing, but
the lone-`\r` terminator and `maxSSELine` bound make it materially more code —
that one trades simplicity for speed. Skip unless a profile demands it.

### E4. One reader goroutine per stream

`pump` (`:805-836`) allocates a channel, spawns a goroutine with a closure and
deferred recover, and creates a `time.NewTimer` **per event**: 746 ns / 456 B /
6 allocs versus 347 ns / **0 allocs** for one long-lived producer with a reused
timer.

Spawn one reader for the whole stream feeding an **unbuffered** channel (keeping
back-pressure and the memory bound identical), with a `done` channel closed by
the pump on every exit path so the producer cannot leak when the idle timeout
abandons a read. Hoist and `Reset` the timer.

Risk is concentrated in the timer stop/drain dance and in `done` — get `done`
wrong and you leak a goroutine per aborted stream. Needs an explicit
idle-timeout test.

### Considered and rejected

- **`sync.Pool`** for bodies or event buffers — E1 removes most of the
  allocation and E3 gets per-stream reuse without cross-request pooling. In a
  multi-tenant proxy a pooled buffer that is not fully overwritten leaks one
  key's prompt bytes to another.
- **Buffered request logging** — would risk losing the last lines on crash and
  reorder operational logs against stderr, to save ~1-10 µs.
- **Backward-seeking quota replay** — `Replay` can scan up to `1 GiB` of JSONL
  (~5M records, 10-30 s) before the listener accepts. But the backward-seek fix
  assumes append-time ordering that a `copytruncate` rotation could momentarily
  violate, and under-counting quota is not fail-closed. **Prefer lowering the
  default `replay_max_bytes`** (e.g. 64 MiB) and leaving the algorithm alone.
- **`Router.UpstreamFor`'s nested scan** — real redundancy (`Select` already
  returned the upstream in `Target` and `dispatch` throws it away), but the cost
  is nanoseconds. Fix it as tidying under D3, not for speed.

---

## 6. Verification protocol

Every task: `gofmt -l`, `go build ./...`, `go vet ./...`, `go test ./...`, and
`go test -race` on the touched packages. Additionally:

- Phase A: the diff must be reviewable as mechanical. `go test ./...` green is
  the gate, already **[verified]** for A1-A3 as a whole.
- Phase B: B1 and B4 require **new tests written first** — they change
  observable behaviour.
- Phase C: C3 requires new `securefile` tests covering each guarantee in the
  table, since two callers gain guarantees they lack today.
- Phase D: D2 requires the accounting and quota tests to be run hardest, plus
  the B1 assertions. D5 changes no strings; the existing tests assert emitted
  JSON.
- Phase E: E1 needs a duplicate-key test (constraint §2) and a body-fidelity
  test. E4 needs an idle-timeout and a client-disconnect test to prove no
  goroutine leak — run with `-race` and consider `goleak`.
- `go test -tags integration ./integration/` after each phase.

## 7. Suggested order

A (all) → B1 → B2, B3, B4 → C1, C5, C6, C4 → C2, C3 → D1, D5 → B1-verify → D2 →
D3, D4 → D6 → C7 → E1 → E2, E3 → E4.

Rationale: mechanical first so later diffs are clean; the defect and dead code
before restructuring, so there is less to restructure; cheap dedup before
expensive dedup; the error taxonomy (D1/D5) before the outcome consolidation
(D2) because D2 consumes it; `dispatch` split after both; test-support
consolidation once the code it supports has settled; performance last, when the
body path has only one owner.

## 8. Decisions needed before starting

1. **`encoding/json/v2`** — recommendation: **do not migrate.** The guideline
   itself advises against migrating existing code; v2 changes wire behaviour
   (nil slices as `[]`, duplicate-name rejection, invalid-UTF-8 rejection), and
   this codebase both emits a documented JSONL format and relays third-party
   JSON byte-ranges. A *targeted* future use is defensible — v2's duplicate-name
   rejection would make constraint §2 structural rather than incidental — but
   that is its own project with its own security review, not part of this plan.
2. **Where `redactURL` lives** — `config` already imports `landlock`, so
   `landlock.RedactURL` costs zero new packages; but redaction is not
   conceptually Landlock's job. The alternative is a ~25-line leaf
   `internal/redact`. **Recommendation: the leaf package** — it is honest about
   the concern and the codebase already has tiny single-purpose leaves. Either
   way, standardise on `<redacted>@`.
3. **README "byte-for-byte"** — **[verified]** request bodies are re-encoded
   with HTML escaping and alphabetical key order. Either soften the claim to
   cover response bodies and unknown *fields* (accurate today), or add
   `SetEscapeHTML(false)` under E1 and narrow the gap. The claim as written is
   inaccurate for request bodies regardless of what we choose.
4. **How far to take C7** — the byte-identical helpers are free; the
   drifted TLS-certificate helpers require picking one SAN behaviour and
   re-verifying three callers. Worth it, but it is the one test-side task that
   can change what a test actually covers.
5. **Scope of Phase E** — E1 is a clear win on both axes. E4 is the largest
   per-event win but the riskiest task in the plan. It is reasonable to stop
   after E3.

## 9. Expected outcome

Roughly **900-1000 lines removed** net (~330 test scaffolding, ~135 dead
qualifier machinery, ~115 `applyDefaults`, ~250 deduplication, the rest
mechanical), one live defect fixed, two behavioural gaps closed, and the
codebase on Go 1.27 with a recorded rationale for every modern idiom
deliberately not adopted.

No security control is weakened by any task in this plan. Three tasks
(C3, D2, E1) touch security-relevant machinery and are marked accordingly.
