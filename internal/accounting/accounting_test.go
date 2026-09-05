package accounting

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseUsageChat(t *testing.T) {
	body := []byte(`{"id":"x","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`)
	u := ParseUsage(body, "chat.completions")
	if !u.Present {
		t.Fatal("expected present usage")
	}
	if u.Input != 10 || u.Output != 5 || u.Total != 15 || u.Cached != 3 || u.Reasoning != 2 {
		t.Fatalf("unexpected usage: %+v", u)
	}
}

func TestParseUsageResponses(t *testing.T) {
	body := []byte(`{"id":"resp_1","output":[{"type":"message"}],"usage":{"input_tokens":20,"output_tokens":7,"total_tokens":27,"input_tokens_details":{"cached_tokens":9},"output_tokens_details":{"reasoning_tokens":4}}}`)
	u := ParseUsage(body, "responses")
	if !u.Present {
		t.Fatal("expected present usage")
	}
	if u.Input != 20 || u.Output != 7 || u.Total != 27 || u.Cached != 9 || u.Reasoning != 4 {
		t.Fatalf("unexpected usage: %+v", u)
	}
}

func TestParseUsageNestedResponse(t *testing.T) {
	// The Responses streaming final event nests usage under "response".
	body := []byte(`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}`)
	u := ParseUsage(body, "responses")
	if !u.Present || u.Input != 3 || u.Output != 4 || u.Total != 7 {
		t.Fatalf("unexpected usage: %+v", u)
	}
}

func TestParseUsageNone(t *testing.T) {
	u := ParseUsage([]byte(`{"id":"x","choices":[{"message":{"content":"hi"}}]}`), "chat.completions")
	if u.Present {
		t.Fatal("expected no usage")
	}
	if u := ParseUsage([]byte(``), ""); u.Present {
		t.Fatal("expected no usage for empty body")
	}
}

