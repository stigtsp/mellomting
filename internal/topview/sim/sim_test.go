package sim

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

var start = time.Date(2026, 11, 18, 13, 23, 34, 0, time.UTC)

func TestSimDeterministic(t *testing.T) {
	a, _ := json.Marshal(New(7, start, nil).Snapshot(0))
	b, _ := json.Marshal(New(7, start, nil).Snapshot(0))
	if string(a) != string(b) {
		t.Fatal("same seed and time gave different data")
	}
	c, _ := json.Marshal(New(8, start, nil).Snapshot(0))
	if string(a) == string(c) {
		t.Fatal("seed ignored")
	}
}

func TestSimAdvancesWithClock(t *testing.T) {
	now := start
	s := New(1, start, func() time.Time { return now })
	first, _ := s.Stats(context.Background(), 0)
	now = now.Add(3 * time.Second)
	next, _ := s.Stats(context.Background(), first.Stats.Seq)
	if next.Stats.Seq != first.Stats.Seq+3 || len(next.Stats.Global.Buckets) != 3 {
		t.Fatalf("seq %d → %d, %d new buckets", first.Stats.Seq, next.Stats.Seq, len(next.Stats.Global.Buckets))
	}
	if first.Stats.Global.Refusals["key_concurrency"] == 0 || len(first.Agents) != 1 || first.Agents[0].Metrics.Waiting < 16 {
		t.Fatal("the screenshot scenario is missing")
	}
}
