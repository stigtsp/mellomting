# Interactive `top` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `mellomting top` with a btop-quality interactive viewer backed by rolling daemon statistics, refusals by limit, limits beside usage, vLLM metrics (configured servers and agents) and accounting history.

**Architecture:** The daemon gains `internal/stats` (per-second ring buffers, bounded series) fed from the request path's existing end-of-request and refusal points plus a 1 Hz gauge sampler, published on the admin socket as `GET /stats?since=`. `internal/backendmetrics` reads vLLM `/metrics` for configured servers (edge) and local servers (agent, served on demand over the attachment). `top` becomes a client that accumulates that data, reads `usage.jsonl` itself, and renders through a small in-repo TUI layer (`internal/topview`).

**Tech Stack:** Go 1.27 standard library; `golang.org/x/sys/unix` for termios (already a dependency).

**Spec:** `docs/superpowers/specs/2026-11-18-top-visibility-design.md` (read it, including "Decisions after review", which overrides earlier sections where they differ).

## Global Constraints

- No new module dependencies. TUI is in-repo; termios via `golang.org/x/sys/unix`, build-tagged for linux and darwin (`TCGETS`/`TCSETS` vs `TIOCGETA`/`TIOCSETA`).
- Admin data only on `server.admin_socket` (0600). Without it: no stats store, no sampler, no metrics pollers.
- Never record or expose prompts, responses, API keys, backend credentials or raw backend bodies.
- Bounds: 64 series per dimension (key, model, server) plus `(other)`; 300 one-second buckets; `/stats` reply ≤ 3 MiB with `"truncated": true` on cut; `/metrics` body ≤ 1 MiB, 2 s timeout, lines > 4 KiB skipped; agent metrics reply ≤ 16 KiB; usage scan ≤ 512 MiB and ≤ 7 days.
- Request path cost: one `stats.Record` / `stats.Refuse` call per request, a short mutex, no allocation in steady state.
- `GET /inflight` keeps its exact shape. Old configs and users files load unchanged; new keys `servers.<name>.metrics` and `attach.metrics` are optional, default true.
- Edge polls `/metrics` and agent metrics every 5 s; agent caches its reading 2 s; servers without metrics retry every 5 minutes; failures log at debug only.
- AGENTS.md writing rules for comments, docs and commit messages; commit subjects imperative lower case; no attribution trailers.
- Quality gates per task: `gofmt -l .` empty, `go vet ./...` and `GOOS=linux go vet ./...`, `GOOS=linux staticcheck ./...`, `go test -race` on touched packages.

## Review Focus

1. Terminal restore: `top` killed by SIGTERM, Ctrl-C, or a panic in rendering must leave the terminal cooked, cursor visible, main screen. (Test in Task 6: `TestSessionRestoresOnPanic`.)
2. Hostile text: a user agent or model name with ESC, C1 controls, CJK wide runes or combining marks must not break alignment or inject escapes. (Task 6: `TestSanitizeAndWidth`.)
3. Tiny or resized terminal: 79×23 shows the "terminal too small" notice; a resize mid-frame re-lays out without debris. (Task 7: `TestTooSmall`, `TestResizeRelayout`.)
4. Daemon restart under `top`: `seq` goes backwards; `top` must drop its history and refetch with `since=0`, and a lost socket shows a banner and retries. (Task 7: `TestSeqResetRefetches`, `TestDisconnectBanner`.)
5. Agent lying in metrics: negative, NaN-equivalent (huge), or oversized replies are clamped or rejected without affecting routing or counters. (Task 4: `TestEdgeClampsAgentMetrics`.)

---

### Task 1: rolling statistics store (`internal/stats`)

**Files:**
- Create: `internal/stats/stats.go`, `internal/stats/series.go`, `internal/stats/export.go`
- Test: `internal/stats/stats_test.go`

