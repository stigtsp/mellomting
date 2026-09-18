# Mellomting — security, stability, and code-quality review

> **Historical document — every finding below is fixed.** All 14 `T-X` findings were
> remediated in `b4beec4..11debd2`; `docs/FIXES_PLAN.md` carries the per-finding tasking
> and `docs/FIX_PLAN_2026-08-22.md` the follow-up round (all items `[x]`). The
> reproductions here describe commit `62aa81d` and do **not** apply to current `main` —
> e.g. T-X1's `singlePossibleBackend` fallback no longer exists; the Responses
> retrieve/cancel path now fails closed (`internal/proxy/proxy.go`, fixed in `b4beec4`).
> Kept for provenance. This is not a live disclosure.

**Status: complete.** Four rounds were run: security by boundary slice (12 reviewers), stability and
concurrency, spec conformance and code quality, and adversarial verification in which two reviewers were
tasked with *refuting* the accumulated findings rather than confirming them. Several findings were
downgraded or refuted in that last round; the corrected versions are what appear here.

- Reviewed commit `62aa81d` (Phase 6). Working tree clean apart from an untracked handover note.
- Reviewed against `docs/PLAN.md` (RFC 2119) and its §4 security invariants.
- Host darwin/arm64, Go 1.26.7. Linux-only code was cross-compiled and read but **never executed** —
  see *Coverage and limits*.
- **No repository file was modified by this review** other than this document. Every reproduction test
  was run against a throwaway copy of the tree in a scratchpad directory.

## How to read the provenance labels

Each finding carries one:

- **executed** — reproduced by running code; the observed output is quoted.
- **inspection** — derived by reading the code (and, where noted, the pinned dependency's source).
  Nothing was executed.

Several findings were reached independently by more than one reviewer working a different slice. That
is recorded as a confidence signal, not as separate findings — they are merged here by mechanism.

---

## Triage

| # | Sev | Finding | Anchor | Provenance |
|---|---|---|---|---|
| X1 | **CRITICAL** | Any key can read *and cancel* another key's response | `proxy.go:255-266` | executed |
| X1b | HIGH | `weighted-round-robin` panics on every request once any replica is excluded | `router.go:179` | executed ×2 |
| X2 | HIGH | Landlock rule is malformed; daemon cannot start sandboxed | `apply_linux.go:51` | inspection ×3 |
| X3 | HIGH | Backend redirects followed → authenticated SSRF + credential disclosure | `backend.go:189` | executed |
| X4 | HIGH | Nil-body panic in a spawned goroutine kills the process; no `recover()` anywhere | `backend.go:410`, `proxy.go:436` | executed ×2 |
| X5 | HIGH | Per-backend concurrency does not bound streams; `Inflight()` is wrong | `backend.go:377`, `:484` | executed ×4 |
| X6 | HIGH | No non-stream generation over 30s can succeed; failures then cool backends out | `backend.go:177` | executed |
| X7 | HIGH | Every terminal upstream non-2xx returns two concatenated JSON errors | `proxy.go:474-511` | executed |
| X8 | MED-HIGH | Per-key limits fail **open**; one key can starve every other key | `limiter.go:80-86` | executed |
| X9 | HIGH | Non-streaming write has no deadline; one key denies service to all | `proxy.go:464-465` | executed |
| X10 | MEDIUM | Unknown-usage quota charge is client-controlled (prefill escapes) | `proxy.go:918-924` | executed |
| X11 | MEDIUM | A single backend-reported `total_tokens` can void a key's quota window | `quota.go:50,67` | executed ×2 |
| X12 | MEDIUM | A body of many short keys costs 13× its size in peak heap | `budget.go:9-12` | executed |
| X13 | MEDIUM | A stream whose last event lacks a blank line is silently truncated | `sse.go:97-101` | executed |
| M1–M19 | MEDIUM | 19 further findings — see *Medium findings* and *Stability* | | |
| — | quality | test-suite integrity and code quality — see those sections | | |

Ordered by ID; severity is column 2. "×N" in Provenance means N reviewers reached the finding
independently — a confidence signal, not a count of separate defects.

---

## X1 — CRITICAL — Any key can read and cancel another key's response

`internal/proxy/proxy.go:255-266`, `:705-724` — **executed (orchestrator).**

PLAN §21.1 states the requirement without hedging:

> Bind response state to the API key that created it.
> A different key must not be able to retrieve/cancel/use the response **even if it obtains the ID**.

The affinity map itself is correct — it *is* keyed by `(KeyID, RespID)` (`affinity.go:16-19`). The break
is the fallback wrapped around it. For `GET /v1/responses/{id}` and `POST /v1/responses/{id}/cancel`:

```go
if b, ok := p.affinity.Get(q.Key.ID, q.ResponseID); ok {
    fixedBackend = b
} else if only := p.singlePossibleBackend(q.Key); only != "" {
    fixedBackend = only          // ← forwarded with NO ownership check
} else {
    fail(404, …)
}
```

On a miss the request is forwarded on the strength of *backend reachability alone*. These routes carry
`needsModel: false`, so no model ACL runs either — no ownership check of any kind remains.

Reproduced end-to-end with one model, one backend, two keys:

```
key B  POST /v1/responses                     → 200 {"id":"resp_SECRET1",…}
key A  GET  /v1/responses/resp_SECRET1        → 200 {"id":"resp_SECRET1",…,
                                                     "output":[{"content":[{"text":"KEY B PRIVATE PROMPT AND ANSWER"}]}]}
key A  POST /v1/responses/resp_SECRET1/cancel → 200
```

Key A reads key B's stored response content verbatim, and can terminate key B's generation.

**Preconditions, stated precisely** — these are what make this credible rather than alarmist.
`singlePossibleBackend` unions `BackendsFor()` over **only the requesting key's** allowed generation
models. So it is *not* a "tiny deployment only" bug: any key scoped to one model with one replica
qualifies, even inside a large multi-backend cluster. Conversely it **fails closed** when the requesting
key spans several backends — that path 404s correctly. The attacker must know the response ID; §21.1
explicitly assumes exactly that ("even if it obtains the ID"), and `THREAT_MODEL.md` lists "cross-key
response-ID access" among the threats mellomting is meant to address, so ID entropy is not an available
defence. IDs leak through ordinary channels: they are returned to the creating client and appear in
client logs and agent transcripts.

§21.3's "if there is one and only one possible backend, MAY be forwarded there" is a MAY about *which
backend to select* for an ID whose owner is unknown. It cannot license dropping §21.1's prohibition.

The `POST /v1/responses` + `previous_response_id` path is **not** affected: it uses
`affinity.Get(q.Key.ID, prev)` and 404s on a miss (`proxy.go:243-252`). Only retrieve and cancel are
broken.


## X1b — HIGH — `weighted-round-robin` panics on every request once any replica is excluded

`internal/routing/router.go:179`, `:215-241` — **executed (reviewer and orchestrator, independently).**

`nextWRR` returns `best`, which it draws *from* `cands` — so the return value is already a **replica
index**:

```go
best := cands[0]
for _, i := range cands[1:] { if st.current[i] > st.current[best] { best = i } }
st.current[best] -= total
return best                       // ← a replica index
```

But `Select` treats it as a *position within* `cands`:

```go
case "weighted-round-robin":
    pick = cands[r.nextWRR(publicModel, entry, cands)]   // ← double indexing
```

Every other strategy gets this right: `single` uses `cands[0]`, `least-inflight` computes
`best := cands[0]` and assigns `pick = best` with no second lookup, and `round-robin` is correct because
it genuinely produces a *position* (`n % len(cands)`). Weighted round-robin is the only one that indexes
twice.

It is masked while every replica is healthy and nothing is excluded, because then `cands == [0,1,…,n-1]`
and `cands[i] == i`. That is exactly the state the existing `router_test.go` exercises. The moment
`cands` becomes sparse — passive health puts a replica in cooldown (PLAN §70), or the retry loop passes a
non-empty `exclude` (PLAN §23) — the index is wrong.

I reproduced it independently of the reviewer, with a 3-replica WRR model and `exclude=["b"]`:

```
attempt 0 -> "a" (legal)
attempt 1 exclude=[b]: PANIC runtime error: index out of range [2] with length 2
attempt 2 -> "a" (legal)
attempt 3 exclude=[b]: PANIC runtime error: index out of range [2] with length 2
attempt 4 -> "a" (legal)
attempt 5 exclude=[b]: PANIC runtime error: index out of range [2] with length 2
```

Note the second half of the defect visible above: on the non-panicking attempts it returns `"a"` every
time and never `"c"`, so even when it does not crash it is not distributing weight as configured — and in
the reviewer's run it returned a backend the retry loop had explicitly excluded, against PLAN §23's rule
that fallback never repeats a tried backend.

The reviewer also caught it firing through the real HTTP stack during a 200-goroutine stress run, i.e.
from an ordinary client request:

```
http: panic serving 127.0.0.1:60279: runtime error: index out of range [1] with length 1
routing.(*Router).Select   routing/router.go:179
proxy.(*Proxy).dispatch    proxy/proxy.go:360
httpapi.(*Server).route    httpapi/server.go:155
```

**Impact.** One backend hiccup turns into a hard failure of *every* request for that model for the whole
5s–60s cooldown window. Unlike X4 this panic is on the handler goroutine, so `net/http` recovers it
per-connection and the process survives — but the client gets a torn connection with **no**
OpenAI-shaped error body (PLAN §72), and the deferred structured request log never records a real
status. The deferred `budget.Release`, `<-s.inflight`, and `releaseKey()` all still run, so there is no
permanent slot leak.

**Severity is HIGH rather than CRITICAL because the strategy is opt-in**: `validate.go:224-228` defaults
to `single` for one backend and `least-inflight` for several, and PLAN §76's suggested configuration uses
`least-inflight`. An operator must explicitly choose `weighted-round-robin` — a natural choice for
heterogeneous GPUs, and PLAN §19 lists it as supported. As it stands the strategy is unusable.

Fix: `pick = r.nextWRR(…)` — drop the outer index — or make `nextWRR` return a position within `cands`.

## X2 — HIGH — The Landlock rule is malformed; the daemon cannot start sandboxed

`internal/landlock/apply_linux.go:51` — **inspection only** (no Linux host). Reached independently by
three reviewers.

```go
writeFile := ll.AccessFSSet(llsys.AccessFSWriteFile | llsys.AccessFSMakeReg)
for _, f := range pol.WriteFiles { rules = append(rules, ll.PathAccess(writeFile, f)) }
```

`pol.WriteFiles` is the accounting JSONL path, always a **regular file**
(`internal/accounting/writer.go:50` opens it `O_CREATE|O_APPEND|O_WRONLY`).
`LANDLOCK_ACCESS_FS_MAKE_REG` is a **directory-only** right, and `landlock_add_rule` returns `EINVAL`
when a directory-only right appears in `allowed_access` for a rule whose object is a regular file.

I verified the mechanism against the pinned `go-landlock@v0.9.0` source rather than taking it on trust:

- `landlock/config.go:13` — the file-only mask `accessFile` is `Execute | WriteFile | Truncate | ReadFile`.
  It deliberately **excludes** `AccessFSMakeReg`.
- `landlock/path_opt.go:183-188` — `ROFiles`/`RWFiles` build rights as `accessFSRead & accessFile` etc.,
  masking directory-only rights off precisely so this cannot happen, and set `enforceSubset: false`.
- `landlock/path_opt.go:138-144` — `PathAccess` sets `enforceSubset: true`.
- `landlock/path_opt_linux.go:15-19` — when `enforceSubset` is true the library does **not** intersect
  the requested rights with anything; the raw set reaches the kernel unmasked.
- `landlock/abi_versions.go:53-59` — ABI 8's `supportedAccessFS` is `(1<<16)-1`, i.e. it *includes*
  `MakeReg`. So the library's own `isSubset(c.handledAccessFS)` guard (`path_opt.go:88-95`) passes and
  does not catch the mistake earlier. (This was the discriminating check: had ABI 8 excluded the bit,
  the failure would surface as a different, library-side error.)
- `landlock/path_opt_linux.go:49-51` — the library carries a dedicated message for this exact outcome:
  *"inconsistent access rights (using directory access rights on a regular file?)"*.

Mellomting takes the one code path that bypasses the library's guard against this mistake.

**Consequence.** With PLAN §76's recommended configuration (`landlock.mode: required` plus
`accounting.enabled: true`) on a Landlock-capable kernel, `Apply` errors and `serve.go:110-115` exits 1 —
the daemon does not start. The obvious operator workaround is `mode: best-effort`, which logs one
warning (`serve.go:240`) and then serves production traffic with **no confinement at all**. That is how a
startup failure becomes a silent security regression. Phase 4's entire value is inert either way.

