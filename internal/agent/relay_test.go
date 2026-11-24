package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"mellomting/internal/discovery"
)

func TestRelayableRoutes(t *testing.T) {
	for _, c := range []struct {
		method, target string
		want           bool
	}{
		{"GET", "/v1/models", true},
		{"POST", "/v1/chat/completions", true},
		{"POST", "/v1/completions", true},
		{"POST", "/v1/embeddings", true},
		{"POST", "/v1/responses", true},
		{"GET", "/v1/responses/resp_abc-1", true},
		{"POST", "/v1/responses/resp_abc-1/cancel", true},
		{"GET", "/v1/responses/resp_abc-1/cancel", false},
		{"POST", "/v1/responses/resp_abc-1", false},
		{"POST", "/v1/models", false},
		{"GET", "/v1/chat/completions", false},
		{"POST", "/v1/load_lora_adapter", false},
		{"GET", "/metrics", false},
		{"GET", "/v1/../metrics", false},
		{"GET", "/v1/%2e%2e/metrics", false},
		{"GET", "/v1/..%2fmetrics", false},
		{"GET", "/v1//models", false},
		{"GET", "/v1/models?x=1", false},
		{"GET", "/v1/responses/../../metrics", false},
		{"GET", "/v1/responses/a.b", false},
	} {
		r := httptest.NewRequest(c.method, "http://agent"+c.target, nil)
		if got := relayable(r, false); got != c.want {
			t.Errorf("%s %s: relayable = %v, want %v", c.method, c.target, got, c.want)
		}
	}
}

// A decision server is relayed POST /v1/systemone and nothing else, and
// a discovered server is not relayed it (SYSTEMONE_PLAN §D13).
func TestRelayableDecisionRoutes(t *testing.T) {
	for _, c := range []struct {
		method, target string
		decision, want bool
	}{
		{"POST", "/v1/systemone", true, true},
		{"POST", "/v1/systemone", false, false},
		{"GET", "/v1/systemone", true, false},
		{"POST", "/v1/systemone/x", true, false},
		{"POST", "/v1/systemone?x=1", true, false},
		{"POST", "/v1//systemone", true, false},
		{"POST", "/v1/%73ystemone", true, false},
		{"POST", "/v1/chat/completions", true, false},
		{"POST", "/v1/embeddings", true, false},
		{"POST", "/v1/responses", true, false},
		{"GET", "/v1/models", true, false},
		{"GET", "/healthz", true, false},
		{"GET", "/metrics", true, false},
	} {
		r := httptest.NewRequest(c.method, "http://agent"+c.target, nil)
		if got := relayable(r, c.decision); got != c.want {
			t.Errorf("%s %s (decision %v): relayable = %v, want %v", c.method, c.target, c.decision, got, c.want)
		}
	}
}

func TestParseModelTypes(t *testing.T) {
	adv := []advertised{{Model: discovery.Model{ID: "k"}}, {Model: discovery.Model{ID: "j"}}}
	got, err := parseModelTypes([]byte(`{"decision":["k"]}`), adv)
	if err != nil || len(got) != 1 || !got["k"] {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := parseModelTypes([]byte(`{"decision":[]}`), adv); err != nil || len(got) != 0 {
		t.Fatalf("empty list: %v, %v", got, err)
	}
	for _, bad := range []string{
		``, `null`, `[]`, `"k"`, `{}`, `{"decision":null}`, `{"decision":"k"}`,
		`{"decision":["x"]}`, `{"decision":["k","k"]}`, `{"decision":[null]}`, `{"decision":[1]}`,
		`{"decision":["k"],"generation":["j"]}`, `{"decision":["k"]} {}`, `{"decision":["k"]`,
	} {
		if got, err := parseModelTypes([]byte(bad), adv); err == nil {
			t.Errorf("%q: accepted as %v", bad, got)
		}
	}
}

func TestSanitizeHeaders(t *testing.T) {
	for _, c := range []struct{ ct, ra, wantCT, wantRA string }{
		{"application/json", "5", "application/json", "5"},
		{"text/event-stream; charset=utf-8", "", "text/event-stream; charset=utf-8", ""},
		{"text/html", "99999", "application/octet-stream", "60"},
		{"application/json", "Wed, 21 Oct 2015 07:28:00 GMT", "application/json", ""},
		{"application/json", "-3", "application/json", ""},
	} {
		h := http.Header{}
		h.Set("Content-Type", c.ct)
		if c.ra != "" {
			h.Set("Retry-After", c.ra)
		}
		sanitizeHeaders(h)
		if h.Get("Content-Type") != c.wantCT || h.Get("Retry-After") != c.wantRA {
			t.Errorf("%q/%q -> %q/%q, want %q/%q", c.ct, c.ra, h.Get("Content-Type"), h.Get("Retry-After"), c.wantCT, c.wantRA)
		}
	}
}