func TestParseUsageZeroIsNotPresent(t *testing.T) {
	u := ParseUsage([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`), "")
	if u.Present {
		t.Fatal("all-zero usage must not count as present")
	}
}

func TestParseStreamChunk(t *testing.T) {
	u := ParseStreamChunk(`{"id":"c","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	if !u.Present || u.Input != 1 || u.Output != 2 {
		t.Fatalf("unexpected usage: %+v", u)
	}
}

func TestQuotaWindows(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	q := &Quota{clock: func() time.Time { return now }}
	lim := WindowLimit{TokensPerHour: 100, TokensPerDay: 1000}

	// Admit with settled 0 and reservation 50 fits.
	if ok, _ := q.Admit("k", lim, 50, now); !ok {
		t.Fatal("expected admit")
	}
	q.Settle("k", 60, now)
	// 60 settled + 50 reservation = 110 > 100 hour limit.
	if ok, reason := q.Admit("k", lim, 50, now); ok {
		t.Fatal("expected reject")
	} else if reason != "tokens_per_hour" {
		t.Fatalf("expected hour reject, got %q", reason)
	}
	// Day window still fits within 60+50.
	if ok, _ := q.Admit("k", WindowLimit{TokensPerDay: 1000}, 50, now); !ok {
		t.Fatal("expected day admit")
	}
}

func TestQuotaHourRollover(t *testing.T) {
	t0 := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	t1 := time.Date(2026, 8, 20, 16, 0, 0, 0, time.UTC)
	q := &Quota{}
	q.Settle("k", 80, t0)
	lim := WindowLimit{TokensPerHour: 100}
	if ok, _ := q.Admit("k", lim, 50, t0); ok {
		t.Fatal("expected reject before rollover")
	}
	// New hour window resets.
	if ok, _ := q.Admit("k", lim, 50, t1); !ok {
		t.Fatal("expected admit after hour rollover")
	}
}

func TestQuotaUnlimited(t *testing.T) {
	q := &Quota{}
	if ok, _ := q.Admit("k", WindowLimit{}, 1<<30, time.Now()); !ok {
		t.Fatal("no limits must always admit")
	}
}

func TestQuotaReplay(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")

	write := func(r Record) {
		b, _ := json.Marshal(r)
		f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
	}
	// A record in the current hour/day.
	write(Record{Time: now, KeyID: "k", TotalTokens: 40})
	// A record in an earlier hour and earlier day — must not count.
	write(Record{Time: now.Add(-2 * time.Hour), KeyID: "k", TotalTokens: 999})
	write(Record{Time: now.Add(-30 * time.Hour), KeyID: "k", TotalTokens: 999})

	q := &Quota{clock: func() time.Time { return now }}
	if err := q.Replay(path, 1<<20); err != nil {
		t.Fatalf("replay: %v", err)
	}
	lim := WindowLimit{TokensPerHour: 50, TokensPerDay: 100}
	if ok, _ := q.Admit("k", lim, 20, now); ok {
		t.Fatal("expected reject: 40 settled in hour")
	}
	if ok, _ := q.Admit("k", WindowLimit{TokensPerHour: 60}, 20, now); !ok {
		t.Fatal("expected admit within remaining hour")
	}
	if ok, _ := q.Admit("k", WindowLimit{TokensPerDay: 50}, 10, now); ok {
		t.Fatal("expected reject: 40 settled in day")
	}
}

// FIX-28: the startup replay path also skips an over-long line rather
// than buffering it without bound, and later records still replay.
func TestQuotaReplaySkipsOverlongLine(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Record{Time: now, KeyID: "k", TotalTokens: 40})
	_, _ = f.Write(append(b, '\n'))
	// A line longer than maxAccountingLine of garbage between two records.
	_, _ = f.Write(make([]byte, maxAccountingLine+4096))
	_, _ = f.WriteString("\n")
	b2, _ := json.Marshal(Record{Time: now, KeyID: "k", TotalTokens: 10})
	_, _ = f.Write(append(b2, '\n'))
	_ = f.Close()

	q := &Quota{clock: func() time.Time { return now }}
	// maxBytes is larger than the whole file, so the full-scan path runs
	// and the over-long line sits between two replays.
	if err := q.Replay(path, 4<<20); err != nil {
		t.Fatalf("replay: %v", err)
	}
	lim := WindowLimit{TokensPerHour: 49}
	if ok, _ := q.Admit("k", lim, 0, now); ok {
		t.Fatal("expected reject: 50 tokens replayed, over-long line skipped")
	}
	if ok, _ := q.Admit("k", WindowLimit{TokensPerHour: 60}, 9, now); !ok {
		t.Fatal("expected admit within the replayed 50-token hour")
	}
}

func TestParseUsageClampsExtremes(t *testing.T) {
	// A single bogus MaxInt64 total_tokens must be clamped, not accepted
	// verbatim, so it can never wrap a quota window negative (T-X11).
	body := []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":9223372036854775807}}`)
	u := ParseUsage(body, "chat.completions")
	if !u.Present {
		t.Fatal("expected usage present")
	}
	if u.Total != maxUsageTokens {
		t.Fatalf("total = %d, want clamped %d", u.Total, maxUsageTokens)
	}
	if u.Input != 1 || u.Output != 1 {
		t.Fatalf("input/output mangled: %+v", u)
	}
	// Negative values are clamped to 0, not subtracted from quota.
	neg := ParseUsage([]byte(`{"usage":{"prompt_tokens":-3,"completion_tokens":-2,"total_tokens":-5}}`), "")
	if !neg.Present {
		t.Fatal("expected usage present")
	}
	if neg.Input != 0 || neg.Output != 0 || neg.Total != 0 {
		t.Fatalf("negatives not clamped: %+v", neg)
	}
}

func TestSatAddNegative(t *testing.T) {
	// FIX-07/N4: a negative addend must never wrap to MaxInt64 via the
	// overflow guard's own arithmetic.
	if got := satAdd(0, -1); got != 0 {
		t.Fatalf("satAdd(0,-1) = %d, want 0", got)
	}
	if got := satAdd(100, -5); got != 95 {
		t.Fatalf("satAdd(100,-5) = %d, want 95", got)
	}
	if got := satAdd(5, -10); got != 0 {
		t.Fatalf("satAdd(5,-10) = %d, want 0 (floored)", got)
	}
	// The positive overflow behaviour is unchanged.
	if got := satAdd(math.MaxInt64, 1); got != math.MaxInt64 {
		t.Fatalf("satAdd(MaxInt64,1) = %d, want MaxInt64", got)
	}
	if got := satAdd(1, math.MaxInt64); got != math.MaxInt64 {
		t.Fatalf("satAdd(1,MaxInt64) = %d, want MaxInt64", got)
	}
	if got := satAdd(math.MaxInt64, -1); got != math.MaxInt64-1 {
		t.Fatalf("satAdd(MaxInt64,-1) = %d, want MaxInt64-1", got)
	}
}

func TestQuotaMaxInt64DoesNotVoidWindow(t *testing.T) {
	now := time.Now()
	u := ParseUsage([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":9223372036854775807}}`), "chat.completions")
	q := &Quota{}
	q.Settle("k", u.Total, now)
	lim := WindowLimit{TokensPerHour: 1000, TokensPerDay: 2000}
	// The key must remain quota-bound: admission with a reservation must
	// reject, never admit because an overflow wrapped the counter.
	if ok, _ := q.Admit("k", lim, 1, now); ok {
		t.Fatal("expected reject: clamped MaxInt64 exceeds window")
	}
	// A second settle saturates rather than wrapping negative.
	q.Settle("k", u.Total, now)
	if ok, _ := q.Admit("k", lim, 1, now); ok {
		t.Fatal("expected reject after second settle: counter must not wrap")
	}
}

func TestQuotaReplayChargedTokens(t *testing.T) {
	// A conservatively-charged unknown-usage record (total_tokens 0 but
	// charged_tokens set) must replay to the same settled state so a
	// restart does not reset the quota (PLAN §39, §40).
	now := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	b, _ := json.Marshal(Record{Time: now, KeyID: "k", TotalTokens: 0, ChargedTokens: 40})
	f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()

	q := &Quota{clock: func() time.Time { return now }}
	if err := q.Replay(path, 1<<20); err != nil {
		t.Fatalf("replay: %v", err)
	}
	// 40 charged settled: an hour limit below 40 must reject.
	if ok, _ := q.Admit("k", WindowLimit{TokensPerHour: 39}, 0, now); ok {
		t.Fatal("expected reject: 40 charged settled exceeds hour limit 39")
	}
	if ok, _ := q.Admit("k", WindowLimit{TokensPerHour: 41}, 0, now); !ok {
		t.Fatal("expected admit within remaining hour")
	}
}

func TestQuotaReplayMissingFile(t *testing.T) {
	q := &Quota{}
	if err := q.Replay(filepath.Join(t.TempDir(), "nope.jsonl"), 1<<20); err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
}

func TestQuotaReplayTail(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	var f *os.File
	f, _ = os.Create(path)
	// Many old records, one current.
	for range 500 {
		b, _ := json.Marshal(Record{Time: now.Add(-5 * time.Hour), KeyID: "old", TotalTokens: 10})
		_, _ = f.Write(append(b, '\n'))
	}
	b, _ := json.Marshal(Record{Time: now, KeyID: "k", TotalTokens: 30})
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()

	q := &Quota{clock: func() time.Time { return now }}
	// Replay only the tail (small budget), so only the final record
	// (and possibly part of the last old record) is seen.
	if err := q.Replay(path, 2000); err != nil {
		t.Fatalf("replay: %v", err)
	}
	lim := WindowLimit{TokensPerHour: 40}
	if ok, _ := q.Admit("k", lim, 20, now); ok {
		t.Fatal("expected reject: current record replayed")
	}
}

func TestWriterEnqueueAndDrain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	w, err := NewWriter(WriterConfig{Path: path, QueueSize: 8, FSync: "every"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i := range 5 {
		w.Enqueue(Record{KeyID: "k", TotalTokens: int64(i + 1), Time: time.Now()})
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f, _ := os.Open(path)
	defer f.Close()
	var lines int
	var total int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("bad line: %v", err)
		}
		lines++
		total += r.TotalTokens
	}
	if lines != 5 || total != 15 {
		t.Fatalf("expected 5 lines/15 tokens, got %d/%d", lines, total)
	}
}

func TestWriterDropAndAlert(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	w, err := NewWriter(WriterConfig{Path: path, QueueSize: 2, FSync: "never"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	// Fill the queue; further Enqueue calls must not block and must drop.
	for range 100 {
		w.Enqueue(Record{KeyID: "k", TotalTokens: 1, Time: time.Now()})
	}
	if w.Dropped() == 0 {
		t.Fatal("expected some dropped records")
	}
	_ = w.Close()
}

// TestOverflowAlertCadence reproduces the review's sim (T-M6): 200
// simulated seconds of drops must produce alerts at the configured
// cadence (not 0). lastAlert starts at 0 and the timestamps are real
// Unix seconds (non-zero), so a CAS that expects `now` as the old value
// would never match and the alert would never fire.
func TestOverflowAlertCadence(t *testing.T) {
	w := &Writer{}
	base := time.Now().Unix()
	var alerts int
	for sec := 0; sec < 200; sec += 5 {
		if w.alertDue(base + int64(sec)) {
			alerts++
		}
	}
	if alerts == 0 {
		t.Fatal("overflow alert never fired (T-M6 regression)")
	}
	want := 200/alertCadence + 1 // alerts at 0, 30, ..., 180
	if alerts != want {
		t.Fatalf("alerts = %d, want %d (cadence %ds)", alerts, want, alertCadence)
	}
	// lastAlert must have advanced past the final alert.
	if w.lastAlert.Load() < base+180+alertCadence {
		t.Fatalf("lastAlert did not advance: %d", w.lastAlert.Load())
	}
}

// TestOverflowAlertFirstDropFires is the minimal T-M6 regression: the
// very first drop (lastAlert == 0) must fire an alert. Before the fix
// the CompareAndSwap compared against `now`, which never equals the
// initial 0, so the alert could never fire.
func TestOverflowAlertFirstDropFires(t *testing.T) {
	w := &Writer{}
	if !w.alertDue(time.Now().Unix()) {
		t.Fatal("first drop did not fire an alert (T-M6 regression)")
	}
	// Within the cadence no further alert fires.
	if w.alertDue(time.Now().Unix()) {
		t.Fatal("alert fired again within the cadence")
	}
}

func TestWriterMissingDirFailsClosed(t *testing.T) {
	if _, err := NewWriter(WriterConfig{Path: filepath.Join(t.TempDir(), "no", "such", "dir", "usage.jsonl")}); err == nil {
		t.Fatal("expected error for missing directory")
	}
}

func TestWriterOverflowKnobGoverns(t *testing.T) {
	// T-Q7: accounting.overflow is threaded from config into the writer
	// and fails closed on a policy the writer does not implement, so the
	// parsed knob actually governs the writer.
	if _, err := NewWriter(WriterConfig{Path: filepath.Join(t.TempDir(), "u.jsonl"), Overflow: "panic"}); err == nil {
		t.Fatal("unsupported overflow policy accepted")
	}
	w, err := NewWriter(WriterConfig{Path: filepath.Join(t.TempDir(), "u.jsonl"), Overflow: "drop-and-alert"})
	if err != nil {
		t.Fatalf("drop-and-alert writer: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// T-T6: the dropped counter must be exercised through the real Enqueue
// path (queue-full drops), not by poking w.dropped directly, which only
// re-tests sync/atomic. Race many Enqueues against a tiny queue and
// assert the accounting invariant: every record is either written to the
// file or counted dropped — never lost.
func TestWriterDroppedCounterRealPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	w, err := NewWriter(WriterConfig{Path: path, QueueSize: 2, FSync: "never"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	const total = 2000
	var wg sync.WaitGroup
	for range total {
		wg.Go(func() {
			w.Enqueue(Record{KeyID: "k", TotalTokens: 1, Time: time.Now()})
		})
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var written int64
	for ln := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if ln != "" {
			written++
		}
	}
	dropped := w.Dropped()
	if dropped == 0 {
		t.Fatal("expected some records dropped through the real queue-full path")
	}
	if written+dropped != total {
		t.Fatalf("written(%d)+dropped(%d) = %d, want %d (accounting lost records)", written, dropped, written+dropped, total)
	}
}

// T-M11: records submitted after Close() must be counted as dropped, not
// silently vanish into a queue the consumer has already left.
func TestWriterEnqueueAfterCloseCountsDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	w, err := NewWriter(WriterConfig{Path: path, QueueSize: 8, FSync: "never"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	w.Enqueue(Record{KeyID: "k", TotalTokens: 1, Time: time.Now()})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if w.Dropped() != 0 {
		t.Fatalf("pre-close record dropped: %d", w.Dropped())
	}
	for range 3 {
		w.Enqueue(Record{KeyID: "k", TotalTokens: 1, Time: time.Now()})
	}
	if w.Dropped() != 3 {
		t.Fatalf("records after Close() must count as dropped, got %d", w.Dropped())
	}
}

// T-M11: Close must surface the final-sync error (e.g. ENOSPC) instead of
// discarding it. /dev/full makes every write and sync fail, exactly the
// full-disk class of failure the final-sync path must report.
func TestWriterCloseSurfacesFinalSyncError(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("/dev/full unavailable")
	}
	w, err := NewWriter(WriterConfig{Path: "/dev/full", QueueSize: 4, FSync: "never"})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	w.Enqueue(Record{KeyID: "k", TotalTokens: 1, Time: time.Now()})
	if err := w.Close(); err == nil {
		t.Fatal("Close must surface the final-sync error, got nil")
	}
}