**Why it was never caught**, confirmed independently: `apply_linux_test.go:39` calls
`lltest.RequireABI(t, 8)`, so `TestAllThreadsEnforced` **skips** below Landlock ABI 8 (Linux 6.15+), and
`.github/workflows/ci.yml` runs `go test ./...` on `ubuntu-latest`, whose kernel is below that. The test
is written correctly — `apply_linux_test.go:71-74` builds `WriteFiles` from a regular file and
`t.Fatalf`s on the error at `:107` — it has simply never executed. A skipped security acceptance test
is indistinguishable from a passing one in the CI summary.

PLAN §56 requires all-thread behaviour to be tested and §95 lists it as Phase 4 acceptance; neither is
met today.

**Caveat, stated plainly:** this is a source-level derivation of kernel behaviour, not an observation.
One run of `go test ./internal/landlock` on a kernel with Landlock ABI ≥ 8 settles it.

Fix direction: drop `AccessFSMakeReg` (the file already exists and is opened before enforcement, so
creation rights are unnecessary), or use the library's `RWFiles` helper and let it mask correctly.

## X3 — HIGH — Backend redirects are followed: authenticated SSRF and credential disclosure

`internal/backend/backend.go:189` — **executed (reviewer).**

`&http.Client{Transport: t}` sets no `CheckRedirect`, so Go's default applies: up to 10 redirects
followed automatically, with the body replayed on 307/308 via `bytes.NewReader`'s `GetBody`. The
**backend**, not the operator, therefore chooses the effective URL.

Reproduced end-to-end through the real proxy stack:

- A fake vLLM answering `POST /v1/chat/completions` with `302 → /metrics` caused the client to receive
  `content-type: text/plain; version=0.0.4`, body `vllm:num_requests_running 3\nADMIN_ONLY_DATA`.
- A `302` to a *different loopback port* caused that second service to log
  `path=/admin/keys  auth="Bearer s3cr3t-backend-token"`, and its response body was returned to the
  client.

So this is an authenticated SSRF read primitive **and** delivery of the backend credential to an
unintended service. It directly contradicts §4 invariant 8 (client requests reach only an allow-list of
inference paths) and §11.3, which names `/metrics` as unreachable. The egress policy still applies
(redirect dials go through `policyDial`) and Landlock §60 pins ports on a hardened Linux host, so the
reachable set is "any allowed IP/port, any path" rather than the open internet.

**Do not apply the one-line fix alone.** Setting `CheckRedirect` to `http.ErrUseLastResponse` makes a 3xx
surface as a response — which then hits `backend.go:410` (not 200, and `< 400`), returning a nil error
with a nil `Body`, i.e. exactly the X4 crash. Fix both together and classify any 3xx as an upstream
error rather than relaying it.

## X4 — HIGH — Nil-body panic in a spawned goroutine terminates the process

`internal/backend/backend.go:410`, `internal/proxy/proxy.go:436`, `:629` — **executed (orchestrator and
reviewer, independently).**

`streamBody := req.Stream && resp.StatusCode == http.StatusOK`. When a `stream:true` request is answered
with a 2xx that is **not** 200 (201–208, 226), `streamBody` is false, the body is buffered and closed,
and `Forward` returns `&Result{BodyBytes: data, Body: nil}` with a **nil error** — the `>= 400` branch at
`backend.go:424` is not taken. This contradicts `Result`'s own doc comment at `backend.go:320-322`
("Body is live only for 2xx stream responses"). `dispatch` then branches on the *client's* `stream` flag
rather than on whether a live body exists, and calls `pump`, which builds `newSSEParser(nil)` and reads
it in the goroutine spawned at `proxy.go:629`:

```
panic: runtime error: invalid memory address or nil pointer dereference
bufio.(*Reader).fill → sseParser.readLine (sse.go:102) → nextEvent (sse.go:57)
  → proxy.pump.func1 (proxy.go:630)
created by mellomting/internal/proxy.(*Proxy).pump in goroutine 37 (proxy.go:629)
```

The panic is in a goroutine the handler *spawned*, so `net/http`'s per-connection `recover()` cannot
reach it. One reviewer additionally confirmed that a deferred `recover()` in the handler does **not**
catch it and the binary died. The process terminates, dropping every other tenant's in-flight stream.

The structural half matters more than the specific trigger: **`grep -rn 'recover()'` over the whole
repository returns nothing.** There is no panic containment anywhere. Every future nil-deref or index
error on the pump path is a full outage rather than a 500.

Reachability: needs a backend, or an intermediary in front of it (nginx, uvicorn, a health-draining load
balancer), to answer a streaming inference request with a 2xx other than 200. Stock vLLM does not, so
this is latent rather than directly client-triggered — the client controls only the `stream:true` half.
But X3 makes it directly reachable if the redirect fix is applied naively, and PLAN §5.1's robustness
envelope explicitly covers a *misbehaving* backend even though §5.3 puts a *malicious* one out of scope.

## X5 — HIGH — Per-backend concurrency does not bound streams, and `Inflight()` is wrong twice over

`internal/backend/backend.go:372-377`, `:429`, `:484-486` — **executed (orchestrator and three
reviewers, independently).**

`Forward` takes the admission slot under `defer h.release()`, but for a stream it returns as soon as
response *headers* arrive, handing back a live `Result.Body`. The deferred release runs while generation
is still in progress, so the slot covers only the header exchange. With `max_concurrency: 1,
queue_size: 8` and a backend that flushes SSE headers then blocks:

```
max_concurrency=1 but 5 streams are simultaneously live with headers received
Client.Inflight() while 5 streams are mid-generation = 0   (expected 1)
```

A second reviewer reproduced ten live streams admitted against `max_concurrency: 2, queue_size: 2`.

Separately, `Inflight()` is `len(c.queue) + len(c.conc)`, but `acquire` never removes the queue token on
admission — so an *active* request counts 2 and a *waiting* one counts 1 (verified: one active request
→ `Inflight() == 2`). Effective queue depth is therefore `queue_size - active`, not `queue_size`.

Consequences:

1. PLAN §22 / §4 invariant 13 — `max_concurrency` and `queue_size` do not bound concurrent *streaming*
   work, the dominant mode for LLM inference. An operator sizing `max_concurrency` to their GPU's batch
   capacity gets no protection, and the §22 `503 server_overloaded` is never emitted. Aggravated by
   `MaxConnsPerHost` being unset (unlimited): the semaphore was the only per-backend connection bound.
   Global `server.max_inflight_requests` and per-key concurrency *do* still apply (held across the pump
   in `internal/httpapi/server.go`), so this is a per-backend-cap defeat, not an unbounded free-for-all
   — though X8 shows the per-key half is itself unset by default.
2. `least-inflight` and `weighted-least-inflight` (PLAN §19) read this counter (`serve.go:424`). Because
   it reports 0 during streams and 2 for a single buffered request, the router will deterministically
   route new work to the *saturated* replica: a backend serving 100 live streams reports 0, while one
   handling a single embeddings call reports 2.

Existing tests miss it: `backend_test.go:188` (`TestInflightSnapshot`) uses `Stream: false`, and the
comment at `backend_test.go:246` asserts the opposite property for the non-streaming path only.

## X6 — HIGH — No non-streaming generation over 30s can succeed, and the failures cool backends out

`internal/backend/backend.go:177`, `internal/config/validate.go:30-31`, `internal/proxy/proxy.go:424-426`,
`:543-552`, `internal/routing/router.go:344-362` — **executed** in round-4 verification; reached
independently by two round-1 reviewers.

The adversarial verifier reframed this, and the reframing is the important part. The headline defect is
simpler than "health poisoning":

**On stock defaults a deployment cannot serve any non-streaming generation longer than 30 seconds.**
`header_timeout` defaults to 30s and is wired to `Transport.ResponseHeaderTimeout`. For a non-streaming
completion the backend sends no headers until the whole completion is finished, so time-to-headers ≈
full generation time. The 20-minute `request_timeout` is therefore unreachable for non-streaming work.
The proxy compounds its own exposure: `prepareOutbound` (`proxy.go:918-922`) *injects*
`max_completion_tokens = policy cap` when the client sent no limit, making the default request the
longest the policy allows.

Health poisoning is the **amplifier**, not the headline. `connectionLevelError` includes
`ErrHeaderTimeout` — asserted directly: `errors.Is(err, ErrHeaderTimeout) == true`, `ErrTimeout == false`
— so each such timeout calls `router.RecordFailure`. One failure suffices (streak 1, cooldown 5s → 10 →
20 → 40 → 60s cap), and the exclusion is keyed by **backend name alone**, so it spills across every
model — generation *and* embedding — and every key.

