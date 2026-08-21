# Mellomting — Bug-fix remediation plan

Derived from the security/stability review in
`docs/CODE_REVIEW_CLAUDE_2026-08-21.md` (reviewed commit `62aa81d`, Phase 6).
This document is the working checklist for fixing every confirmed finding.

## How to use this document

Each section is a **task** that an agent (or human) can execute independently.
Every task has the same shape:

1. **Fix** — the concrete code change, with file anchors.
2. **Verify** — the automated and/or manual step(s) that prove the problem is
   actually fixed. Do not mark a task done until its verification passes.
3. **Done when** — the acceptance criterion.

Tasks are ordered **by severity, then by dependency** (fix the root cause before
the symptom). Later tasks may depend on earlier ones; where they do, that is
noted. Run the full quality gate (`gofmt -l .`, `go build ./...`,
`go test ./...`, `go test -race ./...`, `go vet ./...`) after every task.

> **Baseline before starting:** confirm the failing state first so the fix can
> be shown to change behaviour. Many findings are reproduced by a specific
> test; write the failing test *first* where noted.

---

## Critical

### T-X1 — CRITICAL — Cross-key read/cancel of Responses via `singlePossibleBackend` fallback

**Finding:** `GET /v1/responses/{id}` and `POST /v1/responses/{id}/cancel`
forward on backend reachability alone (`singlePossibleBackend`) with **no
ownership check** when the affinity map misses (`internal/proxy/proxy.go:255-266`).
PLAN §21.1: a different key must not retrieve/cancel another key's response
even if it knows the ID. The `POST /v1/responses` + `previous_response_id` path
is already correct (uses `affinity.Get(q.Key.ID, prev)`); only retrieve and
cancel are broken.

**Fix:**
- In `proxy.go`, for both `ResponsesRetrieve` and `ResponsesCancel`, require a
  **successful `affinity.Get(q.Key.ID, id)`** before forwarding. Remove the
  `singlePossibleBackend` fallback for these two routes (or gate it behind an
  ownership check that cannot be satisfied by a foreign key).
- On an affinity miss, fail closed with 404 (the existing
  `fail(404, …)` path) — never forward on backend reachability.
- Update the misleading comment at `affinity.go:12` to describe the actual
  guarantee once restored.

**Verify:**
- Add an integration test through the real `httpapi` stack with two keys, one
  backend, one model: key B creates a Response; key A (scoped to the same
  single model) must get **404** (not 200) on `GET /v1/responses/{id}` and on
  `POST /v1/responses/{id}/cancel`; key B must still succeed on both.
- Assert that key A cannot read the response content nor terminate key B's
  generation.
- Confirm the existing correct path (creator retrieves its own, and
  `previous_response_id` with the same key) still passes.

**Done when:** no code path forwards a Responses retrieve/cancel without an
`affinity.Get(q.Key.ID, …)` ownership hit, and the two-key integration test
passes. Replace the unsound `proxy_test.go:384` cross-key test (which used an
unknown ID) with this real two-key assertion.

---

## High

### T-X1b — HIGH — `weighted-round-robin` double-index panic

**Finding:** `router.go:179` `nextWRR` returns a **replica index**, but
`Select` (`router.go:215-241`) does `cands[r.nextWRR(...)]`, indexing a second
time. Panics once `cands` is sparse (any replica in cooldown / excluded).

**Fix:**
- Change `Select`'s `weighted-round-robin` case to
  `pick = r.nextWRR(publicModel, entry, cands)` (drop the outer `cands[...]`).
- Add a comment clarifying the return contract of `nextWRR`.

**Verify:**
- Unit test: a 3-replica WRR model with `exclude` non-empty (e.g. `["b"]`)
  over many `Select` calls must never panic, must never return an excluded
  replica, and must distribute weight approximately as configured.
- Re-run the existing `router_test.go` WRR test (healthy, full `cands`) — it
  must still pass.
- Confirm the returned backend was not excluded by the retry loop (PLAN §23).

**Done when:** no panic with a sparse candidate set, no excluded replica is
returned, and the weight distribution holds approximately.

---

### T-X2 — HIGH — Malformed Landlock rule; daemon cannot start sandboxed

**Finding:** `internal/landlock/apply_linux.go:51` includes
`llsys.AccessFSMakeReg` (a **directory-only** right) in the rule for the
accounting JSONL **regular file**. The kernel returns `EINVAL`, so with
`landlock.mode: required` + accounting enabled the daemon fails to start.
`apply_linux_test.go` skips below ABI 8 and CI runs a sub-ABI-8 kernel, so the
test never executes.

**Fix:**
- Drop `AccessFSMakeReg` from the `writeFile` mask (the file already exists and
  is opened before enforcement, so creation rights are unnecessary) — or use
  the library's `RWFiles` helper so the mask is intersected correctly.
- Keep `AccessFSWriteFile | AccessFSMakeReg`-equivalent semantics only where a
  directory rule genuinely needs it.

**Verify:**
- **Must be run on a Linux kernel with Landlock ABI ≥ 8.** `go test
  ./internal/landlock/` must execute `TestAllThreadsEnforced` (do not let it
  skip) and pass. The test at `apply_linux_test.go:107` already `t.Fatalf`s on
  an `Apply` error with `WriteFiles` from a regular file.
- If no ABI ≥ 8 host is available locally, run it in CI on a kernel with
  Landlock ABI ≥ 8 and confirm green.
- Also run the full `serve`-level flow: start the daemon with
  `landlock.mode: required` + `accounting.enabled: true` and confirm it reaches
  `mellomting ready` rather than exit 1.

**Done when:** `TestAllThreadsEnforced` executes and passes on ABI ≥ 8, and a
required-mode daemon starts with accounting enabled. Document in AGENTS.md/CI
that this test must run on an ABI ≥ 8 kernel.

---

### T-X3 — HIGH — Backend redirects followed (authenticated SSRF + credential disclosure)

**Finding:** `internal/backend/backend.go:189` sets no `CheckRedirect`, so the
backend (not the operator) chooses the effective URL. A `302` re-sends the
backend `Authorization` and replays the body to another host/path, and the
redirected response is relayed to the client.

**Fix (do together with T-X4):**
- Set `CheckRedirect` to a function that returns `http.ErrUseLastResponse` so
  redirects are **not** followed; OR classify 3xx as an upstream error.
- Do **not** leave a 3xx to surface as a "200-ish" body: a 3xx must be treated
  as an upstream error (`Upstream` / `ErrUpstream*`), not relayed with its body.
- Ensure a 3xx never reaches the nil-`Body` path (see T-X4).

**Verify:**
- Integration test through the real proxy stack: a fake backend answering
  `POST /v1/chat/completions` with `302 → /metrics` must NOT return `/metrics`
  content to the client, and must NOT send the backend `Authorization` header
  to the redirect target.
- Test a `302` to a different loopback port: that second service must receive
  **no** backend credential.
- The client must receive a sanitized OpenAI-shaped error (PLAN §72), not a raw
  redirect body, and §11.3's `/metrics` must remain unreachable.

**Done when:** no redirect is followed, no credential is re-sent to a redirect
target, and 3xx maps to a sanitized upstream error with no nil-`Body` crash.

---

### T-X4 — HIGH — Nil-body panic in spawned goroutine kills the process

**Finding:** `internal/backend/backend.go:410` returns `&Result{Body: nil}` with
nil error for a 2xx non-200 stream response. `proxy.go:436` branches on the
client's `stream` flag and calls `pump`, which reads a nil body in a spawned
goroutine (`proxy.go:629`). The panic is not reachable by `net/http`'s
per-connection recover and **kills the whole process**. There is no
`recover()` anywhere in the repository.