**Interfaces:**
- Produces:
  - `type Refusal string` with constants `GlobalInflight, SourceRate, GlobalRate, Auth, KeyRate, KeyConcurrency, Quota, BackendQueue, Draining` (string values = spec table names).
  - `type Sample struct { KeyID, KeyName, Model, Server string; Status int; ErrorClass string; BytesIn, BytesOut, TokensIn, TokensOut int64; TTFB, Duration time.Duration; Fallback bool }`
  - `type Gauges struct { Keys map[string]KeyGauge; Servers map[string]ServerGauge; GlobalInflight int }` with `KeyGauge{Inflight int; AtLimit bool}`, `ServerGauge{Admitted, Queued int; Running, Waiting int; KVPercent float64; HasMetrics bool}`.
  - `func New(clock func() time.Time) *Store`; `func (s *Store) Record(Sample)`; `func (s *Store) Refuse(r Refusal, keyID, keyName, model, server string)`; `func (s *Store) Sample(Gauges)`; `func (s *Store) Since(seq uint64, maxBytes int) Export`.
  - `type Export struct { Seq uint64; BucketMillis int64; Truncated bool; Global SeriesExport; Series []SeriesExport }`; `SeriesExport{ Dim, ID, Name string; Buckets []BucketExport; Errors map[string]int64; Refusals map[string]int64 }`; `BucketExport` JSON with omitempty: `seq, ok, c4xx, c5xx, refused, bin, bout, tin, tout, ttfb_ms_sum, ttfb_n, dur_ms_sum, hist [12]uint32, inflight, at_limit, admitted, queued, running, waiting, kv`.
  - `func (s *Store) Nil-safe` — all methods are no-ops on a nil `*Store`.

- [ ] **Step 1: Write failing tests** in `stats_test.go` with an injected clock:
  - `TestRecordAndSince`: two samples in second 0, one in second 1 → `Since(0)` has 2 buckets, global `ok` 2 then 1; `Since(seq of first)` returns only the second.
  - `TestRefusalNotCompleted`: `Refuse(KeyConcurrency,...)` increments `refused` in the key series and global, leaves `ok/c4xx/c5xx` at 0; `Refusals["key_concurrency"]==1`.
  - `TestTerminalClassification`: `Record` with `ErrorClass` `queue_full`/`queue_timeout` (not Fallback) counts as refusal `backend_queue`; `quota_exceeded` → `quota`; `draining` → `draining`; any other non-2xx → error class counted, not refused. (Use the classes `internal/proxy` already emits; grep `class:` in `proxy.go` for exact strings.)
  - `TestWindowRollsOff`: advance 301 s → old bucket gone; series idle for 300 s removed.
  - `TestSeriesCapFoldsToOther`: 70 distinct keys → 64 key series + `(other)`.
  - `TestGaugesSampled`: `Sample` sets `inflight/at_limit` on key buckets and `admitted/queued/running/waiting/kv` on server buckets for the current second.
  - `TestSinceTruncates`: 200 busy series, `maxBytes` small → `Truncated`, the least busy series have `Buckets==nil` but keep totals.
  - `TestNilStore`: every method on `(*Store)(nil)` is a no-op.
- [ ] **Step 2:** `go test ./internal/stats/` → FAIL (package missing).
- [ ] **Step 3: Implement.** Ring of 300 fixed `bucket` structs per series (uint32 counters, int64 sums), indexed by `unix second % 300` with a stored second to detect staleness. Histogram edges in ms: 100, 250, 500, 1k, 2.5k, 5k, 10k, 30k, 60k, 120k, 300k, +inf. Per series keep window totals for errors/refusals as small maps (global keeps per-bucket class detail only for refusals). `Since` builds JSON-sized estimate per series and drops buckets from least-total-requests series until under `maxBytes`.
- [ ] **Step 4:** `go test -race ./internal/stats/` → PASS.
- [ ] **Step 5:** Benchmark `BenchmarkRecord` shows 0 allocs/op. Commit: `add a bounded rolling statistics store for top`.

### Task 2: feed the store and publish `GET /stats`