Reproduced end-to-end: two concurrent 900 ms non-streaming requests against a 2-replica model with
`header_timeout: 250ms` and `retry.max_attempts: 1` → both slow clients get `504 upstream_timeout`, then
an innocent fast client gets `503 {"code":"server_overloaded","message":"no backend is available"}` —
and so does a fast client **on a different model** sharing those backends.

Two corrections the verifier made to the original claim, both honoured here:

- "*All* clients get 503" is too broad. Only models whose **every** replica is simultaneously cooling
  are affected.
- Poisoning N replicas requires **concurrency**, not repetition: sequential 30s requests outlast the 5s
  base cooldown. X8 makes that concurrency free, since one key can hold every inflight slot.

Confirmed with no escape hatch: `lastErr` is declared immediately above the loop (`proxy.go:347`), so the
`lastFailed` health-bypass is unreachable on the only attempt the default budget allows.

With `max_attempts >= 2` (validation permits 1..8), one client request re-issues the full generation to a
second and third replica — none of which can succeed, each facing the same 30s bound — and
`RecordFailure`s a *different* replica each time. PLAN §23's retry list names a dial *connection*
timeout, not a response *header* timeout.

Stock PLAN §76 defaults satisfy every precondition outright.

## X7 — HIGH — Every terminal upstream non-2xx returns two concatenated JSON error objects

`internal/proxy/proxy.go:474-511`, `internal/backend/backend.go:60` — **executed (orchestrator).**

The terminal error block runs two switches with no `return` or `else` between them:

```go
if lastErr != nil {
    var up *backend.Upstream
    if errors.As(lastErr, &up) {
        switch { case up.Status == 429: fail(429,…); case up.Status >= 500: fail(502,…); default: fail(400,…) }
    }          // ← no return
    switch {   // ← also runs for *Upstream errors
    case errors.Is(lastErr, backend.ErrQueueFull): …
    default: fail(502, "api_error", "upstream_unavailable", …)   // ← second write
    }
}
```

`backend.Upstream` is `type Upstream struct{ Status int }` with no `Unwrap`, so no `errors.Is` case in the
second switch can match and it always falls to `default`. `fail` → `writeError` → `WriteHeader` + `Write`,
run twice:

```
upstream 400 → client 400, body = {"error":{…upstream_rejected…}}{"error":{…upstream_unavailable…}}
upstream 429 → client 429, body = two objects
upstream 503 → client 502, body = two objects
```

1. The body is **not valid JSON**. Every OpenAI-compatible SDK parses the error body and will raise a
   decode error instead of surfacing the real status. Violates PLAN §72.
2. `out.status`/`out.class` are overwritten by the second `fail`, so the structured request log *and* the
   accounting record store 502/`backend_5xx` for what was really a 400 or 429 (PLAN §41, §43).
3. `net/http` logs a "superfluous response.WriteHeader call" per occurrence, so a client sending requests
   a backend 4xxs drives unbounded stderr noise.

## X8 — MEDIUM-HIGH — Per-key limits fail open; one key can starve every other key

`internal/limiter/limiter.go:80-86`, `:151-152`, `internal/auth/store.go:85-113`,
`cmd/mellomting/main.go:307-317` — **executed** in round-4 verification; reached independently by three
round-1 reviewers.

`mellomting key create` emits a users file with no `limits:` block and exposes no flags to set one. Zero
means unlimited throughout: `ConcurrentRequests: 0` → `NewConcurrency(0)` returns a nil channel
(unbounded); `RequestsPerSecond: 0` → nil bucket (always allow). `validateUsers` range-checks
id/name/secret_hash/models and never looks at `limits`, and config validation cannot see the users file
at all.

Reproduced with the real CLI and the real ingress stack: one no-limits key took 200/200 concurrency slots
and 100000/100000 rate-limiter allowances; end-to-end, a single no-limits key filled all 8 global inflight
slots and a **different** key received `503 server_overloaded`. The control — the same key with
`concurrent_requests: 2` — bounded the abuser at 2 and admitted the bystander.

PLAN §35 is a prohibition, not a suggestion: "Do not allow a single key to occupy the entire process."

**Severity corrected down from HIGH** by the verifier, and the correction is honoured:
`server.max_inflight_requests` (64) *is* enforced before authentication, so the blast radius is starving
every other key up to 64 concurrent slots, not unbounded resource exhaustion. Stock defaults satisfy the
preconditions completely, and this is the mechanism that makes X6's required concurrency free.

The related sub-claim that hand-edited negative values (`concurrent_requests: -1`,
`requests_per_second: -4`) produce unbounded limiters **reproduces**, but the verifier correctly notes it
carries *zero marginal severity* — the behaviour is identical to omitting the field. It belongs in the
report as a one-line hardening note, not as a separate finding. Worth fixing anyway because the comment
at `limiter.go:163` claims "validated at the key-store boundary; fail closed" and neither half is true.

## X9 — HIGH — The non-streaming write has no deadline; a slow reader denies service to everyone

`internal/proxy/proxy.go:464-465`, `cmd/mellomting/serve.go:340-348` — **executed** in round-4
verification, at stock defaults.

The only `SetWriteDeadline` in the tree is `proxy.go:691`, inside the **streaming** pump
(`stream_write_timeout`, 30s). Streaming is protected; non-streaming is not, and
`http.Server.WriteTimeout` is deliberately unset so as not to truncate long streams.

The decisive run used stock `max_inflight_requests: 64` and a key created by the documented
`mt key create`. 64 paced requests of ~800 KB, whose clients read only the headers and then stopped
reading, accumulated in **0.3 s**:

```
chat attempt1/2/3 : 503
v1/models         : 503
late (t≈33s)      : 503
healthz / readyz  : 200      ← the load balancer still sees the instance as healthy
all 64 blocked > 30s ; recovered: 200 on release
```

A corroborating run at `max_inflight: 4` logged `duration_ms=45089` against a 45 s hold — **no
server-side timeout intervened at any point.**

**Minimum preconditions**, established precisely: (1) a key from the documented CLI — it emits no
`limits:` block, and `NewConcurrency(0)` returns a nil-channel limiter whose `Acquire` always succeeds,
so per-key concurrency is unbounded (this is X8, and it is what makes the attack single-key); (2)
`stream: false`; (3) a response exceeding socket buffers; (4) a client that stops reading; (5) requests
issued **sequentially** — a burst is absorbed by the §22 backend queue (70 simultaneous produced 60 ×
`error_class=queue_full` and the proxy stayed healthy). Stock PLAN §76 defaults satisfy 1, 2, and 5 with
no operator error.

**Two corrections from verification, both honoured:**

- Not "permanently". Recovery is roughly 2 s after the attacker releases. The accurate statement is that
  the proxy is unavailable for as long as the attacker chooses to hold, with **no server-side timeout
  able to break it**.
- The measured size floor (~768 KB blocks, ~512 KB does not) is a **darwin-loopback artifact** and
  should not be quoted as a threshold. A §76-capped chat completion (~130–200 KB) sits below it *on this
  host*. That caveat runs *in favour of* severity, not against it: the deployment target is Linux, whose
  send buffers autotune upward only as the peer's window opens, so against a zero-window client the
  Linux floor is plausibly tens of KB. `/v1/embeddings` — no output-token cap, `max_response_bytes`
  64 MB — clears the floor on any host; that part is analytic, not measured.

This also composes badly with shutdown (M11): a stalled write burns the full grace period, the second
SIGTERM is swallowed, systemd escalates to SIGKILL, and queued accounting records are discarded.

Fix: mirror `proxy.go:691`'s `ResponseController.SetWriteDeadline` on the non-streaming write.

## X10 — MEDIUM — The unknown-usage quota charge is client-controlled; prefill escapes accounting

`internal/proxy/proxy.go:918-924`, `:1026-1036`, `:985-995` — **executed** in round-4 verification;
reached independently by three round-1 reviewers.

PLAN §39's conservative charge for unknown usage is implemented as `reservation = clientLimit` — the
client's *own* output limit — where §39 calls for a **configured** reservation. Reproduced end-to-end
against a real quota with a backend returning no `usage` block: a 100k-character prompt with
`max_completion_tokens: 1` charged **1 token**, versus **32768** when the client omits the field; and a
model with no `policy.max_output_tokens` charged **0** for everything.

**The verifier corrected the framing, and the correction materially changes the finding:**

- The stock path *over*-charges (32768), it does not under-charge. Producing an undercharge needs either
  a deliberately low client `max_completion_tokens` or a model with no `policy` block.
- The genuine, unavoidable gap is that the reservation is **output-only**, so the ~100k-token **prefill**
  escapes accounting entirely regardless of what the client sends. Unknown-usage settlement ignores input
  tokens completely.
- The `stream_options: {"include_usage": false}` mechanism is **real but not an independent bypass** —
  with `ensure_stream_usage: false` a client that simply omits `stream_options` reaches the same
  unknown-usage path. Its unique effect is a mislabeled `injectedUsage` flag whose only consequence is
  M20's chunk swallowing. Not priced twice here.

The mechanism itself is confirmed exactly as described: `clientRequestedUsage` returns false for an
explicit `include_usage: false`, the caller unconditionally sets `injectedUsage = true`, and
`injectStreamUsage` writes the key only when *absent* — so the client's `false` is preserved upstream
while the proxy believes it injected.

**Aggravating, and separately confirmed (MEDIUM):** `account` settles the reservation into the in-memory
quota but writes `TotalTokens: usage.Total` (= 0) to the JSONL, and `replayLine` skips
`TotalTokens <= 0`. Verified: a conservatively-charged record replays to **no state at all**, while an
exact record replays fine (4096). `Record` does carry `usage_status: "unknown"`, so replay *could*
detect these — but **no field carries the amount actually charged**, so the information was never
written. A client suppressing usage therefore makes every one of its records replay as zero, i.e. quota
reset on demand at restart. Against §40's "SHOULD NOT trivially reset". Fix: add a `charged_tokens`
field and write `total`, not `usage.Total`.

Severity where token quotas are relied on as the abuse control: MEDIUM.


## X11 — MEDIUM — A single backend-reported `total_tokens` voids the key's quota window

`internal/accounting/quota.go:50`, `:67-68`, `internal/accounting/record.go:143`, `:170` — **executed
(reviewer and orchestrator, independently).**

`ParseUsage` accepts arbitrary int64 values from the backend body and computes `Total = Input + Output`
with no bounds check. `Quota.Settle` then adds that to `st.hour`/`st.day`, and — the sharper half —
`Quota.Admit` evaluates `st.hour + reservation > limits.TokensPerHour`, so **the comparison itself
overflows**.

Reproduced from a backend body of
`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":9223372036854775807}}` against a
`tokens_per_hour: 1000` key:

```
ParseUsage                                  -> present=true input=1 output=1 total=9223372036854775807
after settling MaxInt64: Admit(1)           -> ok=true   (want false)
after a SECOND MaxInt64 settle: Admit(1000) -> ok=true   (counter wrapped negative)
```

