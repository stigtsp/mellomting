package accounting

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestReportFileAggregates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")

	write := func(r Record) {
		b, _ := json.Marshal(r)
		f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
	}

	write(Record{KeyID: "key-a", InputTokens: 10, OutputTokens: 5, TotalTokens: 15, CachedTokens: 2})
	write(Record{KeyID: "key-a", InputTokens: 20, OutputTokens: 10, TotalTokens: 30, CachedTokens: 4})
	write(Record{KeyID: "key-b", InputTokens: 1, OutputTokens: 1, TotalTokens: 2, CachedTokens: 0})
	// A malformed line must be skipped, not fatal.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o640)
	_, _ = f.WriteString("{not json}\n")
	_ = f.Close()

	rep, err := ReportFile(path)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(rep.Keys) != 2 {
		t.Fatalf("want 2 keys, got %d", len(rep.Keys))
	}
	got := map[string]KeyUsage{}
	for _, k := range rep.Keys {
		got[k.KeyID] = k
	}
	a := got["key-a"]
	if a.Requests != 2 || a.InputTokens != 30 || a.OutputTokens != 15 || a.TotalTokens != 45 || a.CachedTokens != 6 {
		t.Fatalf("key-a unexpected: %+v", a)
	}
	b := got["key-b"]
	if b.Requests != 1 || b.TotalTokens != 2 {
		t.Fatalf("key-b unexpected: %+v", b)
	}
}

func TestReportFileMissing(t *testing.T) {
	rep, err := ReportFile(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(rep.Keys) != 0 {
		t.Fatalf("want empty report, got %d keys", len(rep.Keys))
	}
}

func TestReportFileDoesNotWrap(t *testing.T) {
	// Extreme totals are clamped on read to the X11 bounds (FIX-07/N4)
	// before summing, so the aggregate can never wrap negative or reach a
	// bogus MaxInt64 (T-X11): two MaxInt64 records yield 2 * maxUsageTokens
	// per field, not MaxInt64 and not a negative wrap.
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	f, _ := os.Create(path)
	for i := 0; i < 2; i++ {
		b, _ := json.Marshal(Record{KeyID: "k", TotalTokens: math.MaxInt64, InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64})
		_, _ = f.Write(append(b, '\n'))
	}
	_ = f.Close()

	rep, err := ReportFile(path)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(rep.Keys) != 1 {
		t.Fatalf("want 1 key, got %d", len(rep.Keys))
	}
	k := rep.Keys[0]
	for _, v := range []int64{k.InputTokens, k.OutputTokens, k.TotalTokens} {
		if v < 0 {
			t.Fatalf("reported total wrapped negative: %d", v)
		}
	}
	if want := 2 * maxUsageTokens; k.InputTokens != want || k.OutputTokens != want || k.TotalTokens != want {
		t.Fatalf("clamped fields = %+v, want each %d", k, want)
	}
}

func TestReportFileClampsNegative(t *testing.T) {
	// FIX-07/N4: a corrupt line (e.g. written by a pre-fix binary) with a
	// negative token field must never blow the aggregate up to MaxInt64;
	// the field is clamped to 0 on read.
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	f, _ := os.Create(path)
	b, _ := json.Marshal(Record{KeyID: "k", InputTokens: 10, OutputTokens: 50, TotalTokens: 60})
	_, _ = f.Write(append(b, '\n'))
	// Second record carries the corrupt negative output (review's log).
	b, _ = json.Marshal(Record{KeyID: "k", InputTokens: 91, OutputTokens: -1, TotalTokens: 90})
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()

	rep, err := ReportFile(path)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(rep.Keys) != 1 {
		t.Fatalf("want 1 key, got %d", len(rep.Keys))
	}
	k := rep.Keys[0]
	if k.Requests != 2 {
		t.Fatalf("requests = %d, want 2", k.Requests)
	}
	if k.OutputTokens != 50 {
		t.Fatalf("output = %d, want 50 (negative clamped, never MaxInt64)", k.OutputTokens)
	}
	if k.InputTokens != 101 || k.TotalTokens != 150 {
		t.Fatalf("input/total = %d/%d, want 101/150", k.InputTokens, k.TotalTokens)
	}
}
