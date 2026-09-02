package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestUsageHelpHasNoPlanReferences: the help text is operator-facing
// output on the target host, which has no repository; PLAN citations
// belong in the repo docs, not in CLI help.
func TestUsageHelpHasNoPlanReferences(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	if strings.Contains(buf.String(), "PLAN") {
		t.Errorf("usage help references PLAN:\n%s", buf.String())
	}
}
