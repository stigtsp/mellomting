package apierr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// This package is the single place the sanitized client-facing envelope
// is produced (PLAN §72), so the shape is pinned here rather than only
// indirectly through the packages that call it.
func TestWriteEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	Write(w, http.StatusNotFound, "invalid_request_error", "model_not_found", MsgNotFound)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var got struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Param   *string `json:"param"`
			Code    string  `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not the OpenAI error shape: %v (%s)", err, w.Body)
	}
	if got.Error.Message != MsgNotFound || got.Error.Type != "invalid_request_error" ||
		got.Error.Code != "model_not_found" {
		t.Fatalf("envelope = %+v", got.Error)
	}
	if got.Error.Param != nil {
		t.Fatalf("param = %v, want null", *got.Error.Param)
	}
}

// The standard messages must stay free of anything that could identify a
// backend, a path, or an internal error (PLAN §72).
func TestStandardMessagesRevealNothing(t *testing.T) {
	for _, msg := range []string{MsgNotFound, MsgBadMethod, MsgBadAuth, MsgOverload, MsgRateLimit} {
		for _, leak := range []string{"http://", "https://", "/etc/", "/var/", "goroutine", ".go:"} {
			if strings.Contains(msg, leak) {
				t.Errorf("message %q contains %q", msg, leak)
			}
		}
	}
}

func TestWriteRateLimitCarriesRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		wait time.Duration
		want string
	}{
		{1500 * time.Millisecond, "2"}, // rounds up, never below the real wait
		{time.Second, "1"},
		{time.Nanosecond, "1"}, // floors at 1, never "0"
	} {
		w := httptest.NewRecorder()
		WriteRateLimit(w, tc.wait)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", w.Code)
		}
		if got := w.Header().Get("Retry-After"); got != tc.want {
			t.Errorf("Retry-After for %v = %q, want %q", tc.wait, got, tc.want)
		}
	}
	// No computable wait: the header is left for the caller's default
	// rather than emitted as "0", which a client would busy-retry on.
	w := httptest.NewRecorder()
	WriteRateLimit(w, 0)
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After with no wait = %q, want unset", got)
	}
}