One such response is enough: `MaxInt64 + 1` wraps negative, so the `> limit` test is false and the key
is admitted without limit for the rest of the window. A second settle wraps the *stored* counter
negative, and the negative value also reaches the JSONL log, where `ReportFile` sums it unguarded.

The tell that this is an oversight rather than a decision: `replayLine` **does** guard
`TotalTokens <= 0`. The codebase already knows backend-reported numbers need validating; the live path
just does not do it.

Rated MEDIUM rather than higher because it requires the backend to emit a bogus value, and PLAN §5.3
puts a *malicious* model server out of scope. But a *buggy* one is squarely in §5.1's robustness
envelope, mellomting supports OpenAI-compatible backends generally rather than vLLM alone, and this is
exactly the unchecked-input-from-a-semi-trusted-source class a proxy of this kind exists to contain.
Fix: clamp on parse, and use a saturating or pre-checked addition in both `Settle` and `Admit`.

## X12 — MEDIUM — A body of many short keys costs 13× its size in peak heap, with no aggregate bound

`internal/proxy/budget.go:9-12`, `internal/proxy/proxy.go:92` — **executed, measured.**

`budget.Acquire` reserves `2 * bodyBytes` and its comment claims a 2× peak. Measured on a body of
exactly the **default** `max_body_bytes` (16 MiB) containing roughly 800,000 short top-level keys:

```
body=16777157  |  4.508s  |  totalAlloc 823.6 MiB (51.5×)  |  peak HeapAlloc 211.9 MiB (13.2× body)
```

The only real bound is `max_inflight_requests` (default 64) → roughly 13.5 GiB peak heap, and per-key
`concurrent_requests` defaults to unbounded (X8). PLAN §12.1 requires aggregate memory to be bounded and
asks for the decode + re-encode peak to be accounted for; neither holds. This is the same root cause as
M16 — the body is read and decoded before anything reserves for it — but measured against a
pathological-shape body rather than a large one, and the 4.5s of CPU per request is its own
availability cost.

## X13 — MEDIUM — A stream whose final event lacks a terminating blank line is silently truncated

`internal/proxy/sse.go:97-101`, `internal/proxy/proxy.go:653` — **executed.**

When a backend ends its stream with `data: {...}` and no trailing blank line, then closes cleanly, the
buffered event is discarded and `pump` maps the `io.EOF` to class `"ok"`. Measured with the same content
under three trailers:

```
trailer="\n\n" -> status=200, settled=17,  client received the final content: true
trailer="\n"    -> status=200, settled=100, client received the final content: false
trailer=""       -> status=200, settled=100, client received the final content: false
```

Generated content is lost, the client sees a clean end of stream with no error, and the usage chunk goes
with it — so accounting falls back to the reservation (100 instead of the real 17). Requires a *clean*
EOF; a connection reset surfaces correctly as `backend_stream_error`.

The same probe found that a lone `\r` is treated as data rather than a line terminator, so a
spec-legal CR-terminated SSE stream relays as **zero events with a 200**. Both are §24 "re-emitted
verbatim" violations, and both are invisible in the logs.


---

## Medium findings

- **M1 — Output cap bypassable three ways** (`proxy.go:901-921`, **executed**). (a) A *negative* limit
  suppresses the cap entirely: `-5 != 0` skips injection and `-5 > cap` is false, so
  `{"max_tokens":-5}` reaches the backend with no limit at all. (b) On `/v1/chat/completions`
  `altCapField` sits in an `else if`, so an over-cap `max_tokens` is never validated when
  `max_completion_tokens` is present; and `intField` rejects the lexemes `1e9`/`1000000.0`/`>int64`,
  reading them as *absent*, so the proxy injects the cap into a *different field name* and forwards the
  original. vLLM resolves `max_completion_tokens or max_tokens` so the cap holds there specifically, but
  the body mellomting emits carries a limit above its own cap. `/v1/completions` (no alt field)
  correctly *overwrites* the same input — the two endpoints differ for identical input. (c)
  `max_completion_tokens: 0` is silently raised to the cap, since 0 is the "client supplied nothing"
  sentinel.
- **M2 — Dot-segment response IDs forwarded unnormalized** (`proxy.go:559-575`, `server.go:288-302`,
  **executed**, three reviewers). Both ID validators permit `.`, and the backend URL is built as
  `&url.URL{Path: …}`, which does not remove dot segments; ingress is a bare `HandlerFunc`, not a
  `ServeMux`, so nothing cleans the path inbound either. Confirmed on the wire:
  `POST /v1/responses/../cancel` reaches the backend verbatim. A normalizing hop (nginx, Envoy —
  `normalize_path` is on by default) collapses these to `/v1/cancel` and `/v1/`, outside the §11.1
  allow-list. Bounded: `/` and `%` are rejected, so root-level `/metrics` is *not* reachable this way.
  Fails closed in multi-backend deployments.
- **M3 — `backends[].stream_idle_timeout` is dead config** (`backend.go:187`, `:495`, two reviewers).
  Parsed, defaulted, validated, exposed — and `Client.StreamIdleTimeout()` has no callers. `pump` uses
  the server-level value. It matters more than a usual dead knob because a stream gets `fctx = ctx` with
  no `request_timeout`, so the idle timeout is the *only* wall-clock bound on a stream.
- **M4 — No pre-auth per-source limiting; auth failures logged unbounded** (`server.go:104-118`, `:267`,
  three reviewers). The global bucket (100 rps default) is consumed *before* `authorize`, and nothing in
  `internal/limiter` has a per-source dimension. One host sending 300 rps with a bogus bearer token
  authenticates zero times yet drains the shared bucket, so every legitimate key 429s before
  authentication. The same flood produces ~100 warn-lines/s, which §33 explicitly forbids
  ("Do not log every invalid token during a flood").
- **M5 — `accounting.enabled: false` silently voids all token quotas** (`serve.go:435-460`,
  `proxy.go:316`, two reviewers). `quota` is constructed only inside `if cfg.Accounting.Enabled`, and
  `Admit` is skipped when nil. Nothing cross-checks the users file. Contrast the project's own posture
  on shelved features, which refuse to start "so neither is ever a silent no-op".
