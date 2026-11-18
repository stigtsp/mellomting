package stats

import (
	"fmt"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newTestStore() (*Store, *clock) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	return New(c.now), c
}

func ok(key, model, server string) Sample {
	return Sample{KeyID: key, KeyName: key + "-name", Model: model, Server: server, Status: 200,
		BytesIn: 10, BytesOut: 20, TokensIn: 3, TokensOut: 4, TTFB: 50 * time.Millisecond, Duration: time.Second}
}

func find(e Export, dim, id string) *SeriesExport {
	for i := range e.Series {
		if e.Series[i].Dim == dim && e.Series[i].ID == id {
			return &e.Series[i]
		}
	}
	return nil
}

func TestRecordAndSince(t *testing.T) {
	s, c := newTestStore()
	s.Record(ok("k1", "m", "srv"))
	s.Record(ok("k1", "m", "srv"))
	c.add(time.Second)
	s.Record(ok("k1", "m", "srv"))

	e := s.Since(0, 1<<20)
	if len(e.Global.Buckets) != 2 || e.Global.Buckets[0].OK != 2 || e.Global.Buckets[1].OK != 1 {
		t.Fatalf("global buckets %+v", e.Global.Buckets)
	}
	k := find(e, "key", "k1")
	if k == nil || k.Name != "k1-name" || len(k.Buckets) != 2 {
		t.Fatalf("key series %+v", k)
	}
	if b := e.Global.Buckets[0]; b.BytesIn != 20 || b.TokensOut != 8 || b.TTFBSumMs != 100 || b.TTFBN != 2 {
		t.Fatalf("sums %+v", b)
	}
	if find(e, "model", "m") == nil || find(e, "server", "srv") == nil {
		t.Fatal("model or server series missing")
	}
	later := s.Since(e.Global.Buckets[0].Seq, 1<<20)
	if len(later.Global.Buckets) != 1 || later.Global.Buckets[0].OK != 1 {
		t.Fatalf("since returned %+v", later.Global.Buckets)
	}
	if e.Seq != e.Global.Buckets[1].Seq {
		t.Fatalf("seq %d, newest bucket %d", e.Seq, e.Global.Buckets[1].Seq)
	}
}

func TestRefusalNotCompleted(t *testing.T) {
	s, _ := newTestStore()
	s.Refuse(KeyConcurrency, "k1", "alice", "", "")
	e := s.Since(0, 1<<20)
	k := find(e, "key", "k1")
	if k == nil || len(k.Buckets) != 1 {
		t.Fatalf("key series %+v", k)
	}
	b := k.Buckets[0]
	if b.Refused != 1 || b.OK != 0 || b.C4xx != 0 || b.C5xx != 0 {
		t.Fatalf("bucket %+v", b)
	}
	if k.Refusals["key_concurrency"] != 1 || e.Global.Refusals["key_concurrency"] != 1 {
		t.Fatalf("refusal totals key=%v global=%v", k.Refusals, e.Global.Refusals)
	}
	if e.Global.Buckets[0].Refused != 1 {
		t.Fatal("global bucket did not count the refusal")
	}
}

func TestTerminalClassification(t *testing.T) {
	for _, tc := range []struct {
		class   string
		status  int
		refusal Refusal
	}{
		{"queue_full", 503, BackendQueue},
		{"queue_timeout", 503, BackendQueue},
		{"token_quota", 429, Quota},
		{"overload", 503, Draining},
	} {
		s, _ := newTestStore()
		smp := ok("k", "m", "srv")
		smp.Status, smp.ErrorClass = tc.status, tc.class
		s.Record(smp)
		e := s.Since(0, 1<<20)
		b := e.Global.Buckets[0]
		if b.Refused != 1 || b.C5xx != 0 || b.C4xx != 0 || e.Global.Refusals[string(tc.refusal)] != 1 {
			t.Errorf("%s: bucket %+v refusals %v", tc.class, b, e.Global.Refusals)
		}
		if len(e.Global.Errors) != 0 {
			t.Errorf("%s counted as an error: %v", tc.class, e.Global.Errors)
		}
	}

	s, _ := newTestStore()
	smp := ok("k", "m", "srv")
	smp.Status, smp.ErrorClass = 502, "backend_connect"
	s.Record(smp)
	e := s.Since(0, 1<<20)
	if b := e.Global.Buckets[0]; b.C5xx != 1 || b.Refused != 0 || e.Global.Errors["backend_connect"] != 1 {
		t.Fatalf("error: bucket %+v errors %v", b, e.Global.Errors)
	}
	if k := find(e, "key", "k"); k.Errors["backend_connect"] != 1 {
		t.Fatalf("key errors %v", k.Errors)
	}
}

