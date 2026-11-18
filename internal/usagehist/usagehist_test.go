package usagehist

import (
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 18, 12, 30, 0, 0, time.UTC)

func line(t time.Time, key, model, backend string, in, out, charged int64, status string) string {
	return fmt.Sprintf(`{"time":%q,"request_id":"r","key_id":%q,"model":%q,"backend":%q,"status":200,`+
		`"input_tokens":%d,"output_tokens":%d,"total_tokens":%d,"cached_tokens":1,"usage_status":%q,"charged_tokens":%d}`+"\n",
		t.Format(time.RFC3339Nano), key, model, backend, in, out, in+out, status, charged)
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeGz(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	zw.Write([]byte(strings.Join(lines, "")))
	zw.Close()
	f.Close()
}

func total(rows []Row, id string) Row {
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	return Row{}
}

func TestReadsRotatedGzip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "usage.jsonl")
	write(t, p, line(now.Add(-time.Hour), "k1", "m", "b", 10, 5, 15, "exact"))
	write(t, p+".1", line(now.Add(-25*time.Hour), "k1", "m", "b", 1, 1, 2, "exact"))
	writeGz(t, p+".2.gz", line(now.Add(-49*time.Hour), "k1", "m", "b", 2, 2, 4, "unknown"))
	h, err := Open(p, now)
	if err != nil {
		t.Fatal(err)
	}
	r := total(h.Totals(ByKey, 7*24*time.Hour, now), "k1")
	if r.Requests != 3 || r.In != 13 || r.Out != 8 || r.Charged != 21 || r.Unknown != 1 || r.Cached != 3 {
		t.Fatalf("row %+v", r)
	}
	from, to := h.Covers()
	if got := to.Sub(from); got < 47*time.Hour || got > 49*time.Hour {
		t.Fatalf("covers %v..%v", from, to)
	}
	if r := total(h.Totals(ByKey, 24*time.Hour, now), "k1"); r.Requests != 1 {
		t.Fatalf("24h window %+v", r)
	}
	hours := h.Hourly(ByKey, "k1", now)
	if hours[167].Requests != 0 || hours[166].Requests != 1 || hours[167-25].Requests != 1 {
		t.Fatalf("hourly placement wrong: %+v %+v", hours[166], hours[167-25])
	}
}

func TestSevenDayBound(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.jsonl")
	write(t, p, line(now.Add(-8*24*time.Hour), "k1", "m", "b", 1, 1, 2, "exact"),
		line(now.Add(-time.Minute), "k1", "m", "b", 1, 1, 2, "exact"))
	h, err := Open(p, now)
	if err != nil {
		t.Fatal(err)
	}
	if r := total(h.Totals(ByKey, 30*24*time.Hour, now), "k1"); r.Requests != 1 {
		t.Fatalf("old record counted: %+v", r)
	}
}

func TestTailAppendsAndRotation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.jsonl")
	a := line(now.Add(-time.Minute), "k1", "m", "b", 1, 1, 2, "exact")
	write(t, p, a)
	h, err := Open(p, now)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	b := line(now, "k1", "m", "b", 1, 1, 2, "exact")
	f.WriteString(b + b[:20]) // a whole line and half of the next
	f.Close()
	if err := h.Refresh(now); err != nil {
		t.Fatal(err)
	}
	if r := total(h.Totals(ByKey, time.Hour, now), "k1"); r.Requests != 2 {
		t.Fatalf("after append %+v", r)
	}
	f, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(b[20:])
	f.Close()
	h.Refresh(now)
	if r := total(h.Totals(ByKey, time.Hour, now), "k1"); r.Requests != 3 {
		t.Fatalf("completed partial line: %+v", r)
	}
	// copytruncate: the content moves to .1 and the current file empties.
	data, _ := os.ReadFile(p)
	write(t, p+".1", string(data))
	write(t, p)
	if err := h.Refresh(now); err != nil {
		t.Fatal(err)
	}
	if r := total(h.Totals(ByKey, time.Hour, now), "k1"); r.Requests != 3 {
		t.Fatalf("after rotation %+v (double count or loss)", r)
	}
}

func TestFoldsAgentAttachments(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.jsonl")
	write(t, p, line(now, "k", "m", "agent-lab-3", 1, 1, 2, "exact"),
		line(now, "k", "m", "agent-lab-7", 1, 1, 2, "exact"),
		line(now, "k", "m", "qwen@gpu1", 1, 1, 2, "exact"))
	h, _ := Open(p, now)
	rows := h.Totals(ByServer, time.Hour, now)
	if r := total(rows, "agent-lab"); r.Requests != 2 || len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
}

func TestByteBound(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "usage.jsonl")
	write(t, p, line(now.Add(-time.Hour), "k1", "m", "b", 1, 1, 2, "exact"))
	var old []string
	for range 200 {
		old = append(old, line(now.Add(-30*time.Hour), "k1", "m", "b", 1, 1, 2, "exact"))
	}
	write(t, p+".1", old...)
	h, err := open(p, now, 2*len(old[0]))
	if err != nil {
		t.Fatal(err)
	}
	r := total(h.Totals(ByKey, 7*24*time.Hour, now), "k1")
	if r.Requests >= 201 || r.Requests < 1 {
		t.Fatalf("budget ignored: %+v", r)
	}
	if from, _ := h.Covers(); !from.After(now.Add(-30*time.Hour - time.Second)) {
		t.Fatalf("covers from %v", from)
	}
	if !h.Truncated() {
		t.Fatal("cut not reported")
	}
}

func TestCorruptLinesSkipped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.jsonl")
	write(t, p, "not json\n", `{"time":"bogus"}`+"\n", strings.Repeat("x", 100<<10)+"\n",
		line(now, "k1", "m", "b", 1, 1, 2, "exact"))
	h, err := Open(p, now)
	if err != nil {
		t.Fatal(err)
	}
	if r := total(h.Totals(ByKey, time.Hour, now), "k1"); r.Requests != 1 {
		t.Fatalf("row %+v", r)
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "none.jsonl"), now); err == nil {
		t.Fatal("missing accounting file not reported")
	}
}

// An unrotated log larger than the budget loses its oldest records, not
// its newest, and tailing continues from its end.
func TestLargeCurrentFileKeepsNewest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.jsonl")
	var lines []string
	for range 100 {
		lines = append(lines, line(now.Add(-30*time.Hour), "old", "m", "b", 1, 1, 2, "exact"))
	}
	lines = append(lines, line(now.Add(-time.Minute), "new", "m", "b", 1, 1, 2, "exact"))
	write(t, p, lines...)
	h, err := open(p, now, 3*len(lines[0]))
	if err != nil {
		t.Fatal(err)
	}
	rows := h.Totals(ByKey, Span, now)
	if total(rows, "new").Requests != 1 || total(rows, "old").Requests > 3 || !h.Truncated() {
		t.Fatalf("rows %+v truncated=%v", rows, h.Truncated())
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(line(now, "new", "m", "b", 1, 1, 2, "exact"))
	f.Close()
	h.Refresh(now)
	if r := total(h.Totals(ByKey, Span, now), "new"); r.Requests != 2 {
		t.Fatalf("tail after cut %+v", r)
	}
}