**Fix:**
- In `proxy.dispatch`, branch on whether a **live body exists**
  (`res.Body != nil`), not on the client `stream` flag, before entering `pump`.
- In `backend.Forward`, for a `stream:true` request answered by any 2xx that is
  **not** 200, either treat it as an upstream error or buffer it as a
  non-stream — never return a nil-`Body` with a nil error.
- Add panic containment: a deferred `recover()` around the spawned pump
  goroutine (and, for defence-in-depth per the review's structural note, around
  the handler path) that converts a panic into an OpenAI-shaped 500 and logs a
  sanitized structured error, without killing the process or leaking slots.

**Verify:**
- Integration test: a `stream:true` request answered with `201` (or `202`,
  `226`) must not panic; the process must survive and the client gets a
  sanitized error.
- A goroutine-leak/panic test: force a nil body / panicking read in the pump
  goroutine and assert the process stays alive and the handler returns a 500
  (no cross-tenant crash).
- Grep confirms `recover()` now exists on the pump path and the containment is
  exercised by a test.

**Done when:** no nil-body panic reaches an uncontained goroutine; a panicking
pump yields a 500 without killing the process; `recover()` is present and
tested.

---

### T-X5 — HIGH — Per-backend concurrency does not bound streams; `Inflight()` wrong

**Finding:** `internal/backend/backend.go:372-377` releases the admission slot
via `defer` while a stream is still generating (slot covers only header
exchange). `Inflight()` (`:484-486`) is `len(c.queue) + len(c.conc)` but
`acquire` never removes the queue token on admission, so active counts 2 and
waiting counts 1. PLAN §22 / invariant 13 broken for streaming.

**Fix:**
- Hold the per-backend admission slot for the **lifetime of the stream** (until
  the `Result.Body` is fully drained/closed), not just until headers arrive.
- Fix `Inflight()` to count exactly one per admitted request and one per
  queued request (remove the double count).
- Confirm `max_concurrency`/`queue_size` now bound concurrent streams and that
  the §22 `503 server_overloaded` is emitted when the queue is full.

**Verify:**
- Integration test: `max_concurrency: 1, queue_size: 8` with a backend that
  flushes SSE headers then blocks; assert that at most **1** stream is live at
  once and `Inflight() == 1` for the active request (not 0, not 2).
- A second scenario at `max_concurrency: 2, queue_size: 2`: assert no more than
  2 live streams.
- Unit test `TestInflightSnapshot` (`backend_test.go:188`) must be extended to
  the streaming case; remove/repair the misleading non-streaming comment at
  `:246`.

**Done when:** per-backend `max_concurrency`/`queue_size` bound streaming work,
`Inflight()` reports 1 per active and 1 per queued request, and the invariant-13
integration test passes.

---

### T-X6 — HIGH — Non-streaming generations over 30s can never succeed; cooldowns poison health

**Finding:** `header_timeout` (default 30s) is wired to
`Transport.ResponseHeaderTimeout`, so for non-streaming work (headers arrive
only at completion) time-to-headers ≈ full generation time. The 20-minute
`request_timeout` is unreachable. `prepareOutbound` injects the policy cap when
the client sends no limit, making the default request the longest. Each timeout
calls `RecordFailure`, cooling the backend out (amplifier).

**Fix:**
- Decouple the **non-streaming** time-to-first-byte from `header_timeout`.
  Apply `ResponseHeaderTimeout` only where headers can legitimately be expected
  promptly (streaming / dial), and give non-streaming work a
  completion-appropriate bound (the existing `request_timeout`) for its headers.
- Ensure `header_timeout` no longer truncates legitimate long non-streaming
  generations at stock defaults.