- **M6 — The accounting overflow alert can never fire** (`writer.go:78-79`, **verified by inspection and
  a reviewer's repro**). `w.lastAlert.CompareAndSwap(now, now+30)` passes `now` as the *expected old*
  value, but `lastAlert` starts at 0, so the CAS always fails and `lastAlert` stays 0 forever. 200 drops
  over 200 simulated seconds produced 0 alerts. The queue bound itself is correct; only the alert half of
  invariant 15 is broken — and there is no metrics endpoint (a deliberate §3 non-goal) to notice
  otherwise.
- **M7 — YAML content after `---` bypasses every config strictness layer** (`load.go:50-97`,
  **executed**). A two-document config whose second document sets `mode: any`, an unknown field, and a
  bogus backend reference makes `config check` report "is valid" with exit 0, while `show-effective`
  still prints `mode: loopback-only`.
  `checkShape`, `checkNodes`, and the `KnownFields(true)` decode each call `Decode` exactly once, so only
  the first document is examined and the rest is discarded silently. `cat base.yaml overrides.yaml >
  config.yaml` — or any template emitting a document marker — silently drops the second half, including
  an appended `security:` block, while `config check` reports valid.
- **M8 — No file-permission checks anywhere; `securefile`'s helpers are dead code**
  (`securefile.go:51`, `:64`, three reviewers, **executed**). `WorldWritable` and `LStat` have zero non-test
  callers, and no other mode check exists in product code. Verified live: a 0666 `auth.pepper` and
  `users.yaml` → `healthz 200`; a 0666 TLS private key and backend `api_key_file` → `config check` valid
  and `https healthz 200`.
  **Two corrections from verification, both important for anyone fixing this.** (a) `WorldWritable`
  tests `mode & (0o020|0o002)` — *writable*, not *readable*. Simply wiring up the existing dead helper
  would **not** reject a 0644 pepper or TLS key, which is the likelier exposure; the check needed is
  `&0o007` or `&0o077`. (b) The spec anchor is **§100** ("strict file-permission checks" as a release
  criterion), not §28 — §28 contains no permission language and §26 only says "Recommended permissions:
  0640". The symlinked-parent-directory concern is downgraded to **LOW**: the code description is right,
  but §28 itself says "Do not introduce complicated path traversal logic without tests" and the package
  doc already scopes its guarantee to the final path component.
- **M9 — YAML merge keys in the users file can hide a wildcard ACL from a reviewer**
  (`store.go:73-78`, **executed**). `LoadUsers` applies `KnownFields(true)` but not the `checkNodes`
  walk, so anchors and `<<:` merge keys are accepted in the users file though rejected in the config
  file. Verified: a users file with `&tpl` + `<<: *tpl` produces
  `ZFUHM5WQ  looks-restricted  enabled  -  *` — the merge key silently granted `models: ["*"]` and
  `enabled: true`, while the identical construct in the config file fails with
  `line 6: YAML aliases and anchors are not allowed`. `key list` calls the same `auth.LoadUsers` the
  daemon uses at startup, so this covers the serving path.
  The sharper spec anchor is **§77 verbatim**: "Wildcard model permission should be explicit and easy to
  spot in review." Since the precondition is write access to the users file, the harm is **review
  integrity** — a diff engineered to fool a human reviewer — not privilege escalation.
- **M10 — Revocation does not work the way the CLI implies** (`store.go:225`, `main.go:386-419`,
  `serve.go:131`, **executed**). (a) `key revoke` of the *last* key fails —
  `users file needs at least one key`, exit 1 — so a compromised single-key deployment cannot revoke
  through the CLI. The file is left byte-identical, so §29.1's atomicity does hold. (b) `key disable`
  prints `disabled key 4EKTMV5V` and `key list` shows it disabled, while the running daemon returns
  `200` immediately and three seconds later, logging `key_id=4EKTMV5V status=200 error_class=ok`. The
  only signal registration in the tree is `NotifyContext(…, os.Interrupt, syscall.SIGTERM)`. This is a
  direct **§30 violation** — §30 mandates a SIGHUP reload of key enabled state. (c) SIGHUP *kills* the
  daemon: with dispositions reset to `SIG_DFL`, the process exits `killed by signal 1` with no shutdown
  lines, where SIGTERM on the same binary logs `"shutting down" grace_period=30s` → `"shutdown
  complete"`.
  **Correction to the original composition, honoured here:** this is *not* "revocation is impossible".
  `deploy/mellomting.service` sets `Restart=on-failure` with `RestartSec=2` and no `ExecReload=`, so
  SIGHUP kills the process, systemd restarts it about 2 s later, and the restart *does* pick up the new
  users file. The accurate statement is narrower and still worth fixing: revocation requires a restart;
  `key disable` alone silently does nothing; SIGHUP achieves the effect only by killing the process and
  severing in-flight streams instead of taking the §74 graceful path; and `systemctl reload` fails
  outright. (A re-tester trap worth recording: an initial attempt showed the daemon "surviving" SIGHUP
  because `nohup` sets SIGHUP to `SIG_IGN` and Go preserves an inherited `SIG_IGN`.)
- **M11 — Shutdown gaps** (`serve.go:131-163`). `signal.NotifyContext` stays registered through the drain
  and its goroutine exits after one signal, so a second SIGTERM is both buffered-and-dropped *and* has
  its default terminate disposition suppressed — only SIGKILL escapes. `srv.Close()` does not wait for
  handlers, and `Enqueue` has no closed-state check, so records arriving after `acc.Close()` vanish
  without incrementing `dropped`. `Close()` declares `var err error` and never assigns it, so the
  `if err := d.acc.Close(); err != nil` check is dead and a final-sync ENOSPC is invisible.
- **M12 — `backend_network.mode: any` emits no startup warning** (`validate.go:360-363`, two reviewers,
  **executed**). §16.3 requires an explicit opt-in *and* a startup warning. Verified: `config check` on
  `mode: any` reports "is valid", and `serve` logs only `configuration loaded` / `landlock disabled` /
  `mellomting ready`. All five Warn sites were grepped; none relates to `backend_network`. The opt-in
  half **is** satisfied (the default is `loopback-only`, confirmed via `show-effective`) — only the
  warning is missing, so this is LOW-MEDIUM. It is inconsistent even internally: the analogous §8.2
  plaintext-listener case and the landlock weakenings both do log.
- **M13 — A malformed `base_url` is echoed with its credentials** (`validate.go:511-514`, **executed**).
  Verified: `backends.qwen-a.base_url: "http://admin:hunter2SUPERSECRET@127.0.0.1:18001\x7f" is not a
  valid URL`. The well-formed-userinfo branch at `:521` correctly does not leak — the same credential
  yields only `userinfo is not supported`. **LOW**: it needs a narrow conjunction of a credential inlined
  in `base_url` (which §17 already discourages in favour of `api_key_file`) *and* a URL malformed enough
  to fail `url.Parse`.
- **M14 — DNS resolution happens before the connect timeout applies** (`backend.go:269`). `LookupIPAddr`
  runs before `d.Timeout` is set, and for streams `fctx` has no deadline and the client has no `Timeout`.
  A silent resolver pins a global inflight slot and a per-key slot indefinitely with no 504, and the
  failure is misclassified as `ErrConnect`, poisoning passive health (feeding X6).

Also noted at LOW, after verification trimmed several of them:

- **Duplicate `X-Api-Key` is silently first-wins** while duplicate `Authorization` is correctly rejected
  (`server.go:224-241`). Confirmed over a raw socket: valid-then-garbage → 200, garbage-then-valid → 401,
  two identical valid `Authorization` → 401. But **the claimed harm was not demonstrated**: it needs a
  fronting proxy that reads *last*-wins while mellomting reads first, and first-wins is what most stacks
  do (Go, nginx's `$http_x_api_key`, Envoy), so a front layer would most likely agree. What survives is
  an inconsistent hardening posture, not an authentication split. Two-line fix.
- **`allowFor` is a second, unsynchronised route table** (`server.go:196-214`) emitting
  self-contradictory `Allow` headers: `GET /v1/responses` → 405 with `Allow: GET, POST`, and
  `POST /v1/models` → 405 with `Allow: GET, POST`. Protocol correctness only — the larger claims
  originally attached to this were **refuted**, see the verification section.
- **The client's public `model` string is logged before the ACL check** (`proxy.go:221`, emitted at
  `:160`) with no truncation, so a 16 MiB model name writes 16 MiB to the journal per request.
- **`mellomting key create --models does-not-exist` succeeds** (`main.go:280`, **executed**), producing a
  key whose every request 404s with the deliberately ambiguous `model_not_found_or_not_allowed`. The
  §28 MUST originally cited **does not apply** — §28 covers configuration-file handling, and §29, the
  actual CLI spec, imposes no model validation. The argument that stands is code-internal and stronger:
  `keyState(c)` loads and validates the config and *returns* `cfg`, and `keyCreate` discards it
  (`_, usersPath, pepperPath, exit := keyState(c)`). The model list is loaded, in hand, and ignored. A
  *warning* is the right fix rather than a hard reject, since an ACL may legitimately name a model that
  is about to be added.
- **`affinity.Put` is called without validating the backend-supplied ID** (`proxy.go:468`, `:669`), so a
  backend returning a 900 KiB `id` stores it whole — bounded only by `max_response_bytes` — and evicts a
  legitimate record. Lookups themselves are safe.
- Two **unreachable** nil-map panics (`proxy.go:764`, `:920`) on a `null` body. Verified unreachable
  today: both sit behind `o.needsModel`, and `shallowParse` rejects `null` first. Recorded only because
  there is no `recover()` anywhere and the guard is non-local.
- `MaxResponseHeaderBytes` and `MaxConnsPerHost` are unset; `DisableCompression` is unset against §9.2's
  SHOULD; `internal/config/policy.go` is a dead second implementation of the network policy that has
  already diverged from the live one; `safeUnixListen` unlinks a socket without a liveness check, so a
  second instance silently steals it; and `ErrorLog` is nil, so `net/http` writes plaintext panic stacks
  and TLS handshake errors to stderr outside the JSON log pipeline.

---

## Checked and found sound

Negative results, recorded because they are the expensive half of an audit and worth keeping:

- **Invariant 21 (model-ACL bypass via a backend model name) holds.** Exact-match lookups against the
  public table only, no pass-through fallback, backend names are only ever outputs, no case-folding or
  normalization between decode and lookup, `provider/model` rejected, and unknown vs. not-allowed
  collapse into one indistinguishable 404.
- **Invariant 22 (no backend broadcast for an unknown response ID) holds.** On a miss the code pins
  exactly one backend or 404s; `router.Select` is never consulted for a pinned request. There is no
  fan-out anywhere. (The separate §21.1 ownership break is X1.)
- **Header sanitation is a genuine allow-list**, not a deny-list: a fresh map with two entries, no
  mutation of the caller's `r.Header`. Hop-by-hop headers, `Cookie`, `X-Forwarded-*`, and
  `Connection`-listed names are all unreachable. Header-value injection fails closed with no echo.
- **The backend credential never reaches a client, a log, or an error string** — no `%v` on a request or
  URL anywhere. `config show-effective` prints only file *paths*; the pepper and backend tokens are
  `*_file` references whose bytes never enter `Config`.
- **Key handling is sound**: `crypto/rand` with *both* errors checked, 256-bit secret, delimiter-safe
  base32; the raw key never reaches a log, temp file, or error string; the pepper fails closed at two
  layers (<16 bytes rejected, empty rejected); the only secret comparison is `subtle.ConstantTimeCompare`.
  Bearer parsing is case-insensitive, whitespace-trimmed, rejects empty tokens and duplicate
  `Authorization` headers.
- **Routing and limiter internals are clean**: no div-by-zero (weights clamped in three places), no
  infinite loop, no WRR counter drift or cooldown-shift overflow, empty candidate set yields a clean 503
  with no reachable panic. `limiter.Registry` is keyed by `key.ID` *after* authentication, so its growth
  is bounded by the configured key count and is **not** attacker-influenced. Bucket math is
  monotonic-safe; `Retry-After` is clamped ≥1 and leaks no cross-tenant state.
- **JSONL accounting cannot be forged**: `O_APPEND`, a single consumer goroutine, one `Write` per record
  with the newline in the same syscall, everything through `json.Marshal`. Newline injection via `model`,
  `key_id`, `backend`, `endpoint`, or `request_id` is not possible.
- **Route matching is safe despite being hand-rolled.** `net/http` does no path cleaning, so
  `/v1//chat/completions`, `/v1/chat/completions/`, `/v1/chat/completions/..`, `/V1/Chat/Completions`,
  `;param`, and `%2e%2e%2f…` all 404/405 with nothing forwarded; `CONNECT` is never proxied; every route
  has an explicit method guard; `HEAD` does not alias GET. The inflight token is released on all 15
  exit paths tested, including panics.
- **§8.2/§8.3 listener safety is implemented properly.** `:8080` (empty host) is **rejected**, as are
  `0.0.0.0`, `::`, hostnames, `127.1`, `2130706433`, `0177.0.0.1`, and NAT64 `[64:ff9b::7f00:1]`;
  `[::ffff:127.0.0.1]` is correctly loopback. Unix socket handling does Lstat, refuses symlinks and
  non-sockets, and chmods after bind with no blind unlink.
- **Config strictness is strong** *within a single document*: duplicate YAML keys **are** rejected
  (top-level and nested), unknown fields rejected, every enum rejects unknown values with no permissive
  fallthrough, negative limits rejected across every int and Duration, aliases/anchors/custom tags
  rejected. `allowed-cidrs` is re-validated per connection in the dialer — no DNS-rebinding TOCTOU;
  the full policy matrix including `::ffff:` mapped forms and `169.254.169.254` behaves correctly.
- **No telemetry** (invariant 24): the only outbound calls are the backend `LookupIPAddr` and dialer.
  **Landlock invariant 17 holds** — all four failure paths in `required` mode are fatal, with no
  log-and-continue, and "never a partial policy" holds because all rules are added before
  `landlock_restrict_self`. On darwin, `required` is genuinely fatal rather than a silent no-op.
  **§64 systemd hardening**: every directive PLAN asks for is present and unweakened.
- **CI is in good shape**: actions pinned to full commit SHAs, a least-privilege top-level `permissions:`
  block, staticcheck and govulncheck pinned to explicit versions, a fuzz smoke job, and a cross-compile
  matrix over linux/amd64, linux/arm64, darwin/arm64 with `CGO_ENABLED=0 -trimpath`. No compiled binaries
  are tracked in git.
- **No goroutine leaks, in any of five scenarios** (executed): client disconnect mid-stream ×20; a
  backend stalling forever (aborted at 303 ms against a 300 ms idle bound); a backend closing mid-event
  ×10; cancellation during retry backoff (returned in 23 µs); cancellation while queued for admission
  (22 µs). The +3 residual goroutines were confirmed from the stack dump to be `net/http` `persistConn`
  loops.
- **No file-descriptor leaks**: `resp.Body` is closed on every path including the `ErrTooLarge` early
  return — 300 requests produced an fd delta of **+0**. Undrained bodies do not hurt normal 4xx/5xx
  traffic (20 sequential 200/500/429 responses reused 1 connection each); only genuine `ErrTooLarge`
  churns connections, which is the correct trade.
- **No lock is held across a channel operation or I/O** in `routing`, `accounting.Quota`/`Writer`,
  `limiter`, or `proxy.affinity`.
- **Shutdown is correct in its core mechanics**: `Shutdown(gctx)` plus a `Close()` fallback is a proper
  hard deadline, it does complete within the grace period with a live stream, the signal channel is
  buffered, the listener is correctly closed when the Landlock gate fails (the port is immediately
  re-bindable), and TLS loads before the listener binds. A suspected stale-write-deadline leak into the
  next keep-alive request was **disproved** by execution.
- **Global and per-key concurrency slots do bound streams** — only the per-backend slot leaks (X5).
- **TLS**: verified against go1.26.7 that `MinVersion: TLS12` with a nil `CipherSuites` yields a
  forward-secret-only suite list — RSA-KEX, 3DES, RC4, and CBC_SHA256 are all off by default.

Two hypotheses seeded into this review were **disproved** by the reviewers and are recorded here so they
are not re-raised: an empty listen host (`:8080`) slipping past the loopback check, and yaml.v3 silently
taking last-wins on duplicate keys. Both are handled correctly.

---

## Stability, resource use, and measured behaviour

Round 2 ran the race detector, goroutine-leak probes, allocation benchmarks, and real-binary lifecycle
tests. Several of its most useful results are **negative** and are recorded in *Checked and found sound*.

- **M15 — `accounting.fsync: every` and `never` are impossible to configure; the daemon refuses to
  start** (`validate.go:145-147` vs `:432-440`, **executed**). `applyDefaults` injects
  `fsync_interval = 5s` *unconditionally*, without looking at `fsync`. `validateAccounting` then rejects
  a non-zero interval unless `fsync == "interval"`. So `fsync: every` fails validation citing
  `fsync_interval`, a key the operator never wrote — and setting `fsync_interval: 0s` explicitly does not
  help, because `applyDefaults` treats 0 as "unset" and re-injects 5s. Confirmed with the real CLI across
  five configs: `every` → exit 1; `every` + explicit `0s` → exit 1; `never` → exit 1; `interval` → valid;
  omitted → valid. Consequence: the only reachable durability mode leaks up to 5s of accounting records
  on power loss, and an operator trying to harden that gets a startup failure that misdirects them. It
  fails *closed*, which is why this is MEDIUM rather than higher — but a documented option that cannot be
  selected is a real defect.
- **M16 — The request-byte budget gates nothing, and memory is ~9.5× the body size** (`proxy.go:194-213`,
  `:92`, **executed, measured**). The body is fully read into memory *before* `budget.Acquire`, so the
  budget can only turn an allocation that already happened into a late 503. Measured `TotalAlloc` per
  16 MiB request: **151.99 MiB — 9.50× the body**, because the body is decoded and re-encoded twice
  (`shallowParse`, `prepareOutbound`, `rewriteModel`). With 64 concurrent 16 MiB requests held
  simultaneously: **HeapInuse 3.85 GiB, peak 4.24 GiB**, against a `max_buffered_request_bytes` of
  64 MiB — the raw bodies alone are an uncontestable 1 GiB floor. Worse, the 503 at `proxy.go:208` is
  structurally unreachable on defaults: it can only fire when
  `max_body_bytes > max_buffered_request_bytes / 2`, and the defaults are 16 MiB vs 64 MiB. PLAN §12.1
  asks to reserve *before* reading and to account for the decode+re-encode peak; neither happens. The
  shipped systemd unit has no `MemoryMax=`.
- **M17 — Affinity `Put` costs 187 µs once the table is full, permanently, under one global mutex**
  (`affinity.go:75-94`, **executed, benchmarked**). `evictLocked` does two full map scans to delete
  exactly one entry, while `Put` adds one — so once `max_affinity_entries` (10000 default) is reached the
  table stays pinned at max and every subsequent `Put` pays the scan. Measured **187,060 ns/op full vs
  212.7 ns/op empty — an 880× cliff**, with no parallel speedup (195,327 ns/op under `RunParallel`)
  because of the single mutex. That caps Responses-creates at roughly 5,300/s process-wide, and each
  stream's pump blocks for 187 µs at `response.created` (`proxy.go:669`). With the default
  `affinity_ttl: 2h` the expiry scan almost never reclaims anything.
- **M18 — `MaxIdleConnsPerHost` is unset, so `MaxIdleConns: 16` is inert** (`backend.go:170-175`,
  **executed**). Go's default per-host idle cap is 2. Measured: 5 rounds of 8 concurrent requests against
  `max_concurrency: 8` produced **32 TCP connections for 40 requests** (ideal: 8), with 6 of 8
  connections torn down after every burst. Bursty traffic pays a fresh handshake — and a TLS handshake
  for https backends — on roughly 75–80% of requests.
- **M19 — A draining process unlinks a replacement instance's Unix socket** (`serve.go:96`, **executed**).
  `Shutdown` closes the listener at the *start* of the grace period and Go already unlinks the socket; the
  redundant `defer os.Remove` then fires at process exit, up to 30s later. Verified: a replacement bound
  and healthy, the old process exited, and the socket vanished — leaving the replacement alive and
  permanently unreachable. LOW-to-MEDIUM because the shipped systemd unit stops-then-starts; exposure is
  manual restarts, blue-green, and start-before-stop supervisors. One-line fix.
- **Smaller items** (all executed): `ln.Close()` after `Shutdown` always fails, so every clean shutdown
  logs `WARN listener close: use of closed network connection`; a second SIGTERM does nothing (sent at
  +503 ms, the process still exited at +3.012 s with `grace_period: 3s`) and only SIGKILL escapes, which
  skips the accounting flush; `backend.go:349-355`'s `default:` makes the `<-ctx.Done()` case unreachable
  when the queue is full, so a client disconnect is logged as `queue_full` and misleads capacity
  planning; there is no cap on accepted connections (800 idle connections → 812 daemon fds, `/healthz`
  still 200), bounded only by `idle_timeout` and the fd limit, and the unit sets no `LimitNOFILE=`.
- **The per-event pump overhead is real but not a problem** (`proxy.go:625-645`, **measured**). The
  goroutine + channel + timer per SSE event costs +2.13 µs, +432 B, and +6 allocations per event
  (10,001 events: 44.92 ms / 13.1 MB / 220,046 allocs, versus 23.64 ms / 8.79 MB / 160,030 allocs for a
  stripped variant — 1.90× slower). At 100 streams × 200 events/s that is about 4% of one core. Stated
  plainly so nobody "optimises" it ahead of the real findings. There is **no timer leak** — `Stop()` is
  called on both branches.


### Fuzzing and concurrency-stress results

Round 2 wrote and ran **18 fuzz targets** (45s each, 1.2M–4.9M executions apiece) covering the parsers
PLAN §80 names, plus a 200-goroutine stress harness driving the real `httpapi.Server` + proxy + router
against three `httptest` backends.

**13 of the 18 targets found nothing**, which is stated plainly because it is a real result. Clean:
`shallowParse`, `stringField`, `isUsageOnlyChunk`, `isValidResponseID`, the SSE parser, `rewriteModel`
(as a round-trip property, gated on `shallowParse`), `UsageToQuota`, `replayLine`, `Record`,
`ParseKey`, `ParseHashValue`, `StoreLookup`, and — notably — the **Authorization parser**, at 2.30M
executions, with no panic on the `auths[0][len(scheme)+1:]` slicing and no bypass via the dual-header
branch. No OOM, no unbounded allocation, no hang anywhere. `rewriteModel` provably preserves every
non-`model` field semantically across 1.5M inputs including invalid UTF-8, duplicate keys, and huge
numbers.

**Races found: 0.** Roughly 31,000 requests across five runs — 200 goroutines mixing streaming and
non-streaming across 4 keys and 4 endpoints with mid-stream disconnects and garbage bodies, 8 goroutines
churning `Router.RecordFailure`/`RecordSuccess`/`Select`, 3 flipping backends between ok/500/garbage/
stall/reset, and 16 hammering `Quota.Admit`/`Settle` and `Writer.Enqueue`. The detector reported
nothing. Goroutine counts plateau at a constant `+18` (transport bookkeeping) over repeated batches —
no leak.

That stress run also corroborated X7 at scale: exactly **328** `superfluous response.WriteHeader`
warnings against exactly **328** 502 responses.

**Bounds confirmed correct and fail-closed by probing:** SSE line ≤ 262,144 accepted / 262,145 rejected;
SSE event ≤ 1,048,576 accepted / 1,048,577 rejected; request body at exactly the limit → 200 and +1 →
413, including chunked encoding (where `ContentLength == -1` so only the `LimitReader` bounds it), a
`Content-Length` lying low (clean 400, never over-reads), and lying high (413 before reading); gzip →
415. Deeply nested JSON is bounded — depth 9,999 → 200, 10,000+ → clean 400 `invalid_json`, and a
million nested arrays costs 2 ms and 4.9 MiB with no stack blowup.

The reviewer disclosed that two races and one panic seen during this round were bugs in its **own test
code**, fixed them, and re-ran the targets clean. They are not counted as findings.


---

## PLAN §4 security-invariant conformance

All 24 invariants, checked individually. "executed" means a test was run against the invariant, not that
the surrounding code was merely read.

| # | Invariant | Verdict | Evidence | How checked |
|---|---|---|---|---|
| 1 | Anonymous inference disabled by default | HOLDS | `httpapi/server.go:120-123` | executed |
| 2 | No network-reachable admin interface | HOLDS | `httpapi/server.go:83-192` | executed |
| 3 | Client API keys never stored plaintext | HOLDS | `auth/key.go:100`; `main.go:311` | executed |
| 4 | Client API keys never logged | HOLDS | `httpapi/server.go:267`; `proxy.go:155-168` | read |
| 5 | Prompts/responses never persisted by default | HOLDS | `accounting/record.go:38-54` | executed |
| 6 | Backend credentials never returned to clients | HOLDS | `proxy.go:459-465`; `backend.go:422-423` | executed |
| 7 | Backend URLs never controlled by client input | HOLDS | `backend.go:379-383`; `validate.go:512-535` | read |
| 8 | Only allow-listed inference paths reachable | **PARTIAL** | `proxy.go:779`; `server.go:297` | executed |
| 9 | No generic catch-all reverse-proxy route | HOLDS | `httpapi/server.go:191` | executed |
| 10 | Request bodies bounded | HOLDS | `proxy.go:841-853`; `validate.go:294-305` | executed |
| 11 | Response event sizes bounded | HOLDS | `sse.go:80-83`, `:124-126` | executed |
| 12 | Concurrent requests bounded | HOLDS | `httpapi/server.go:66`, `:105-111` | executed |
| 13 | Backend queues bounded | **BROKEN** (streaming) | `backend.go:372-377`, `:428-430` | executed |
| 14 | Retry counts bounded | HOLDS | `proxy.go:333-349`; `validate.go:479-482` | executed |
| 15 | Accounting queues bounded | HOLDS | `accounting/writer.go:72-86` | executed |
| 16 | No retry after stream bytes emitted | HOLDS | `proxy.go:436-447` | executed |
| 17 | Required Landlock never silently degrades | HOLDS | `serve.go:232-261`; `apply_other.go:12-14` | executed (gate) + cross-compiled |
| 18 | Landlock applied to all runtime threads | NOT-VERIFIABLE-HERE | `apply_linux.go:67` | cross-compiled only |
| 19 | MPTCP disabled where Landlock TCP is relied on | HOLDS | `backend.go:288`; `serve.go:294`, `:323` | read |
| 20 | Reload never widens the sandbox | HOLDS (vacuous — no reload exists) | `serve.go:131` | read |
| 21 | No model-ACL bypass via backend model name | HOLDS | `proxy.go:238` | executed |
| 22 | Unknown response IDs never broadcast | HOLDS | `proxy.go:708-724` | executed |
| 23 | Remote qualifier gets no prompt without config | HOLDS | `serve.go:177-189`; `validate.go:614-617` | executed |
| 24 | No product telemetry | HOLDS | `backend.go:189`, `:283`; `go.mod` | read |

**21 of 24 hold. One is broken (13), one is partial (8), one cannot be settled off a Linux host (18).**

Notes on the three that are not clean:

- **13 — BROKEN for streaming only.** Re-confirmed independently: 5 live streams admitted against
  `max_concurrency: 1` with `Inflight()` reporting 0. Blast radius is precisely one bound: the *global*
  `max_inflight_requests` cap **does** survive streams (verified — peak 2 live streams at
  `max_inflight_requests: 2`). See X5.
- **8 — PARTIAL.** The outbound request carries `/v1/responses/../cancel` to the backend verbatim, so
  mellomting emits a path outside its own allow-list. Whether it *lands* somewhere unintended depends on
  the origin's normalization, which is not verifiable off-host — hence PARTIAL rather than BROKEN.
  See M2.
- **18 — NOT-VERIFIABLE-HERE.** `GOOS=linux go build`/`go vet` are clean and `BestEffort` appears
  nowhere in the tree, but the TSYNC path cannot be exercised on darwin. Note this is the invariant X2
  would prevent from ever being exercised at all.

Invariant 16 ("a streamed request is never retried after response bytes have been emitted") deserves
explicit mention as a *pass*: verified with a mid-stream connection kill at `max_attempts: 3`, which
produced exactly one upstream request. It is structurally safe rather than flag-guarded — `pump` is
terminal and `dispatch` returns unconditionally after it.

Additional defects surfaced during this pass, not reported elsewhere:

- **`server.max_response_bytes` is bypassed for streams** (`backend.go:411-416`). It is applied only on
  the buffered path; a live SSE body has no cumulative cap — only the 1 MiB per-event bound and the idle
  timeout. This is the same gap as the missing total-stream bound.
- **Landlock grants a right for a feature that does not exist**: `ReadFiles: [users_file]`
  (`serve.go:225`) is granted for a SIGHUP reload that is not implemented. Free tightening.
- **`defer os.Remove(d.listenAddr())` runs for TCP listeners too** (`serve.go:96`), unlinking a
  `host:port`-named relative path on shutdown, outside `safeUnixListen`'s symlink guards.

---

## Toolchain gates

| Gate | Result |
| --- | --- |
| `gofmt -l .` | clean |
| `go build ./...` | clean |
| `go vet ./...` | clean |
| `GOOS=linux go build ./...` | clean |
| `GOOS=linux go vet ./...` | clean |
| `go test ./...` | all packages pass |
| `go test -race -count=2 ./...` | all pass, no races detected |
| `staticcheck ./...` | 2 diagnostics on the darwin build (Q1) |
| `GOOS=linux staticcheck ./...` | clean |

Coverage: `tlsconfig` 100%, `routing` 93.8%, `landlock` 93.8%\*, `logging` 90%, `limiter` 89.3%,
`securefile` 88%, `accounting` 81.6%, `config` 79.9%, `proxy` 78.3%, `auth` 74.5%, `httpapi` 69.3%,
`backend` 65.1%, `cmd/mellomting` 8.5%\*, `version` 0%.

\* `landlock` 93.8% is measured on darwin, where only the ~30-line `*_other.go` stubs compile — the real
`apply_linux.go` path has **no** coverage in that figure. `cmd/mellomting` 8.5% is understated because
`serve_test.go` exercises the binary out-of-process.

**Q1 — `staticcheck` is not clean on a darwin developer machine.** AGENTS.md lists it as a gate, but on
darwin it reports `cmd/mellomting/serve.go:259:38 SA4023` because `apply_other.go:12`'s `Apply` never
returns a nil interface. That is *correct, intended* fail-closed behaviour, but it means the stated gate
does not hold where most development happens, and a genuine SA4023 elsewhere would be lost in the noise.
Either scope the gate to `GOOS=linux` in AGENTS.md and CI, or add a targeted `//lint:ignore SA4023` with
the reason.

---

## Downgraded or refuted during adversarial verification

Round 4 re-tested the findings by trying to break them. These were revised; the revised version is what
stands. Recording the corrections matters as much as the findings.

- **Quota reservation is not held across concurrent admissions — LOW, not a security finding.** The
  overshoot is real and reproduced (64 concurrent `Admit` calls with limit 1000 and reservation 1000 all
  pass, settling to 64000 — a 64× overshoot). But PLAN §39 says "**optionally** reserve" and closes with
  "Token quotas are an abuse-control mechanism, **not billing-grade accounting**." `Admit` does apply the
  reservation as check-time headroom and the quota binds correctly on the next request. The spec permits
  exactly this behaviour, so the original MEDIUM was a misreading of §39.
- **`isUsageOnlyChunk` swallowing — LOW, and the scope was over-broad.** `json.RawMessage` does capture
  literal `null` (length 4), and the pump does silently drop such a chunk: a frame carrying
  `"choices":[],"usage":null,"system_fingerprint":…,"x_custom":"IMPORTANT"` reached the client as
  *nothing*. But "ANY chunk with an empty or absent `choices` array" is **wrong** — an *absent* `usage`
  key is not matched. The trigger needs `usage` present (null or object) **and** empty `choices`.
  `data: [DONE]`, `finish_reason` frames, content deltas, and error-shaped chunks all survive. What is
  lost is provider metadata on empty-choices frames, against §24's "re-emitted verbatim". Two-line fix:
  reject `bytes.Equal(chunk.Usage, []byte("null"))`.
- **Accounting `Close()` gaps — LOW each, and they should be split.** Both halves confirmed (8 records
  accepted post-close, 0 reached the file, `dropped` unchanged; `Close()` always returns nil). But the
  reachability is narrow: `srv.Shutdown` *does* wait for handlers, so the normal path is safe; only the
  `srv.Close()` force-path races, and the lost records belong to requests already being aborted.
- **Quota replay head-truncation — LOW, precondition not met on defaults.** The torn-line skip and head
  truncation both reproduce, but truncation requires the log to exceed `replay_max_bytes` = **1 GiB**
  (roughly 3M requests, unrotated), and only the *head* is dropped while the current windows live in the
  tail — which is the stated design rationale, not an oversight. The torn-line skip loses exactly one
  record. (The *second* half of that original claim — conservatively-charged records replaying as zero —
  is the substantive one and is folded into X10.)
- **Accounting overflow alert — LOW-MEDIUM (observability), not higher.** Confirmed dead (998 drops, 0
  alert lines, `lastAlert = 0`), and `Dropped()` has no production caller, so §42's configured
  `drop-and-alert` degrades to drop-and-be-silent. But it needs a disk stall to bite, and the queue bound
  itself is correct.

- **The `allowFor` "path-existence oracle" — largely REFUTED.** Two of its three assertions do not hold.
  "Every path under `/v1/responses/` returns 405" is **false**: `GET /v1/responses/garbage` returns
  **200**, because `responsesID` accepts any safe segment and dispatches to `ResponsesRetrieve`; only
  wrong-method requests reach `allowFor` at all. The §11.3 reading was also wrong — §11.3 mandates 404
  for *unimplemented* paths and forbids arbitrary forwarding, whereas `/v1/responses/{id}` **is**
  implemented (§11.1, §21), so 405-on-wrong-method is ordinary RFC 9110. The requirement §11.3 actually
  states was separately confirmed to hold: `GET /metrics` returns **404** and is not forwarded. The
  "oracle" further requires a valid key (both probes 401 unauthenticated) and reveals only public OpenAI
  API shape. What survives is the LOW protocol-correctness note above.
- **`securefile` remediation advice corrected.** Wiring up the existing `WorldWritable` helper would
  *not* fix the exposure — it tests the write bits, not the read bits. See M8.
- **"Revocation is impossible" — corrected.** The shipped systemd unit's `Restart=on-failure` means a
  SIGHUP-killed daemon restarts in about 2 s and *does* pick up the new users file. See M10.
- **The §28 attribution on `key create --models` — REFUTED.** §28 governs configuration-file handling,
  not the CLI. See the LOW note above for the code-internal argument that replaces it.

Two hypotheses seeded into this review were **disproved** and must not be re-raised: an empty listen host
(`:8080`) slipping past the loopback check — it is rejected at `validate.go:272` — and yaml.v3 silently
taking last-wins on duplicate keys — it rejects them, at top level and nested.

---

## Test-suite integrity

The suite passes, `-race` is clean, and coverage looks respectable. Several of those signals are
misleading. Findings below were computed by decomposing per-package coverprofiles into zero-count blocks
and intersecting them with rejection-site line numbers, not by reading tests.

**Tests that do not test what they claim:**

- `internal/backend/backend_test.go:70`, `:79` assert that `X-Api-Key` and `Authorization` are not
  forwarded — but the fixture's `Headers` map at `:63` contains only `User-Agent`, so the assertion is
  vacuous. `Forward` filters nothing (`backend.go:396-400` is a bare `Header.Add` loop) and will happily
  forward `Authorization`, `Proxy-Authorization`, and `X-Forwarded-For`. The *real* allow-list lives at
  `proxy.go:855-866` and **both of its arms are at coverage count 0**. Two PLAN §81 requirements rest on
  this pair of tests. (The allow-list itself is correct — see *Checked and found sound* — it is simply
  untested at the layer the tests claim to cover.)
- `internal/proxy/proxy_test.go:334` `TestUpstreamErrorsAreSanitized` asserts only an error `Code` and a
  `Contains`, which is why the double-envelope bug (X7) survived. **No test in the repository ever
  JSON-decodes a proxy error body.**
- `internal/proxy/proxy_test.go:384` tests cross-key response access using an *unknown* ID, and its
  comment describes "multiple possible backends" that the one-backend fixture cannot create — so PLAN
  §81's "response-ID access cannot cross API keys" is both untested and false (X1).
- `internal/httpapi/server_test.go:180` — the `{"query param ignored", …, 200}` case sets a valid bearer
  at `:202`, making it byte-identical to the `"bearer ok"` case; the query parameter is present on every
  case's URL. It cannot fail. The same table lacks disabled→401, expired→401, and 3 of 4
  malformed-bearer branches.
- `internal/backend/backend_test.go:181` does `errors.Is(err, ErrTimeout)` on an `ErrHeaderTimeout` —
  two bare sentinels with no wrapping, so it is a tautology.
- `internal/accounting/accounting_test.go:234` asserts `w.Dropped() == n` where both sides are
  incremented by the same loop — it tests `sync/atomic`, not mellomting.
- `internal/proxy/proxy_test.go:414` `TestHugeSSEEventBounded` feeds a single 1 MiB *line*, which trips
  the 256 KiB per-line bound first. `ErrSSEEventTooLarge` is at count 0 **repo-wide**: `maxSSEEvent` has
  no coverage at all. A correct fixture (many short `data:` lines totalling > 1 MiB) passes immediately.
- `internal/proxy/retry_test.go:165` `TestStreamFailureIsNeverRetried` writes a complete event then
  closes cleanly, and `dispatch` returns before the retry loop once `stream == true` — so the assertion
  holds for a healthy stream and cannot fail. The stream-abort path and idle timeout are count 0.
  (The invariant it names does hold — verified separately — but not because of this test.)

**Coverage gaps at fail-closed boundaries:**

- `internal/config/validate.go` has **98 rejection sites; 57 are uncovered.** Uncovered: all 11 server
  bounds except `max_body_bytes`, all 4 TLS, both auth `required`, all 3 backend-network, both logging,
  all 5 accounting, all 7 limits/shutdown/responses/retry, all 10 backend (including both `base_url`
  branches, which §80 names as a fuzz target), 5 models, 9 qualifier.
- `internal/auth/store.go:84-112` `validateUsers` has **zero** rejection tests — version, empty key list,
  bad id, missing name, bad `secret_hash`, empty models. `LoadPepper` including "pepper too short" is at
  0.0%. This is the fail-closed boundary of the key store.
- The Responses API, `/v1/completions`, and `/v1/embeddings` are **never routed through `httpapi`** by
  any test; `responsesID`/`isSafeSegment` are at 0.0%. `proxy_test.go:180-201` hand-rolls a *third* route
  table, which is precisely why the `route`/`allowFor` disagreement is structurally invisible to the
  suite. 2 of the 4 PLAN §11.1 MVP endpoints ship untested at every layer.
- Also at count 0: `MaxResponseBytes`/`ErrTooLarge`, `MaxHeaderBytes` (no test anywhere), the byte
  budget, and affinity eviction.

**PLAN §80–§87 required tests:**

| Section | Required | Present |
| --- | --- | --- |
| §80 fuzz targets | 13 applicable parsers | **1** (`config`, 15s in CI, no seed corpus) |
| §81 HTTP/security integration | 30 | 17 present, 5 partial, 8 missing or unsound |
| §82 Landlock integration | 16 | 4, 1 partial — and see X2: the key one never executes |
| §83 MPTCP regression | source + integration | source grep only |
| §84 fake-backend harness | 16 | 8 |
| §85/§86 real vLLM, agent compat | — | absent (deferred by design) |
| §87 benchmarks | 9 | **0** — `grep "func Benchmark"` returns nothing |

Two §81 gaps deserve naming: there is no test for the slow-client write bound, and **all four
log-scrubbing requirements are untested** — no test anywhere captures log output and asserts a secret's
absence, because every test uses `discardLogger()`. For a project whose central claim is "never log keys
or prompts", that is the gap I would close first. §82's root cause is that `landlock.Policy.ReadFiles`,
populated at `serve.go:225`, is never exercised by any test.

---

## Code quality

Ranked by how much each would bite. Style opinions are deliberately excluded.

- **`_ = cancel` discards the `/cancel` suffix** (`httpapi/server.go:284`). `responsesID` computes
  whether the path ended in `/cancel` and then throws the result away, so
  `GET /v1/responses/{id}/cancel` routes to **retrieve** rather than 405 — while `allowFor:206-211`
  believes that path is POST-only. The `_ =` is what hides it from staticcheck.
- **`localhost` is loopback in one file and not in the other** (`config/validate.go:711` vs
  `cmd/mellomting/serve.go:485`). `listen: localhost:8080` without TLS is *refused* until the operator
  sets `allow_plaintext_non_loopback: true` — at which point `serve.go:98` suppresses the very warning
  that flag exists to emit. §8.2's MUST-warn never fires for `localhost`.
- **The backend network policy is implemented three times** (`config/policy.go:9`, `backend.go:79`,
  `serve.go:351`), and the two policy implementations **already differ**: `backend.go:83-86` retries via
  `ip.To4()` and `policy.go:20-26` does not. Worse, `netip.ParsePrefix` silently drops a malformed CIDR
  (`policy.go:16`, `validate.go:502`) where `net.ParseCIDR` fails closed (`validate.go:369`,
  `serve.go:355`). The dead implementation is the one with tests.
- **`make check` cannot fail on formatting** (`Makefile:41-47`). `gofmt -l .` exits 0 while listing
  files, so the gate AGENTS.md tells contributors to run does not gate. `check` also omits `staticcheck`
  and `govulncheck` despite claiming to be the "full quality gate (AGENTS.md)". CI gets this right.
- **CI never checks the non-Linux build.** Every job is `ubuntu-latest`, so `landlock_other.go` and
  `apply_other.go` are never vetted or staticchecked anywhere, and `TestApplyNonLinuxFailsClosed` takes
  its Linux branch in CI — its fail-closed assertion never runs automated. `darwin/arm64` is only
  compiled to `/dev/null`. One line fixes it: `GOOS=darwin go vet ./...`.
- **`allowFor` advertises methods `route` rejects** (`httpapi/server.go:198-211`): `POST /v1/models` →
  405 with `Allow: GET, POST`; the `/healthz` 405 arm is unreachable because health precedes auth.
- **Two byte-identical error writers with different contracts** (`httpapi/errors.go:32` vs
  `proxy.go:1058`) — only the `httpapi` one sets `Retry-After`, so the proxy's 429s (quota at
  `proxy.go:319`, upstream at `:481`) ship none.
