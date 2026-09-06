package landlock

import (
	"runtime"
	"testing"

	"mellomting/internal/sandbox"
)

func TestApplyNonLinuxFailsClosed(t *testing.T) {
	// On every platform Apply must be callable and fail closed when the
	// policy cannot be enforced (PLAN §55, §57). On Linux this only
	// errors for an out-of-range ABI; the full enforcement path is
	// covered by the Linux integration test.
	if runtime.GOOS == "linux" {
		if err := Apply(99, sandbox.Policy{}); err == nil {
			t.Fatal("out-of-range ABI must fail closed")
		}
		return
	}
	if err := Apply(9, sandbox.Policy{}); err == nil {
		t.Fatal("non-Linux platform must report Landlock as unavailable, not silently succeed")
	}
}