- Reconsider whether a `ErrHeaderTimeout` should immediately `RecordFailure`
  and poison health (it is a capacity/latency signal, not necessarily a backend
  fault). At minimum, ensure one slow request does not cascade a cooldown over
  unrelated models/keys (see the review's keyed-by-backend-name-only note).

**Verify:**
- Integration test: a non-streaming completion that takes longer than
  `header_timeout` but within `request_timeout` must **succeed** on stock
  defaults.
- Confirm the poisoning amplifier is bounded: a single 30s+ failure must not
  deterministically 503 an innocent fast client on the same or a different
  model sharing that backend.
- Retest with `retry.max_attempts: 2`: confirm the retry list does not blindly
  re-issue full generations that cannot succeed within the header bound.

**Done when:** non-streaming generations > 30s succeed on stock defaults, and a
header-timeout no longer cascades across models/keys via cooldowns.

---

### T-X7 — HIGH — Every terminal upstream non-2xx returns two concatenated JSON errors

**Finding:** `proxy.go:474-511` runs two switches with no `return`/`else`
between them. `*backend.Upstream` has no `Unwrap`, so the second switch always
falls to `default`, writing a second error body and overwriting
`out.status`/`out.class`. Body is invalid JSON; SDKs fail to parse; logs store
the wrong class; stderr gets "superfluous response.WriteHeader" noise.

**Fix:**
- Add a `return` (or `else`) so the `*backend.Upstream` handling is terminal and
  the second switch only runs when the first did not match.
- Make `backend.Upstream` wrap/classify correctly (or handle it explicitly) so
  the intended mapping (429→429, ≥500→502, else→400) is the single write.
- Ensure exactly **one** `fail(...)` runs per terminal error.

**Verify:**
- Add a test that JSON-decodes the proxy error body (none exists today) for
  upstream 400, 429, and 503; assert the body parses as **one** valid JSON
  error object with the correct `code`/`type` and correct status.
- Assert the structured request log and accounting record store the correct
  class (400/429), not a generic 502.
- Assert no "superfluous response.WriteHeader" warning in stderr for these
  cases.

**Done when:** every terminal upstream error produces exactly one valid,
correctly-classed JSON error body; log/accounting records match; no
double-write warnings.

---

### T-X8 — MED-HIGH — Per-key limits fail open; one key can starve every other key

**Finding:** `mellomting key create` emits a users file with no `limits:` block
and exposes no flags to set one. Zero means unlimited throughout
(`limiter.go:80-86`, `:151-152`). One no-limits key can occupy all inflight
slots, 503-ing other keys. `validateUsers` never checks `limits`. Negative
values also produce unbounded limiters (hardening note).

**Fix:**
- Add CLI flags to `mellomting key create` to set `limits.concurrent_requests`
  and `limits.requests_per_second` (and record them in the users file).
- In `auth/store.go` `validateUsers`, validate the `limits:` block: reject
  negative values (fail closed) and range-check.
- Decide and enforce a sane default for a key with no `limits:` block — either
  document it as intentionally unlimited **and** add a startup warning, or
  apply a conservative default (recommended: per PLAN §35, do not let a single
  key occupy the whole process). Update the misleading comment at
  `limiter.go:163` ("validated at the key-store boundary; fail closed") to
  match reality.
- Ensure negative `concurrent_requests` / `requests_per_second` fail closed.

**Verify:**
- Integration test: create a key with no limits and one with
  `concurrent_requests: 2`; a single no-limits key must **not** be able to fill
  all global inflight slots and 503 a different key under stock defaults (or,
  if a default limit is applied, assert the default is applied and documented).
- Unit tests for `validateUsers` reject negative limits and invalid values.
- CLI test: `key create --concurrent-requests 2` emits a users file with a
  `limits` block that the daemon enforces.

**Done when:** no key can starve every other key by default; negative/limit
values fail closed; the users-file `limits` block is validated at the key-store
boundary.

---

### T-X9 — HIGH — Non-streaming write has no deadline; slow reader denies service to all

**Finding:** the only `SetWriteDeadline` is in the streaming pump
(`proxy.go:691`). Non-streaming writes have no deadline, so a client that stops
reading holds inflight slots indefinitely (`proxy.go:464-465`).

**Fix:**
- Mirror `proxy.go:691`'s `ResponseController.SetWriteDeadline` on the
  non-streaming write path, using an appropriate timeout (e.g. the existing
  `stream_write_timeout` value or a new non-stream write timeout).
- Ensure the deadline is set per-write and does not leak into the next
  keep-alive request (the review disproved a leak for streams; keep that
  property for non-streams).

**Verify:**
- Integration test at stock defaults: a client that reads headers then stops
  reading a non-streaming response must have its write bounded — the proxy
  must reclaim the inflight slot and return an error/timeout rather than hold
  forever, and must not deny service to other keys indefinitely.
- Confirm recovery after the attacker stops holding is prompt, and the
  load-balancer health endpoints are not falsely affected.

**Done when:** non-streaming writes are bounded by a deadline; a stalled reader
cannot hold slots indefinitely; service recovers once the client releases.

---

## Medium

### T-X10 — MEDIUM — Unknown-usage quota charge is client-controlled; prefill escapes accounting

**Finding:** `proxy.go:918-924` uses `reservation = clientLimit` where §39 calls
for a **configured** reservation. The reservation is output-only, so the prefill
escapes accounting entirely. `account` settles the reservation in memory but
writes `usage.Total` (= 0) to JSONL, and `replayLine` skips `TotalTokens <= 0`,
so conservatively-charged records replay to no state (quota reset on restart).

**Fix:**
- Use a **configured** reservation for unknown usage (per PLAN §39), not the
  client's own output limit.
- Add a `charged_tokens` (or similar) field to the JSONL record and write the
  **actual amount charged** (`total`), not `usage.Total`, so replay restores
  the settled state. Make `replayLine` account for the charged amount.
- Account for input tokens (prefill) in the unknown-usage reservation, or
  document the deliberate limitation explicitly.

**Verify:**
- Integration test: a backend returning no `usage` block against a real quota —
  the charge must reflect the configured reservation, not the client's
  `max_completion_tokens`.
- Replay test: a conservatively-charged record must **replay to the same
  settled state** (not reset to zero) after restart.
- Assert the JSONL record carries the charged amount and `usage_status`.

**Done when:** unknown-usage charge uses a configured reservation, prefill is
accounted or explicitly scoped, and replayed records restore the charged state
so §40's "SHOULD NOT trivially reset" holds.

---

### T-X11 — MEDIUM — Single backend `total_tokens` voids the key's quota window

**Finding:** `ParseUsage` accepts arbitrary int64 with no bounds check; `Settle`
adds to `st.hour`/`st.day` with overflow; `Admit`'s `st.hour + reservation >
limit` comparison overflows. One `MaxInt64` response wraps the counter negative,
admitting the key without limit. `ReportFile` sums unguarded.

**Fix:**
- Clamp usage values on parse in `ParseUsage` (`internal/accounting/quota.go:50`,
  `record.go`).
- Use saturating / pre-checked addition in both `Settle` and `Admit` so
  overflow never wraps the counter negative or makes the `> limit` comparison
  false.
- Guard `ReportFile` summation against the same overflow.

**Verify:**
- Reproduce the review's case: `usage = {prompt_tokens:1, completion_tokens:1,
  total_tokens: MaxInt64}` against a `tokens_per_hour: 1000` key. After
  settling, `Admit(1)` and a second settle must **not** admit unlimited —
  the key must remain quota-bound.
- Fuzz `ParseUsage`/`UsageToQuota` with extreme int64 values; assert no overflow
  and no negative counter.
- Unit test that the stored/reported total never wraps negative.

**Done when:** backend-reported token values are clamped on parse and all
additions saturate, so a single bogus `total_tokens` cannot void a quota window.

---

### T-X12 — MEDIUM — A body of many short keys costs 13× its size in peak heap, with no aggregate bound

**Finding:** `budget.Acquire` (`internal/proxy/budget.go:9-12`) reserves `2 *
bodyBytes` but the body is read/decoded **before** reserving, so the budget
gates nothing and the actual peak is ~13× body (measured 211.9 MiB for a 16 MiB
body of 800k short keys). PLAN §12.1 requires bounded aggregate memory.

**Fix:**
- Reserve the byte budget **before** reading the body (move the
  `budget.Acquire` ahead of the body read), so a huge/awkward body is rejected
  before it is allocated.
- Account for the decode+re-encode peak in the reservation so the 503 at
  `proxy.go:208` is actually reachable and meaningful at defaults.
- Ensure the aggregate bound holds with `max_inflight_requests` × per-request
  peak.

**Verify:**
- Reproduce the review's measurement: a 16 MiB body of ~800k short keys must
  not reach ~13× body peak heap; the budget must reject it before the costly
  decode.
- A body exceeding the effective aggregate budget gets a clean 503 (reachable
  at defaults), not a late one.
- Test that `max_buffered_request_bytes` and the pre-read reservation actually
  bound memory.

**Done when:** the byte budget is reserved before reading and the decode+re-encode
peak is accounted for, so pathological bodies are rejected early and aggregate
memory is bounded (PLAN §12.1).

---

### T-X13 — MEDIUM — A stream whose final event lacks a terminating blank line is silently truncated

**Finding:** `internal/proxy/sse.go:97-101` discards the buffered event on a
clean EOF with no trailing blank line; `pump` maps the EOF to `"ok"`. Generated
content and its usage chunk are lost. A lone `\r` is treated as data rather
than a line terminator.

**Fix:**
- On a clean EOF, flush the buffered partial event (emit it) rather than
  discarding it, so the final `data:` line is relayed (PLAN §24 "re-emitted
  verbatim").
- Treat a lone `\r` as a line terminator in the SSE parser (spec-legal CR
  termination).
- Ensure the final event's usage chunk reaches accounting.

**Verify:**
- Reproduce the review's trailers test: `trailer=""`, `trailer="\n"`, and
  `trailer="\r"` must all deliver the final content event to the client with
  the correct status and correct settled usage (not the fallback reservation).
- Fuzz the SSE parser; assert CR/CRLF/LF terminations are all handled and no
  event is dropped on a clean EOF.

**Done when:** a cleanly-closed stream with a non-blank-line-terminated final
event relays that event verbatim, and CR terminations parse correctly.

---

## Medium findings M1–M19

### T-M1 — MEDIUM — Output cap bypassable three ways

**Finding:** `proxy.go:901-921`: (a) a **negative** limit suppresses the cap
entirely; (b) on `/v1/chat/completions` `altCapField` sits in an `else if`, so
an over-cap `max_tokens` is never validated when `max_completion_tokens` is
present, and `intField` rejects `1e9`/`1000000.0`/`>int64` reading them as
absent; (c) `max_completion_tokens: 0` is silently raised to the cap.

**Fix:**
- Reject/clamp negative limits so a negative value cannot suppress the cap.
- Make `altCapField` validation independent of the `else if` so an over-cap
  value in **either** field is caught and overwritten; handle
  non-integer/overflowing lexemes as present-and-invalid rather than absent.
- Treat `0` (and negatives) in the cap logic correctly so the cap is never
  bypassed and never silently raised without intent.

**Verify:**
- Table test over `/v1/chat/completions` and `/v1/completions` with:
  `max_tokens:-5`, `max_tokens:1e9`, `max_tokens:1000000.0`, over-cap
  `max_tokens` with `max_completion_tokens` present, and
  `max_completion_tokens:0`. Assert the forwarded body always carries a limit
  at or below the policy cap (no field above the cap) and identical inputs
  behave identically across both endpoints.

**Done when:** no input can forward a generation with a limit above the policy
cap; both endpoints agree; negatives and malformed lexemes fail closed.

---

### T-M2 — MEDIUM — Dot-segment response IDs forwarded unnormalized

**Finding:** both ID validators permit `.` (`proxy.go:559-575`, `server.go:288-302`),
and the backend URL is built with `&url.URL{Path: …}`, which does not remove dot
segments; ingress is a bare `HandlerFunc`, so nothing cleans the path inbound.
`POST /v1/responses/../cancel` reaches the backend verbatim.

**Fix:**
- Reject `.` and `..` (and any path-normalizing segment) in `responsesID` /
  `isSafeSegment`, so the response ID is a single safe path segment.
- Optionally normalize the outbound path (clean dot segments) as defence in
  depth.

**Verify:**
- Integration test through the real HTTP stack: `POST /v1/responses/../cancel`
  (and `/%2e%2e`, `.`, `..` forms) must be rejected/404'd and must **not** reach
  the backend verbatim.
- Confirm the allowed single-segment IDs still route correctly.

**Done when:** no request with a dot/`..` segment in a response-ID path is
forwarded to a backend outside the §11.1 allow-list.

---

### T-M3 — MEDIUM — `backends[].stream_idle_timeout` is dead config

**Finding:** `internal/backend/backend.go:187`, `:495` — `Client.StreamIdleTimeout()`
has no callers; `pump` uses the server-level value. It is the only wall-clock
bound on a stream (streams get `fctx = ctx` with no `request_timeout`).

**Fix:**
- Wire `backends[].stream_idle_timeout` into the streaming `pump` (or, if the
  server-level value is intended to govern, document and remove the dead
  per-backend knob).
- Ensure a stream is bounded by *some* wall-clock idle timeout and that the
  configured value is what governs.

**Verify:**
- Integration test: a per-backend `stream_idle_timeout` is honoured by the pump
  (a stalled stream is terminated at that bound, not the server default).
- Grep confirms the `StreamIdleTimeout()` method now has a caller.

**Done when:** `stream_idle_timeout` is either enforced where configured or
explicitly removed, and every stream has a wall-clock idle bound.

---

### T-M4 — MEDIUM — No pre-auth per-source limiting; auth failures logged unbounded

**Finding:** the global bucket is consumed before `authorize`
(`server.go:104-118`, `:267`); nothing in `limiter` has a per-source dimension.
One host flooding with bogus bearer tokens drains the shared bucket (429-ing
legit keys before auth) and produces ~100 warn-lines/s (§33 forbids logging
every invalid token during a flood).

**Fix:**
- Add a per-source (per-IP, or per-IP+listener) pre-auth limiter that binds the
  connection/request rate *before* authentication, so a bogus-token flood does
  not drain the shared authenticated bucket.
- Rate-limit/drop invalid-auth logging during a flood (do not log every invalid
  token); log at most a bounded summary.

**Verify:**
- Integration test: a host sending high-rate bogus bearer tokens must be
  pre-auth throttled without 429-ing a legitimate key on the shared bucket.
- Assert log volume during a bogus flood stays bounded (no unbounded
  warn-line/s).

**Done when:** a bogus-token flood cannot drain the shared authenticated bucket
and does not produce unbounded log output.

---

### T-M5 — MEDIUM — `accounting.enabled: false` silently voids all token quotas

**Finding:** `quota` is constructed only inside `if cfg.Accounting.Enabled`
(`serve.go:435-460`), and `Admit` is skipped when nil (`proxy.go:316`). Nothing
cross-checks the users file, so disabling accounting silently disables quota
enforcement.

**Fix:**
- Ensure token quotas are enforced even when JSONL accounting is disabled (the
  quota state is in-memory; only the persistence is off), OR fail closed at
  startup when accounting is disabled but quota-bearing keys are configured.
- At minimum, emit a clear startup warning when quotas will not be enforced.

**Verify:**
- Integration test: with `accounting.enabled: false`, a quota-bearing key must
  still be enforced (or the daemon refuses to start with a clear message).
- Confirm the chosen behaviour is documented in COMPATIBILITY.md.

**Done when:** disabling accounting can no longer silently disable quota
enforcement.

---

### T-M6 — MEDIUM — The accounting overflow alert can never fire

**Finding:** `writer.go:78-79` — `w.lastAlert.CompareAndSwap(now, now+30)` passes
`now` as the *expected old* value, but `lastAlert` starts at 0, so the CAS
always fails and the alert never fires.

**Fix:**
- Fix the `CompareAndSwap` to use the correct expected value (e.g. swap from 0
  to `now+30`, or use `Load`/`Store` with a monotonic check).

**Verify:**
- Reproduce the review's sim: 200 drops over 200 simulated seconds must produce
  alerts at the configured cadence (not 0).
- Unit test asserting `lastAlert` advances and the alert path fires after the
  interval.

**Done when:** the drop-and-alert path actually alerts at the configured
cadence.

---

### T-M7 — MEDIUM — YAML content after `---` bypasses every config strictness layer

**Finding:** `load.go:50-97` — `checkShape`, `checkNodes`, and the
`KnownFields(true)` decode each call `Decode` exactly once, so only the first
document of a multi-document YAML is examined; the rest is silently discarded.
`cat base.yaml overrides.yaml > config.yaml` silently drops the second half
(including an appended `security:` block) while `config check` reports valid.

**Fix:**
- Reject multi-document YAML configs outright (fail closed) — a `---` marker
  must be an error, not silently swallowed.
- Or decode/validate **all** documents and refuse ambiguity.

**Verify:**
- `config check` on a two-document config must fail (exit non-zero) with a clear
  message, not report "is valid".
- Confirm `show-effective` and the `serve` path also refuse such files.
- Fuzz the loader with `---` markers.

**Done when:** any multi-document YAML config is rejected at every layer, so no
configuration is ever silently discarded.

---

### T-M8 — MEDIUM — No file-permission checks; `securefile` helpers are dead code

**Finding:** `WorldWritable`/`LStat` have zero non-test callers; no mode check
exists in product code. A 0666 pepper/users/TLS-key/backend-`api_key_file` is
accepted. Note the correction: `WorldWritable` tests *write* bits (`0o020|0o002`),
but the exposure is *readable* bits (`&0o007` or `&0o077`).

**Fix:**
- Add strict file-permission checks (per PLAN §100, not §28) for the pepper,
  users file, TLS key/cert, and backend `api_key_file`.
- The check must reject files readable by group/other (`&0o077` or `&0o007`),
  not just writable ones. Wire up or replace the dead `securefile` helpers so
  the correct mode test is actually enforced.
- Fail closed on a too-permissive file (refuse to start / refuse to load).

**Verify:**
- Integration test: a `0644`/`0666` pepper, users file, TLS key, and backend
  `api_key_file` must each be rejected with a clear error; `0640`/`0600` must
  be accepted.
- Unit tests for the mode-check helper assert the read-bit (not write-bit)
  semantics.

**Done when:** secret-bearing files with group/other-readable modes are rejected
at startup/load, and the check is the correct read-bit test with tests.

---

### T-M9 — MEDIUM — YAML merge keys in the users file can hide a wildcard ACL

**Finding:** `store.go:73-78` — `LoadUsers` applies `KnownFields(true)` but not
the `checkNodes` walk, so anchors and `<<:` merge keys are accepted in the users
file (though rejected in the config file), silently granting `models: ["*"]`
and `enabled: true`.

**Fix:**
- Apply the same alias/anchor/merge-key rejection to the users file as to the
  config file (reuse the `checkNodes` walk or equivalent), so a wildcard ACL
  cannot be hidden inside a merge key. Aligns with PLAN §77 ("wildcard model
  permission should be explicit and easy to spot in review").

**Verify:**
- Integration test: a users file with `&tpl` + `<<: *tpl` granting
  `models: ["*"]` must be rejected (or surfaced explicitly), not silently
  accepted.
- Confirm `key list` and the serving path both use the guarded loader.

**Done when:** merge keys/anchors in the users file are rejected, so a wildcard
ACL cannot be hidden from review.

---

### T-M10 — MEDIUM — Revocation does not work the way the CLI implies

**Finding:** (a) `key revoke` of the last key fails (`users file needs at least
one key`); (b) `key disable` prints success but the running daemon does not
reload (§30 SIGHUP reload absent); (c) SIGHUP *kills* the daemon instead of
taking the §74 graceful reload path.

**Fix:**
- Allow revoking/removing the last key (or provide a documented, explicit
  escape hatch), so a compromised single-key deployment can revoke through the
  CLI.
- Implement the §30 SIGHUP key-reload: on SIGHUP, atomically reload the users
  file and apply the new enabled state without killing the process or severing
  in-flight streams (§74 graceful path). The Landlock policy already grants the
  users-file read (PLAN §30).
- Wire `ExecReload`/`systemctl reload` in `deploy/mellomting.service` to the
  graceful reload.

**Verify:**
- CLI test: `key revoke` of the last key succeeds (or a documented equivalent).
- Integration test: `key disable` on a key, send SIGHUP to the running daemon;
  assert the key is rejected on the next request **without** a process restart
  and without severing in-flight streams; the process survives SIGHUP.
- Confirm `systemctl reload` performs the graceful reload (not a kill/restart).

**Done when:** revocation works through the CLI even for the last key, SIGHUP
gracefully reloads key state without killing the daemon, and the systemd unit
uses `ExecReload` for the graceful path.

---

### T-M11 — MEDIUM — Shutdown gaps

**Finding:** `serve.go:131-163` — a second SIGTERM is buffered-and-dropped with
its default terminate disposition suppressed (only SIGKILL escapes); `srv.Close()`
does not wait for handlers; `Enqueue` has no closed-state check (records after
`Close()` vanish without incrementing `dropped`); `Close()` declares `err` but
never assigns it, so a final-sync ENOSPC is invisible.

**Fix:**
- Re-register a hard second-signal termination path (second SIGTERM/SIGHUP
  forces exit and accounting flush, rather than being swallowed).
- Add a closed-state check to `Enqueue` so records after `Close()` are counted
  as dropped.
- Fix `Close()` to actually return/surface the final-sync error, and make the
  `if err := d.acc.Close(); err != nil` check live.

**Verify:**
- Integration test: send a second SIGTERM during a drain and assert the process
  exits promptly (not swallowed) and flushes accounting.
- Unit test: `Enqueue` after `Close()` increments `dropped`.
- Test that a final-sync error surfaces through `Close()`.

**Done when:** a second signal forces termination, post-close records are
counted as dropped, and the final-sync error is visible.

---

### T-M12 — MEDIUM — `backend_network.mode: any` emits no startup warning

**Finding:** `validate.go:360-363` — §16.3 requires an explicit opt-in *and* a
startup warning. The opt-in half holds; the warning is missing.

**Fix:**
- Emit a startup warning when `backend_network.mode: any` is in effect,
  matching the §8.2 plaintext-listener and landlock-weakening warnings.

**Verify:**
- Start the daemon with `mode: any` and assert a warning line appears in the
  structured logs (analogous to the other weakenings).
- `config check` on `mode: any` reports valid (unchanged) but a warning is
  logged.

**Done when:** `mode: any` produces the required startup warning.

---

### T-M13 — MEDIUM — A malformed `base_url` is echoed with its credentials

**Finding:** `validate.go:511-514` — a malformed `base_url` containing inlined
userinfo (`http://admin:pass@host\x7f`) is echoed in the error message, leaking
the credential. The well-formed-userinfo branch is safe.

**Fix:**
- Redact/sanitize the `base_url` in validation error messages (strip userinfo,
  or refuse to echo the full URL), so a malformed URL with credentials never
  leaks them to the operator/logs.

**Verify:**
- Test: a malformed `base_url` with inlined credentials produces an error that
  does **not** contain the password (and ideally not the userinfo).
- Confirm the well-formed-userinfo branch still does not leak.

**Done when:** validation errors never echo credentials embedded in a malformed
`base_url`.

---

### T-M14 — MEDIUM — DNS resolution happens before the connect timeout applies

**Finding:** `backend.go:269` — `LookupIPAddr` runs before `d.Timeout` is set,
and for streams `fctx` has no deadline. A silent resolver pins a global and
per-key slot indefinitely with no 504, and the failure is misclassified as
`ErrConnect`, poisoning passive health (feeding T-X6).

**Fix:**
- Apply the connect/dial timeout to the DNS resolution (set `d.Timeout` before
  `LookupIPAddr`, or use a context with the deadline).
- Ensure a DNS hang is bounded and classified as a timeout (not `ErrConnect`
  health-poisoning), and that it does not pin slots indefinitely.

**Verify:**
- Integration test with a resolver that hangs: the dial/DNS must be bounded by
  the configured timeout and return a 504/timed-out classification, and the
  slot must be released.
- Confirm the failure does not poison passive health as a connect error.

**Done when:** DNS resolution is bounded by the dial timeout and a slow resolver
cannot pin slots or misclassify as a connect failure.

---

## LOW findings (with surviving substance)

### T-L1 — LOW — Duplicate `X-Api-Key` silently first-wins (hardening)

**Finding:** duplicate `X-Api-Key` is silently first-wins while duplicate
`Authorization` is correctly rejected (`server.go:224-241`).

**Fix:** reject duplicate `X-Api-Key` headers like duplicate `Authorization`
(consistent hardening posture).

**Verify:** raw-socket/integration test: two `X-Api-Key` headers → 401 (rejected),
not first-wins.

**Done when:** duplicate `X-Api-Key` is rejected consistently with
`Authorization`.

---

### T-L2 — LOW — `allowFor` second route table emits self-contradictory `Allow` headers

**Finding:** `server.go:196-214` — `GET /v1/responses` → 405 with `Allow: GET,
POST`, and `POST /v1/models` → 405 with `Allow: GET, POST`.

**Fix:** make the `Allow` headers emitted by `allowFor` agree with the `route`
method guards (`GET /v1/responses` should not advertise `POST`; `POST /v1/models`
should not advertise `GET`), or remove the duplicate table and derive `Allow`
from `route`.

**Verify:** assert the `Allow` header on each 405 matches the route's actual
allowed methods.

**Done when:** `Allow` headers are protocol-correct and consistent with routing.

---

### T-L3 — LOW — Client `model` string logged before ACL check, untruncated

**Finding:** `proxy.go:221` (emitted at `:160`) logs the client's public `model`
with no truncation; a 16 MiB model name writes 16 MiB to the journal.

**Fix:** truncate the logged `model` string (e.g. to a fixed bound) before
logging.

**Verify:** test that logging a very long model name emits only the truncated
value.

**Done when:** log records never contain an unbounded `model` string.

---

### T-L4 — LOW — `key create --models does-not-exist` succeeds silently (warning fix)

**Finding:** `keyCreate` loads/validates the config and **discards** the model
list (`main.go:280`, `keyState(c)`), so a nonexistent model is accepted.

**Fix:** emit a warning when `--models` names a model not present in the loaded
config (a hard reject is not wanted, since an ACL may legitimately name a model
about to be added).

**Verify:** CLI test: `key create --models does-not-exist` succeeds but prints a
warning referencing the missing model.

**Done when:** a warning is emitted for models absent from the config.

---

### T-L5 — LOW — `affinity.Put` called without validating backend-supplied ID

**Finding:** `proxy.go:468`, `:669` — a backend returning a 900 KiB `id` is
stored whole (bounded only by `max_response_bytes`), evicting a legitimate
record.

**Fix:** bound/validate the response `id` before `affinity.Put` (e.g. reject or
skip IDs above a sane length).

**Verify:** test that an oversized backend `id` is not stored (or is bounded) and
does not evict legitimate entries.

**Done when:** oversized backend-supplied IDs cannot evict legitimate affinity
records.

---

### T-L6 — LOW — Unreachable nil-map panics behind `needsModel` (defence-in-depth)

**Finding:** `proxy.go:764`, `:920` — nil-map panics on a `null` body, currently
unreachable only because `shallowParse` rejects `null` first. Once T-X4 adds
`recover()`, these become harmless; keep them guarded and covered.

**Fix:** (with T-X4) ensure `recover()` containment covers these paths; optionally
harden the nil-map access so it is safe regardless of the guard.

**Verify:** with T-X4's containment test, force a `null` body and assert a 500
rather than a process crash.

**Done when:** no nil-map access can crash the process.

---

### T-L7 — LOW — `MaxResponseHeaderBytes`, `MaxConnsPerHost`, `DisableCompression` unset

**Finding:** from the review's "Smaller items": `MaxResponseHeaderBytes` and
`MaxConnsPerHost` unset (unbounded per-backend connections); `DisableCompression`
unset against §9.2's SHOULD.

**Fix:** set a bounded `MaxConnsPerHost`, a `MaxResponseHeaderBytes`, and
`DisableCompression: true` (per §9.2) on the backend client.

**Verify:** config/test asserts these are applied and that per-backend
connections are bounded (relates to T-X5/T-M18).

**Done when:** the backend client bounds per-host connections, response headers,
and disables compression per §9.2.

---

### T-L8 — LOW — `internal/config/policy.go` is a dead, diverged second implementation

**Finding:** the backend network policy is implemented three times
(`config/policy.go`, `backend.go:79`, `serve.go:351`) and the two live ones
differ (`ip.To4()` retry vs not; `netip.ParsePrefix` silently drops malformed
CIDR vs `net.ParseCIDR` failing closed).

**Fix:** remove the dead `config/policy.go` implementation (or consolidate), and
make the surviving implementations fail closed on malformed CIDR identically.

**Verify:** delete/consolidate; grep confirms one implementation; tests cover
malformed CIDR failing closed.

**Done when:** the network policy has a single, fail-closed implementation.

---

### T-L9 — LOW — `safeUnixListen` unlinks a socket without a liveness check

**Finding:** a second instance silently steals a Unix socket.

**Fix:** perform a liveness check (attempt to connect, or check for an active
owner) before unlinking an existing socket, refusing to steal a live one.

**Verify:** start a second instance on the same Unix socket; it must refuse
(rather than silently steal) while the first is alive.

**Done when:** a live Unix socket is never silently stolen.

---

### T-L10 — LOW — `ErrorLog` is nil; panics/TLS errors go to stderr outside JSON pipeline

**Finding:** `net/http` writes plaintext panic stacks and TLS handshake errors to
stderr outside the structured JSON log pipeline.

**Fix:** set `http.Server.ErrorLog` to a logger that routes through the structured
logging pipeline (sanitized), so no plaintext stack/TLS error bypasses it.

**Verify:** trigger a TLS handshake error and assert it appears in the structured
logs, not raw stderr.

**Done when:** all `net/http` errors flow through the structured, sanitized log
pipeline.

---

### T-L11 — LOW — `defer os.Remove(d.listenAddr())` runs for TCP listeners too

**Finding:** `serve.go:96` unlinks a `host:port`-named relative path on shutdown
even for TCP listeners, outside `safeUnixListen`'s symlink guards.

**Fix:** only unlink for Unix-socket listeners (inside `safeUnixListen`'s guards).

**Verify:** shutdown of a TCP listener does not attempt an `os.Remove`; Unix
listener behaviour unchanged.

**Done when:** `os.Remove` runs only for Unix-socket listeners.

---

### T-L12 — LOW — `ln.Close()` after `Shutdown` always fails (WARN noise)

**Finding:** every clean shutdown logs `WARN listener close: use of closed
network connection`.

**Fix:** only close the listener if `Shutdown` did not already close it (e.g.
ignore/treat `ErrServerClosed` / closed-connection as expected).

**Verify:** clean shutdown produces no WARN about closed network connection.

**Done when:** clean shutdown logs no spurious close warning.

---

### T-L13 — LOW — Second SIGTERM does nothing; SIGKILL skips accounting flush

**Finding:** (relates to T-M11) a second SIGTERM is swallowed and only SIGKILL
escapes, skipping the accounting flush.

**Fix:** handled with T-M11 (hard second-signal termination that flushes
accounting).

**Verify:** as T-M11.

**Done when:** as T-M11.

---

### T-L14 — LOW — `backend.go:349-355` `default:` makes `<-ctx.Done()` unreachable when queue full

**Finding:** a client disconnect while the queue is full is logged as
`queue_full` (misleading capacity planning) instead of as a client disconnect.

**Fix:** check the context cancellation before/in addition to the `default:` so a
client disconnect is classified correctly.

**Verify:** test that a client disconnect while queued logs the disconnect class,
not `queue_full`.

**Done when:** client disconnects are classified correctly even when the queue is
full.

---

### T-L15 — LOW — No cap on accepted connections (fd exhaustion)

**Finding:** unbounded accepted connections (800 idle → 812 fds), bounded only by
`idle_timeout` and fd limit; the systemd unit sets no `LimitNOFILE=`.

**Fix:** add a connection cap (e.g. a bounded `MaxHeaderBytes`/accept limiter or
an explicit max-connections gate) and/or set `LimitNOFILE=` in the systemd unit.

**Verify:** config/test asserts the connection bound; systemd unit review shows
`LimitNOFILE=` set.

**Done when:** accepted connections are bounded and the unit sets a sensible
`LimitNOFILE=`.

---

### T-L16 — LOW — Landlock grants `ReadFiles: [users_file]` for a non-existent feature

**Finding:** `serve.go:225` grants the users-file read for a SIGHUP reload that is
not implemented. Free tightening (resolved when T-M10 implements SIGHUP reload —
then the grant becomes used; otherwise remove it).

**Fix:** once T-M10 implements reload, keep the grant (now used); if reload is
not implemented in this pass, remove the grant.

**Verify:** with T-M10, confirm the users-file read is exercised by the reload
path; without it, the policy no longer grants it.

**Done when:** the Landlock grant matches an actually-implemented feature.

---

## Quality / toolchain gates

### T-Q1 — QUALITY — `staticcheck` not clean on darwin (SA4023)

**Finding:** `serve.go:259` SA4023 because `apply_other.go:12`'s `Apply` never
returns a nil interface. The stated gate (AGENTS.md) does not hold on darwin.

**Fix:** either scope the gate to `GOOS=linux` in AGENTS.md and CI, or add a
targeted `//lint:ignore SA4023` with the reason.

**Verify:** `staticcheck ./...` is clean on the supported developer platform(s);
document the scoping in AGENTS.md.

**Done when:** the staticcheck gate is consistent with AGENTS.md and clean.

---

### T-Q2 — QUALITY — `make check` cannot fail on formatting; omits staticcheck/govulncheck

**Finding:** `Makefile:41-47` — `gofmt -l .` exits 0 while listing files, so the
gate does not gate; `check` also omits staticcheck and govulncheck.

**Fix:** make the format gate fail when `gofmt -l` lists files (e.g.
`test -z "$$(gofmt -l .)"`), and add staticcheck/govulncheck to the `check`
target to match its "full quality gate (AGENTS.md)" claim.

**Verify:** introduce a deliberately unformatted file and confirm `make check`
fails; confirm staticcheck/govulncheck run under `make check`.

**Done when:** `make check` actually fails on formatting and runs all gates.

---

### T-Q3 — QUALITY — CI never checks the non-Linux build

**Finding:** every CI job is `ubuntu-latest`; `landlock_other.go`/`apply_other.go`
are never vetted/staticchecked, and `TestApplyNonLinuxFailsClosed` takes its
Linux branch in CI (its fail-closed assertion never runs).

**Fix:** add `GOOS=darwin go vet ./...` (and a darwin `go test` where feasible) to
CI so the non-Linux build and its fail-closed test are exercised.

**Verify:** CI runs a darwin vet/test job and `TestApplyNonLinuxFailsClosed`
executes its non-Linux branch.

**Done when:** the non-Linux build is vetted and its fail-closed test runs in CI.

---

### T-Q4 — QUALITY — Comments that misstate security properties

**Finding:** misleading comments: `proxy.go:559` "can never inject a path";
`affinity.go:12` (fixed by T-X1); `limiter.go:163` (fixed by T-X8);
`backend.go:320-322` (fixed by T-X4); `backend.go:447-448`; `routing` doc calls
`least-inflight` deterministic.

**Fix:** correct each comment to match the fixed behaviour; the routing doc should
not claim determinism that the strategy does not guarantee.

**Verify:** grep the listed lines and confirm the comments match behaviour; review
the routing package doc.

**Done when:** no comment misstates a security or determinism property.

---

### T-Q5 — QUALITY — `case err == auth.ErrDisabled` uses `==` not `errors.Is`

**Finding:** `httpapi/server.go:262,264` — one wrap upstream and every
disabled/expired key silently logs as `auth_unknown`. Both arms at count 0.

**Fix:** use `errors.Is`; add tests for the disabled and expired branches.

**Verify:** test that a wrapped `ErrDisabled`/expired error is classified
correctly (not `auth_unknown`).

**Done when:** disabled/expired keys are classified correctly through wrapping.

---

### T-Q6 — QUALITY — Double-maintained tables can drift

**Finding:** scheme→port (`backend.go:240`/`landlock/policy.go:65`), `MaxABI = 9`
in three places, `MinABI = 8` in two, strategy list, landlock mode list.
`config` does not import `landlock`, so a go-landlock bump will not propagate
ABI constants; a scheme→port drift would make the sandbox deny the port the
dialer uses.

**Fix:** consolidate the ABI constants and the scheme→port table into single
sources of truth (with a test that the dialer and Landlock agree on the ports).

**Verify:** a test asserts the Landlock-granted ports equal the dialer's used
ports for the configured schemes; ABI constants come from one place.

**Done when:** no duplicated constant can drift, and a test guards the
dialer/Landlock port agreement.

---

### T-Q7 — QUALITY — Dead code staticcheck cannot see (exported identifiers)

**Finding:** `backend.AsUpstream`, `backend.Name`, `backend.UpstreamModel`,
`accounting.SettledTokens`, `accounting.UsagePartial`, all of `config/policy.go`
(see T-L8), `main.go:20` `plannedCommands`, and all of `securefile`'s permission
helpers (see T-M8). Plus `accounting.overflow` parsed/defaulted/validated while
`WriterConfig` has no such field (like the dead `stream_idle_timeout`).

**Fix:** remove or wire up each dead export; align `accounting.overflow` config
with the `WriterConfig` it governs (see T-M6). Remove `plannedCommands` and the
unreachable `main.go:51-52`.

**Verify:** after removal, grep confirms no references; `go vet`/`staticcheck`
clean; the `accounting.overflow` knob actually governs the writer.

**Done when:** no dead exported identifier remains, and every parsed config knob
governs a real behaviour.

---

### T-Q8 — QUALITY — `sse.go:48-55` unreachable `have` fast path

**Finding:** the `if p.have` fast path is only entered after a read error, after
which no caller re-enters; if ever reachable it would emit a partial event
without its terminating blank line.

**Fix:** remove the unreachable branch or make it safe (emit a terminating blank
line if kept).

**Verify:** fuzz/unit the SSE parser and confirm no path emits a partial event
without its terminator.

**Done when:** the fast path is either removed or safe.

---

### T-Q9 — QUALITY — `docs/PLAN.md:2606` reference config says `max_inflight_requests: 32`

**Finding:** the reference config says 32 while §9.1, `:1457`, and
`validate.go:20` all say 64.

**Fix:** correct PLAN.md:2606 to 64.

**Verify:** grep PLAN.md for `max_inflight_requests` and confirm all values agree.

**Done when:** PLAN.md reference config matches the code default (64).

---

### T-Q10 — QUALITY — `_ = cancel` discards the `/cancel` suffix

**Finding:** `httpapi/server.go:284` computes whether the path ended in `/cancel`
and throws it away, so `GET /v1/responses/{id}/cancel` routes to retrieve rather
than 405, while `allowFor:206-211` believes it is POST-only.

**Fix:** use the computed `cancel` value to route correctly (405 for GET on a
POST-only cancel path).

**Verify:** test `GET /v1/responses/{id}/cancel` returns 405 (not 200 retrieve);
the `Allow` header agrees.

**Done when:** the `/cancel` suffix governs routing and the `Allow` header agrees.

---

### T-Q11 — QUALITY — `localhost` loopback handling inconsistent; MUST-warn never fires

**Finding:** `config/validate.go:711` vs `cmd/mellomting/serve.go:485` —
`listen: localhost:8080` without TLS is refused until
`allow_plaintext_non_loopback: true`, at which point `serve.go:98` suppresses
the warning that flag exists to emit. §8.2's MUST-warn never fires for
`localhost`.

**Fix:** treat `localhost` consistently in both files and ensure the §8.2
plaintext-non-loopback warning fires for `localhost` as required.

**Verify:** start with `listen: localhost:8080` (no TLS) and confirm the §8.2
warning appears when `allow_plaintext_non_loopback: true`; config check rejects
it otherwise.

**Done when:** `localhost` handling is consistent and the MUST-warn fires.

---

### T-Q12 — QUALITY — Two byte-identical error writers; proxy 429s ship no `Retry-After`

**Finding:** `httpapi/errors.go:32` vs `proxy.go:1058` — only the httpapi one sets
`Retry-After`, so the proxy's 429s (quota `:319`, upstream `:481`) ship none.

**Fix:** set `Retry-After` on proxy 429 responses too (or consolidate the error
writers).

**Verify:** test that a proxy quota/upstream 429 includes `Retry-After`.

**Done when:** all 429 responses carry `Retry-After`.

---

## Test-suite integrity

### T-T1 — QUALITY — `backend_test.go` vacuous header-sanitation assertions

**Finding:** `backend_test.go:70,79` assert `X-Api-Key`/`Authorization` are not
forwarded, but the fixture's `Headers` map contains only `User-Agent`, so the
assertion is vacuous; `Forward` filters nothing (`backend.go:396-400`). The real
allow-list is at `proxy.go:855-866`, both arms at coverage 0.

**Fix:** fix the fixture to actually include the sensitive headers and assert
they are stripped at the proxy layer; add coverage for both arms of the
allow-list.

**Verify:** the corrected test fails on the old code and passes on the fixed
code; both allow-list arms are covered.

**Done when:** the header allow-list is genuinely tested at the layer the test
claims, with both arms covered.

---

### T-T2 — QUALITY — No test JSON-decodes a proxy error body

**Finding:** `proxy_test.go:334` asserts only a `Code` and `Contains`, which is
why X7 survived. (Resolved as part of T-X7.)

**Verify:** the T-X7 test JSON-decodes the body.

**Done when:** T-X7's decode test exists.

---

### T-T3 — QUALITY — Cross-key test uses unknown ID (resolved by T-X1)

**Verify:** as T-X1 — replace with a real two-key test using a known ID.

**Done when:** T-X1's two-key integration test exists.

---

### T-T4 — QUALITY — `server_test.go:180` "query param ignored" case is a tautology

**Finding:** the case sets a valid bearer, making it byte-identical to "bearer
ok"; the table lacks disabled→401, expired→401, and 3 of 4 malformed-bearer
branches.

**Fix:** give the query-param case a distinguishing property, and add the missing
auth branches (disabled, expired, all malformed-bearer variants).

**Verify:** each added case asserts its distinct outcome and can fail on a wrong
classification.

**Done when:** the auth table tests disabled, expired, and all malformed-bearer
branches, and the query-param case is non-tautological.

---

### T-T5 — QUALITY — `backend_test.go:181` `errors.Is(err, ErrTimeout)` on `ErrHeaderTimeout`

**Finding:** two bare sentinels with no wrapping, so `errors.Is` is a tautology.

**Fix:** assert the actual classification explicitly (and exercise T-X6's
non-streaming fix).

**Verify:** the corrected assertion can fail when the classification is wrong.

**Done when:** the timeout classification test is meaningful.

---

### T-T6 — QUALITY — `accounting_test.go:234` `w.Dropped() == n` tautology

**Finding:** both sides incremented by the same loop; tests `sync/atomic`, not
mellomting.

**Fix:** make `Dropped()` assertions exercise the real drop path (relates to
T-M6/T-M11 closed-state).

**Verify:** the corrected test fails when the drop accounting is wrong.

**Done when:** the dropped-counter test exercises the real path.

---

### T-T7 — QUALITY — `TestHugeSSEEventBounded` trips the per-line bound, not `ErrSSEEventTooLarge`

**Finding:** a single 1 MiB *line* trips the 256 KiB per-line bound first;
`ErrSSEEventTooLarge`/`maxSSEEvent` has no coverage.

**Fix:** use a fixture of many short `data:` lines totalling > 1 MiB so the
`ErrSSEEventTooLarge` path is exercised.

**Verify:** the corrected fixture triggers `ErrSSEEventTooLarge` (not the
per-line bound).

**Done when:** `maxSSEEvent`/`ErrSSEEventTooLarge` is covered.

---

### T-T8 — QUALITY — `retry_test.go:165` stream-never-retried test cannot fail

**Finding:** it writes a complete event then closes cleanly and `dispatch`
returns before the retry loop once `stream == true`, so the assertion holds for
a healthy stream.

**Fix:** add coverage of the actual stream-abort path and idle timeout (the
stream-abort and idle-timeout lines are count 0).

**Verify:** the new test exercises a mid-stream abort and asserts no retry, and
covers the idle-timeout path.

**Done when:** stream-abort and idle-timeout paths are covered.

---

### T-T9 — QUALITY — Coverage gaps at fail-closed boundaries

**Finding:** `config/validate.go` 57/98 rejection sites uncovered; `validateUsers`
zero rejection tests; Responses API, `/v1/completions`, `/v1/embeddings` never
routed through `httpapi` by any test; `responsesID`/`isSafeSegment` at 0%;
`MaxResponseBytes`/`ErrTooLarge`, `MaxHeaderBytes`, the byte budget, and
affinity eviction at 0.

**Fix:** add tests covering each listed fail-closed boundary and route the
Responses/completions/embeddings endpoints through the real `httpapi` stack.

**Verify:** coverage reports show the listed sites no longer at 0; the
`route`/`allowFor` disagreement is structurally visible to the suite (single
route table).

**Done when:** the named fail-closed boundaries and endpoints are covered.

---

### T-T10 — QUALITY — PLAN §80–§87 required tests missing

**Finding:** §80 fuzz targets (13 applicable) → only 1; §81 30 required → 17
present, 5 partial, 8 missing/unsound; §82 16 → 4, 1 partial (X2 never
executes); §83 source grep only; §84 16 → 8; §87 benchmarks → 0.

**Fix:** add the missing fuzz targets for the parsers PLAN §80 names (following
the review's 18-target list), the §81 HTTP/security cases listed above, §82
Landlock integration tests runnable on an ABI ≥ 8 kernel (see T-X2), §84 fake
backend cases, and §87 benchmarks. Two §81 priorities: a **slow-client write
bound** test (T-X9) and **log-scrubbing** tests that capture log output and
assert a secret's absence (currently every test uses `discardLogger()`).

**Verify:** each added test executes in CI and can fail on a regression; the
log-scrubbing tests capture real log output and assert no key/prompt/password
appears.

**Done when:** the PLAN §80–§84/§87 required tests are present, executable, and
meaningful.

---

## Suggested sequencing

Work in this order (each group is internally orderable, and each task includes
its own verification):

1. **Critical + High correctness bugs** that change behaviour first:
   T-X1, T-X1b, T-X4 (contains the structural `recover()`), T-X7, T-X5, T-X2
   (Linux-only, needs the ABI ≥ 8 host), T-X3 (with T-X4), T-X6, T-X9, T-X8.
2. **Medium security/accounting:** T-X10, T-X11, T-M7, T-M8, T-M9, T-M10,
   T-M1, T-M2, T-M4, T-M5, T-M14.
3. **Remaining Medium stability/resource:** T-M3, T-M6, T-M11–T-M13,
   T-X12, T-X13.
4. **LOW hardening:** T-L1–T-L16.
5. **Quality + test-suite integrity:** T-Q1–T-Q12, T-T1–T-T10.

Dependencies: T-X3 and T-X4 must be fixed together (T-X4's nil-body guard is
required so the T-X3 redirect fix does not crash). T-M10 (SIGHUP reload)
interacts with T-L16 (Landlock grant) and T-M11 (signals). T-X8's defaults
interact with T-X6 (which needs concurrency to amplify) and T-X9 (single-key
hold). T-X5 is the root of the per-backend bound; T-M18/T-L7 touch the same
client config.

After every task, run the full quality gate. Do not weaken a security
requirement to make a test pass.
