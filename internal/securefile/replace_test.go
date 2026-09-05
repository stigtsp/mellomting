package securefile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeString(s string) func(io.Writer) error {
	return func(w io.Writer) error {
		_, err := io.WriteString(w, s)
		return err
	}
}

func TestReplaceCreatesWithMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.txt")
	if err := Replace(path, 0o640, writeString("hello")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "hello" {
		t.Fatalf("content = %q, err = %v", data, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640", st.Mode().Perm())
	}
}

func TestReplaceOverwritesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(path, 0o600, writeString("new")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new" {
		t.Fatalf("content = %q, want new", data)
	}
}

// A symlink at the destination must be refused rather than followed: a
// privileged write must not be redirectable by whoever can create a name
// in the directory.
func TestReplaceRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err := Replace(link, 0o600, writeString("attacker"))
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want a symlink refusal", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "target" {
		t.Fatalf("symlink target was written through: %q", data)
	}
}

func TestReplaceRefusesNonRegular(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "adir")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	err := Replace(sub, 0o600, writeString("x"))
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v, want a non-regular refusal", err)
	}
}

// A failed write must leave neither a partial destination nor a stray
// temporary file behind.
func TestReplaceLeavesNothingOnWriteFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("boom")
	err := Replace(path, 0o600, func(io.Writer) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the write error", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "original" {
		t.Fatalf("destination was modified: %q", data)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary file left behind: %d entries", len(entries))
	}
}

// ReplacePreservingOwner clamps an existing file's mode rather than
// widening it, and uses the fallback mode only for a new file.
func TestReplacePreservingOwnerClampsMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.yaml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReplacePreservingOwner(path, 0o600, 0o640, writeString("new")); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want the existing 0600 preserved, never widened", st.Mode().Perm())
	}

	fresh := filepath.Join(dir, "fresh.yaml")
	if err := ReplacePreservingOwner(fresh, 0o600, 0o640, writeString("x")); err != nil {
		t.Fatal(err)
	}
	st, _ = os.Stat(fresh)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestReplacePreservingOwnerKeepsGroupReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.yaml")
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := ReplacePreservingOwner(path, 0o600, 0o640, writeString("new")); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640 preserved so the daemon can still read it", st.Mode().Perm())
	}
}
