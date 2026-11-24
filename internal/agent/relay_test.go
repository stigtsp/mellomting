package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
		{"POST", "/v1/systemone", true},
		{"GET", "/v1/systemone", false},
		{"POST", "/v1/systemone/x", false},
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
		if got := relayable(r); got != c.want {
			t.Errorf("%s %s: relayable = %v, want %v", c.method, c.target, got, c.want)
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
