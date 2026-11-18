package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mellomting/internal/inflight"
	"mellomting/internal/stats"
)

type fakeStats struct{ since int64 }

func (f *fakeStats) Stats(since int64) Stats {
	f.since = since
	return Stats{
		NowMillis: 1, Global: GlobalInfo{MaxInflight: 64, Inflight: 3},
		Stats:   stats.Export{Seq: 42},
		Keys:    []KeyInfo{{ID: "k1", Name: "alice", ConcurrentRequests: 8, Inflight: 8}},
		Servers: []ServerInfo{{Name: "gpu1", Kind: "configured", MaxConcurrency: 8}},
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func TestStatsShape(t *testing.T) {
	src := &fakeStats{}
	w := get(t, Handler(inflight.New(), nil, src), "/stats?since=17")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d type %q", w.Code, w.Header().Get("Content-Type"))
	}
	if src.since != 17 {
		t.Fatalf("since passed as %d", src.since)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"now_ms", "started_ms", "global", "stats", "keys", "servers", "agents", "requests"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %q", k)
		}
	}
}

func TestStatsBadSince(t *testing.T) {
	if w := get(t, Handler(inflight.New(), nil, &fakeStats{}), "/stats?since=-1"); w.Code != http.StatusBadRequest {
		t.Fatalf("status %d", w.Code)
	}
}

func TestStatsAbsentWithoutSource(t *testing.T) {
	if w := get(t, Handler(inflight.New(), nil, nil), "/stats"); w.Code != http.StatusNotFound {
		t.Fatalf("status %d", w.Code)
	}
}

// The old view keeps its shape, so an older top still works.
func TestInflightUnchanged(t *testing.T) {
	w := get(t, Handler(inflight.New(), nil, &fakeStats{}), "/inflight")
	var m map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m["requests"] == nil || m["agents"] == nil {
		t.Fatalf("inflight keys %v", m)
	}
}