**Files:**
- Modify: `internal/httpapi/server.go` (refusal points in `routeBody`), `internal/proxy/proxy.go` (end of `dispatch`; first-byte time), `internal/inflight/inflight.go` (`phase_ms`, `ttfb_ms`), `internal/limiter/limiter.go` (`func (c *Concurrency) InUse() int`, `func (r *Registry) Peek(keyID string) (*KeyState, bool)`), `internal/accounting/quota.go` (`func (q *Quota) Usage(keyID string, now time.Time) (hour, day int64, hourResets, dayResets time.Time)`), `internal/adminapi/adminapi.go`, `cmd/mellomting/serve.go` (construct store and sampler only with admin socket)
- Create: `internal/adminapi/stats.go`, `cmd/mellomting/sampler.go`
- Test: `internal/adminapi/stats_test.go`, `internal/httpapi/stats_test.go`, `internal/inflight/inflight_test.go`, `internal/accounting/quota_test.go`

**Interfaces:**
- Consumes: Task 1 `stats.Store`, `Sample`, `Refusal`, `Gauges`.
- Produces: `GET /stats?since=N` JSON:
  ```
  { "seq", "bucket_ms", "truncated", "now_ms", "started_ms",
    "global": {"max_inflight", "inflight", "rps", "burst", "accounting": {"enabled", "dropped"}},
    "stats": <stats.Export>,
    "keys": [{"id","name","agent","concurrent_requests","requests_per_second","burst","inflight",
              "tokens_per_hour","tokens_per_day","hour_used","day_used","hour_resets_ms","day_resets_ms"}],
    "servers": [{"name","kind":"configured|agent","models":[...],"admitted","queued","max_concurrency","queue_size",
                 "health":[{"backend","model","cooldown_until_ms"}],"metrics": <backendmetrics.Reading|null>}],
    "agents": <[]agent.Record>, "requests": <[]inflight.Record> }
  ```
  `keys` lists every key in the store. Go types `adminapi.Stats`, `adminapi.KeyInfo`, `adminapi.ServerInfo` in `internal/adminapi/stats.go`, shared with `top`.
  - `inflight.Record` adds `PhaseMillis int64 \`json:"phase_ms"\``, `TTFBMillis int64 \`json:"ttfb_ms,omitempty"\``; `(*Request).FirstByte()`.
  - `httpapi.Server` gets `SetStats(*stats.Store)`; `proxy.New` gets the store through an option field `Proxy.SetStats`.