func TestWindowRollsOff(t *testing.T) {
	s, c := newTestStore()
	s.Record(ok("k1", "m", "srv"))
	c.add(Window + time.Second)
	s.Record(ok("k2", "m", "srv"))
	e := s.Since(0, 1<<20)
	if len(e.Global.Buckets) != 1 {
		t.Fatalf("old bucket kept: %+v", e.Global.Buckets)
	}
	if find(e, "key", "k1") != nil {
		t.Fatal("idle series kept past the window")
	}
}

func TestSeriesCapFoldsToOther(t *testing.T) {
	s, _ := newTestStore()
	for i := range 70 {
		s.Record(ok(fmt.Sprintf("k%02d", i), "m", "srv"))
	}
	e := s.Since(0, 1<<22)
	keys := 0
	var other *SeriesExport
	for i := range e.Series {
		if e.Series[i].Dim == "key" {
			keys++
			if e.Series[i].ID == OtherID {
				other = &e.Series[i]
			}
		}
	}
	if keys != MaxSeries+1 || other == nil || other.Buckets[0].OK != 70-MaxSeries {
		t.Fatalf("keys=%d other=%+v", keys, other)
	}
}

func TestGaugesSampled(t *testing.T) {
	s, _ := newTestStore()
	s.Sample(Gauges{
		GlobalInflight: 5,
		Keys:           map[string]KeyGauge{"k1": {Name: "alice", Inflight: 8, AtLimit: true}},
		Servers:        map[string]ServerGauge{"gpu": {Admitted: 24, Queued: 2, HasMetrics: true, Running: 6, Waiting: 18, KVPercent: 94}},
	})
	e := s.Since(0, 1<<20)
	if g := e.Global.Buckets[0]; g.Inflight != 5 {
		t.Fatalf("global gauge %+v", g)
	}
	if b := find(e, "key", "k1").Buckets[0]; b.Inflight != 8 || !b.AtLimit {
		t.Fatalf("key gauge %+v", b)
	}
	b := find(e, "server", "gpu").Buckets[0]
	if b.Admitted != 24 || b.Queued != 2 || b.Running != 6 || b.Waiting != 18 || b.KV != 94 || !b.Metrics {
		t.Fatalf("server gauge %+v", b)
	}
}

func TestSinceTruncates(t *testing.T) {
	s, c := newTestStore()
	for sec := range 60 {
		for i := range 3 * MaxSeries {
			smp := ok(fmt.Sprintf("k%03d", i%MaxSeries), fmt.Sprintf("m%03d", i%MaxSeries), fmt.Sprintf("s%03d", i%MaxSeries))
			if i%MaxSeries == 0 {
				s.Record(smp) // k000 is the busiest
			}
			s.Record(smp)
		}
		_ = sec
		c.add(time.Second)
	}
	e := s.Since(0, 64<<10)
	if !e.Truncated {
		t.Fatal("not truncated")
	}
	busy, quiet := find(e, "key", "k000"), find(e, "key", "k063")
	if busy == nil || len(busy.Buckets) == 0 {
		t.Fatal("busiest series lost its buckets")
	}
	if quiet == nil || quiet.Buckets != nil || quiet.Requests == 0 {
		t.Fatalf("quiet series %+v", quiet)
	}
	if len(e.Global.Buckets) != 60 {
		t.Fatal("global series truncated")
	}
}

func TestNilStore(t *testing.T) {
	var s *Store
	s.Record(ok("k", "m", "s"))
	s.Refuse(Auth, "", "", "", "")
	s.Sample(Gauges{})
	if e := s.Since(0, 1); e.Seq != 0 || len(e.Series) != 0 {
		t.Fatalf("nil store export %+v", e)
	}
}

func BenchmarkRecord(b *testing.B) {
	s, _ := newTestStore()
	smp := ok("k1", "m", "srv")
	s.Record(smp)
	b.ReportAllocs()
	for b.Loop() {
		s.Record(smp)
	}
}
