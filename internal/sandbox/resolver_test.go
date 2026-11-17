package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Only a backend named by hostname needs the resolver; a configuration
// of IP literals must keep the narrower policy.
func TestNamesHost(t *testing.T) {
	for _, tc := range []struct {
		urls []string
		want bool
	}{
		{nil, false},
		{[]string{"http://127.0.0.1:8001", "http://[::1]:8002", "https://[fd00::5]:443"}, false},
		{[]string{"http://[fe80::1%25eth0]:8001"}, false},
		{[]string{"http://127.0.0.1:8001", "https://gpu.example.org:443"}, true},
		{[]string{"http://localhost:8001"}, true},
	} {
		if got := NamesHost(tc.urls...); got != tc.want {
			t.Errorf("NamesHost(%v) = %v, want %v", tc.urls, got, tc.want)
		}
	}
}

// The grant is the directory of each file, as written and as resolved
// through symlinks, because resolv.conf is replaced by rename and a
// per-file grant would stop matching at the first rewrite. A missing
// directory is left out: Landlock refuses a rule it cannot open.
func TestResolverDirs(t *testing.T) {
	// Resolved up front so the temporary directory's own symlinks
	// (/var -> /private/var on macOS) do not add a second form of etc.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	etc := filepath.Join(root, "etc")
	run := filepath.Join(root, "run", "resolve")
	for _, d := range []string{etc, run} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub := filepath.Join(run, "stub-resolv.conf")
	if err := os.WriteFile(stub, []byte("nameserver 127.0.0.53\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stub, filepath.Join(etc, "resolv.conf")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "hosts"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	got := resolverDirs([]string{
		filepath.Join(etc, "resolv.conf"),
		filepath.Join(etc, "hosts"),
		filepath.Join(etc, "nsswitch.conf"), // absent: its directory still counts
		filepath.Join(root, "missing", "resolv.conf"),
	})
	want := []string{etc, run}
	if !slices.Equal(got, want) {
		t.Fatalf("resolverDirs = %v, want %v", got, want)
	}
}

// The summary must say why the resolver is reachable, so an operator
// can see that using IP literals removes the grant.
func TestPolicySummarizeResolve(t *testing.T) {
	sum := strings.Join(Policy{Resolve: true}.Summarize(), " | ")
	if !strings.Contains(sum, "resolve dns (a backend is named by hostname)") {
		t.Fatalf("summary = %s", sum)
	}
	if sum := (Policy{}).Summarize(); len(sum) != 0 {
		t.Fatalf("empty policy summary = %v", sum)
	}
}