- **Comments that misstate security properties.** In a codebase whose pitch is auditability these are
  defects, not nits: `proxy.go:559` "can never inject a path" (`..` passes); `affinity.go:12` "a
  different key cannot retrieve or cancel it even if it learns the ID" (X1 disproves it);
  `limiter.go:163` "validated at the key-store boundary; fail closed" (neither half true);
  `backend.go:320-322` "Body is live only for 2xx stream responses" (X4); `backend.go:447-448`
  misdescribes the timeout classification; the `routing` package doc calls `least-inflight`
  deterministic.
- **`case err == auth.ErrDisabled`** uses `==` rather than `errors.Is` (`httpapi/server.go:262,264`), so
  one wrap upstream and every disabled or expired key silently logs as `auth_unknown`. Both arms are at
  count 0.
- **Five more double-maintained tables** that agree today and have nothing keeping them in step:
  scheme→port (`backend.go:240` / `landlock/policy.go:65`), `MaxABI = 9` in three places, `MinABI = 8` in
  two, the strategy list, and the landlock mode list. `config` does not import `landlock`, so a
  go-landlock bump will not propagate the ABI constants — and a scheme→port drift would make the sandbox
  deny the port the dialer actually uses.
- **Dead code** `staticcheck` cannot see, because exported identifiers in internal packages are not
  flagged by U1000: `backend.AsUpstream`, `backend.Name`, `backend.UpstreamModel`,
  `accounting.SettledTokens`, `accounting.UsagePartial`, all of `config/policy.go`, `main.go:20`
  `plannedCommands` (an empty map, making `main.go:51-52` unreachable), and all of `securefile`'s
  permission helpers. Plus `accounting.overflow` is parsed, defaulted, and validated while
  `WriterConfig` has no such field — the same class as the dead `stream_idle_timeout`.
