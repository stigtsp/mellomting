//go:build !darwin

package seatbelt

import (
	"strings"
	"testing"

	"mellomting/internal/sandbox"
)

// Off macOS the backend must say so and refuse, never silently succeed:
// a required sandbox that quietly enforced nothing is the one outcome
// the mode exists to prevent. This mirrors the Landlock stub's test.
func TestSeatbeltUnavailableOffDarwin(t *testing.T) {
	r := Check()
	if r.Supported {
		t.Fatal("seatbelt reported available on a platform that has none")
	}
	if r.Backend != Backend || r.Reason == "" {
		t.Fatalf("report = %+v, want the backend named and a reason given", r)
	}
	err := Apply(sandbox.Policy{})
	if err == nil {
		t.Fatal("Apply must fail closed off macOS, not silently succeed")
	}
	if !strings.Contains(err.Error(), "macOS") {
		t.Fatalf("err = %v, want it to name the platform", err)
	}
}