- [ ] **Step 1: Failing tests.**
  - `inflight`: `TestPhaseAndTTFB` — phase change resets `phase_ms`; `FirstByte` sets `ttfb_ms` once.
  - `accounting`: `TestQuotaUsage` — after `Settle(k, 100, t)` `Usage` returns hour 100, day 100, resets at next UTC hour/day.
  - `limiter`: `TestConcurrencyInUse`.
  - `httpapi`: `TestKeyConcurrencyRefusalRecorded` — key limit 1, hold one request, second gets 429 and `Since(0)` shows `key_concurrency` refusal on that key (use the package's existing in-memory server test helpers; no network bind).
  - `adminapi`: `TestStatsShape` — handler with fake sources returns the documented fields; `TestStatsSince` passes `since`; `TestInflightUnchanged` — `/inflight` JSON keys are exactly the old set plus the two new record fields.
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: Implement.** In `routeBody` call `s.stats.Refuse(...)` at each refusal (key name/ID only where authenticated). In `dispatch`'s deferred block call `p.stats.Record` with server from `config.Backend.Server` (attachments: the backend name), tokens = charged total (for untrusted backends scale reported in/out to the charged total), TTFB from the first relayed byte. `cmd/mellomting/sampler.go`: `runSampler(ctx, store, sources)` ticking 1 s, building `stats.Gauges` from limiter `Peek`/`InUse`, backend clients `Admitted/Queued`, agent snapshot and the latest metrics readings.
- [ ] **Step 4:** tests PASS with `-race`; run `scratchpad/skipbind.sh ./internal/httpapi/ -race` style if tests bind.
- [ ] **Step 5:** Commit: `record throughput and refusals, and serve them on the admin socket`.

### Task 3: vLLM metrics reader and configured-server poller (`internal/backendmetrics`)

**Files:**
- Create: `internal/backendmetrics/parse.go`, `internal/backendmetrics/poller.go`
- Modify: `internal/config/source.go`, `internal/config/config.go` (`sourceServer.Metrics *bool \`yaml:"metrics,omitempty"\``, carried to `config.Backend.Metrics bool`, default true), `cmd/mellomting/serve.go`
- Test: `internal/backendmetrics/parse_test.go`, `internal/backendmetrics/fuzz_test.go`, `internal/backendmetrics/poller_test.go`, `internal/config/source_test.go`

**Interfaces:**
- Produces:
  - Engines: vLLM, SGLang and Ollama (the operator's three). `type Reading struct { Engine string; Running, Waiting float64; HasQueue bool; KVPercent float64; PromptTokens, GenerationTokens, Preemptions float64; Loaded []LoadedModel; At time.Time }` JSON `engine, running, waiting, has_queue, kv_percent, prompt_tokens_total, generation_tokens_total, preemptions_total, loaded, at_ms`; `LoadedModel{Name string; VRAMBytes int64; ExpiresAt time.Time}` (at most 16).
  - Prometheus names: vLLM `vllm:num_requests_running`, `vllm:num_requests_waiting`, `vllm:kv_cache_usage_perc` (fallback `vllm:gpu_cache_usage_perc`), `vllm:prompt_tokens_total`, `vllm:generation_tokens_total`, `vllm:num_preemptions_total`; SGLang `sglang:num_running_reqs`, `sglang:num_queue_reqs`, `sglang:token_usage`, `sglang:prompt_tokens_total`, `sglang:generation_tokens_total`. Usage fractions 0..1 become percent.
  - Ollama has no `/metrics`: when `/metrics` is 404, the poller tries `GET <base>/api/ps` (JSON, 256 KiB cap, strict) and fills `Loaded` with `HasQueue=false`. `func ParseOllamaPS(r io.Reader) (Reading, error)`.
  - `func Parse(r io.Reader) (Reading, bool, error)` — `bool` false when no allow-listed name appears.
  - `type Poller` with `func NewPoller(servers map[string]Target, log *slog.Logger) *Poller`, `Target{BaseURL string; Client *http.Client; APIKey string}`, `func (p *Poller) Run(ctx context.Context)`, `func (p *Poller) Latest() map[string]Reading`.
- [ ] **Step 1: Failing tests.** `TestParseVLLM` and `TestParseSGLang` on fixtures of each engine's output; `TestParseOllamaPS` (two loaded models, VRAM, expiry; 17 models → 16); `TestPollerFallsBackToOllama` (`/metrics` 404, `/api/ps` 200 → Ollama reading); with two `model_name` label sets → sums; `kv_cache_usage_perc` preferred over `gpu_cache_usage_perc` (0..1 scaled to percent); `TestParseIgnoresOthers` (Mellomting 404 body / Ollama JSON → `ok=false`); `TestParseBounds` (1 MiB + 1 → error; 5 KiB line skipped; `NaN`, `+Inf`, negative dropped); `FuzzParse` arbitrary bytes never panic; `TestPollerBackoff` with an `httptest`-free `RoundTripper` stub: 404 → next attempt after 5 min, 200 → every 5 s; `TestMetricsConfigDefault` (absent → true, `false` honoured, show-effective omits when true).
- [ ] **Step 2:** FAIL.
- [ ] **Step 3: Implement.** Line scanner with `bufio.Reader` and explicit 4 KiB limit; match name prefix before label parsing. Poller per server with jitter ±10 %, uses the backend's configured transport policy and credential, no redirects, `io.LimitReader(1<<20+1)`. Started in `serve` only when `admin_socket` is set, for servers with `metrics` true.
- [ ] **Step 4:** PASS; `go test -fuzz FuzzParse -fuzztime 20s ./internal/backendmetrics/` clean.
- [ ] **Step 5:** Commit: `read vllm metrics from configured servers when top can show them`.

### Task 4: agent metrics on demand

**Files:**
- Modify: `internal/config/attach.go` (`Metrics *bool \`yaml:"metrics,omitempty"\``; `func (a *Attach) MetricsEnabled() bool`), `internal/agent/client.go` (route `/mellomting/backend-metrics` before `relayable`; 2 s cache; `Options.Metrics bool`), `internal/agent/edge.go` (per-attachment poller using the raw edge transport, not admission or `countingTransport`; `Record.Metrics *backendmetrics.Reading`), `cmd/mellomting/attach.go`
- Test: `internal/agent/metrics_test.go`, `internal/config/attach_test.go`

**Interfaces:**
- Consumes: Task 3 `Parse`, `Reading`.
- Produces: `agent.Record.Metrics *backendmetrics.Reading \`json:"metrics,omitempty"\``; `func (g *Registry) PollMetrics(ctx context.Context)` started by serve only with admin socket.
- [ ] **Step 1: Failing tests.** `TestAgentServesMetricsOnDemand` (handler with stub local transport: two GETs within 2 s hit the server once; `Metrics:false` → 404; path is never relayed); `TestEdgeClampsAgentMetrics` (reply > 16 KiB rejected; negative → absent; values > 1e12 clamped; unknown fields rejected by strict decode); `TestMetricsPollNotCounted` (poll does not change `Requests`/`Failures`/admission); `TestAttachMetricsKey` (absent → enabled; old attach configs parse).
- [ ] **Step 2:** FAIL.
- [ ] **Step 3: Implement** as specified; old agents' 404 → `Metrics=nil`, retry every 5 min.
- [ ] **Step 4:** PASS (`skipbind.sh ./internal/agent/ -race`).
- [ ] **Step 5:** Commit: `let the edge read an agent's backend metrics, which its operator can refuse`.

### Task 5: usage history reader (`internal/usagehist`)

**Files:**
- Create: `internal/usagehist/usagehist.go`
- Test: `internal/usagehist/usagehist_test.go`

**Interfaces:**
- Consumes: `accounting.Record` JSON shape; reuse the bounded line scanner (export `accounting.ScanLines(r io.Reader, maxLine int, fn func([]byte)) error` from `scanBounded` if needed).
- Produces: `type History`; `func Open(path string, now time.Time) (*History, error)`; `func (h *History) Refresh(now time.Time) error` (tail; handles shrink/inode change); `func (h *History) Totals(dim Dim, window time.Duration, now time.Time) []Row`; `func (h *History) Hourly(dim Dim, id string, now time.Time) [168]Row`; `func (h *History) Covers() (from, to time.Time)`; `type Dim int` (`ByKey, ByModel, ByServer`); `Row{ID string; Requests, In, Out, Cached, Charged, Unknown int64}`.
- [ ] **Step 1: Failing tests** (temp dir files): `TestReadsRotatedGzip` (`usage.jsonl`, `.1`, `.2.gz` → 3 days covered); `TestSevenDayBound` (older records ignored); `TestTailAppendsAndRotation` (append → Refresh adds; truncate (copytruncate) → rescans without double count); `TestFoldsAgentAttachments` (`agent-lab-3`, `agent-lab-7` → `agent-lab`); `TestByteBound` (scan stops at budget, `Covers` reflects the cut); `TestCorruptLinesSkipped`.
- [ ] **Step 2:** FAIL. **Step 3:** implement (hour buckets keyed by UTC hour, map per dim; server = backend name mapped by `top` later, fold rule applied here). **Step 4:** PASS. **Step 5:** Commit: `read accounting history for top`.

### Task 6: terminal layer (`internal/topview/term`)

**Files:**
- Create: `internal/topview/term/term.go`, `term_linux.go`, `term_darwin.go`, `term_other.go`, `input.go`, `width.go`, `screen.go`, `draw.go`
- Test: `internal/topview/term/*_test.go`

**Interfaces:**
- Produces:
  - `type Session`; `func Start(in, out *os.File) (*Session, error)` (raw mode, alt screen `CSI ?1049h`, hide cursor, `CSI ?2026` sync when drawing); `func (s *Session) Restore()` (idempotent); `func (s *Session) Size() (w, h int)`.
  - `type Key struct { Code KeyCode; Rune rune }`, codes `Up, Down, Left, Right, PgUp, PgDn, Home, End, Enter, Esc, Tab, Backspace, Rune, CtrlC`; `func Decode(buf []byte) (keys []Key, rest []byte)` handling CSI and SS3 forms, lone ESC after 50 ms (caller's timer).
  - `func Width(s string) int`; `func Truncate(s string, cells int) string`; `func Sanitize(s string) string` (drop C0/C1/DEL, ESC sequences).
  - `type Style struct{ FG, BG Color; Bold, Dim, Reverse bool }`; `type Color uint8`-based 256 palette; `func Gradient(f float64) Color` (green→yellow→red); `NoColor` mode.
  - `type Screen` cell grid; `func NewScreen(w, h int) *Screen`; `Set/Text/Box(x,y,w,h,title)/Fill`; `func (s *Screen) Diff(prev *Screen, w io.Writer)` writes only changed runs, one buffered write.
  - `func Braille(values []float64, w, h int, max float64) []string` (area graph, 2×4 dots per cell); `func Spark(values []float64, w int) string`; `func Meter(frac float64, w int) (string, Color)`.
- [ ] **Step 1: Failing tests:** `TestDecodeKeys` (`\x1b[A`, `\x1bOA`, `\x1b[5~`, `\x1b[H`, `\x1b[1;5C`, partial sequence kept in `rest`, `\x03`); `TestSanitizeAndWidth` (ESC/C1 stripped; `"漢字"` width 4; combining mark width 0; truncation never splits a wide rune; result fits cells exactly); `TestBraille` (known series → exact rune rows; all-zero → blank; max 0 safe); `TestScreenDiff` (second identical frame writes nothing; one changed cell writes one run); `TestSessionRestoresOnPanic` (use a pty-free fake `termios` interface: `Start` saves state, a panic in a deferred-recover wrapper calls `Restore`, which writes `CSI ?1049l` and `CSI ?25h`).
- [ ] **Step 2:** FAIL. **Step 3:** implement; width table for East Asian Wide/Fullwidth and zero-width ranges (compact range table, no dependency). **Step 4:** PASS on darwin and `GOOS=linux go vet`. **Step 5:** Commit: `add a small terminal layer for top`.

### Task 7: the viewer (`internal/topview`)

**Files:**
- Create: `internal/topview/model.go` (accumulated data from `/stats`, history merge, seq reset), `views.go` (Overview, Keys, Models, Backends, Requests, Usage), `detail.go`, `redact.go`, `app.go` (event loop), `plain.go` (non-TTY / `--once` text)
- Test: `internal/topview/*_test.go`, golden frames in `internal/topview/testdata/*.golden`

**Interfaces:**
- Consumes: Task 2 `adminapi.Stats`, Task 5 `usagehist`, Task 6 `term`.
- Produces: `type Source interface { Stats(ctx context.Context, since uint64) (adminapi.Stats, error) }`; `type Options struct { Redact bool; NoColor bool; Interval time.Duration; Rows int; Usage *usagehist.History }`; `func Run(ctx context.Context, src Source, sess *term.Session, opt Options) error`; `func Frame(m *Model, ui UIState, w, h int) *term.Screen` (pure, for goldens); `func Plain(w io.Writer, st adminapi.Stats, opt Options)`.
- Behaviour pinned by the spec: Overview (view 1) is the dashboard `top` opens on, with the attention box (spec §5) ranked worst first; views 1–6 (Usage is 6), keys `↑↓ j k PgUp PgDn Home End Enter Esc ←→ Tab 1-6 s / r p q Ctrl-C`, selection by item ID, bottom-border hints, header `REDACTED` when on, refusal reason named in the header, "offered" vs completed rate, banner on disconnect with retry, `truncated` shown, 80×24 minimum.
- [ ] **Step 1: Failing tests:** golden frames at 120×40 and 80×24 for each view from a fixed `adminapi.Stats` fixture (include the screenshot scenario: key at 8/8 with `key_concurrency` refusals; agent with 24 admitted and vLLM 6 running/18 waiting); `TestAttentionRanking` (key refusals, vLLM queueing, cooldown, accounting drops → lines ordered by severity; none → `nothing held back`); `TestStartsOnOverview`; `TestSelectionFollowsID` (rows reorder, selected key stays selected); `TestFilterAndSort`; `TestRedactStableLabels` (same IP → same `ip-N` across frames; IPv6; IP inside user agent); `TestTooSmall`; `TestResizeRelayout`; `TestSeqResetRefetches` (stub source returns lower seq → model cleared, next call `since=0`); `TestDisconnectBanner` (source error → banner, keeps last data, next tick retries); `TestPlainNoEscapes` (Plain output contains no `\x1b`).
- [ ] **Step 2:** FAIL. **Step 3:** implement; input handled on its own goroutine so key presses redraw immediately; data poll on a ticker; frames rendered through `Screen.Diff`. Update goldens with `-update` flag only after eyeballing them.
- [ ] **Step 4:** PASS. **Step 5:** Commit: `rebuild top as an interactive viewer`.

### Task 8: wire `top` and finish docs

**Files:**
- Modify: `cmd/mellomting/top.go` (flags `--redact`, `--interval`, `--once`, `--rows`; TTY detection; `/stats` 404 → old-daemon notice + legacy `/inflight` table; open `usagehist` when `accounting.path` set and readable), `cmd/mellomting/help.go`, `docs/PLAN.md` (§43.2, §11.3, §104), `docs/OPERATIONS.md` (top section), `docs/CONFIGURATION.md` (`metrics`, `attach.metrics`), `AGENTS.md` (status paragraph for Phase 3c), `README.md` only if it mentions `top` output
- Test: `cmd/mellomting/top_test.go`, `cmd/mellomting/top_render_test.go`

- Also: `--simulate` runs `top` against `internal/topview/sim` (create `sim.go`): `func New(seed int64) topview.Source`, a deterministic synthetic daemon (no config or socket needed) with several client keys, configured servers and agents, vLLM metrics, a usage history, and a scripted incident loop (a key hitting `concurrent_requests` with growing refusals; an agent's vLLM queueing; a server entering cooldown; accounting drops) so every panel and the attention box can be checked by eye. Works with `--once` and `--redact`.
- [ ] **Step 1: Failing tests:** `TestSimDeterministic` (same seed → same `Stats` sequence; seq increases by one per call; contains at least one refusal, one vLLM reading, one cooldown within 60 simulated seconds); `TestTopSimulateOnce` (`top --simulate --once` exits 0 and prints the summary without needing a config); `TestTopOnceUsesPlain` (non-TTY stdout → no escapes, includes summary line); `TestTopOldDaemonFallback` (stub admin returning 404 on `/stats` → legacy table + notice); `TestTopRedactFlag`.
- [ ] **Step 2:** FAIL. **Step 3:** implement; signal handling: SIGINT/SIGTERM cancel ctx, `defer sess.Restore()` plus recover; SIGWINCH triggers relayout. **Step 4:** full gates (`gofmt`, vet darwin+linux, staticcheck linux, `-race` tests via skipbind). **Step 5:** Commit: `wire the new top and document it`.