- **`sse.go:48-55`'s `if p.have` fast path is unreachable**; it is entered only after a read error, after
  which no caller re-enters. If it ever were reachable it would emit a partial event without its
  terminating blank line.
- **`docs/PLAN.md:2606`** — the reference configuration says `max_inflight_requests: 32` while §9.1
  (`:449`), `:1457`, and `validate.go:20` all say 64. Every other value in that block matches the code,
  so this reads as a typo an operator would copy.

**Dependencies (§88) are clean**: `golang.org/x/sys` is imported directly, go-landlock is pinned to a
tagged `v0.9.0` with a `go.sum` hash, `go mod tidy -diff` is empty, and there are no unjustified
dependencies.


---

## Coverage and limits of this review

- **Landlock was reviewed by inspection only.** The host is darwin; the Linux-only files are not
  compiled by a plain `go build`/`go test` and their tests cannot run here. They were type-checked with
  `GOOS=linux go build/vet` and read against the pinned library source, but **no Landlock behaviour was
  executed or observed**. X2 in particular needs one run on a kernel with Landlock ABI ≥ 8. The PLAN §82
  Landlock integration tests need a Linux host.
- **No real vLLM was involved.** All backend behaviour was exercised against `httptest` fakes, so PLAN
  §85 real-backend compatibility is untested here. Claims about which output-limit field a given vLLM
  version honours (M1b) are reasoned from upstream behaviour, not measured.
- **Every high-severity finding was re-tested adversarially.** X6, X8, X9, and X10 were re-checked by a
  reviewer instructed to refute them and to default to REFUTED when it could not reproduce. X6, X8, and
  X9 were confirmed with corrected scope; X10 was confirmed with corrected framing and a reduced
  severity. Four further claims were downgraded and four refuted outright — all recorded above rather
  than quietly dropped.
- **X2 is the one high-severity finding with no runtime evidence anywhere**, because darwin cannot
  provide any. It rests on a source-level derivation checked against the pinned library, including the
  discriminating question of whether ABI 8's handled-rights set includes the offending bit (it does).
  One `go test ./internal/landlock` run on a kernel with Landlock ABI ≥ 8 settles it either way.
- `docs/HANDOVER_STALL_PATTERN.md` is out of scope: it documents a coding-agent output anomaly, not a
  defect in this codebase, and its own investigation already concluded the repository code is not at
  fault.
