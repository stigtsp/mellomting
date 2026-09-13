package securefile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// A directory in a group other than the caller's own was provisioned
// for a service account; one in the caller's group was not.
func TestServiceGroup(t *testing.T) {
	if gid, ok := ServiceGroup(1234, 1000); !ok || gid != 1234 {
		t.Fatalf("ServiceGroup(1234, 1000) = %d, %v; want 1234, true", gid, ok)
	}
	if _, ok := ServiceGroup(1000, 1000); ok {
		t.Fatal("a directory in the caller's own group was taken for a service directory")
	}
}

// A privileged `key create` that writes the FIRST users file has no
// existing owner to preserve, and used to leave a 0600 file owned by
// root's own group in a directory set up as root:<service group>. The
// service account could then not read the file the installer had just
// arranged for it, and the unit failed to start. A new file adopts the
// directory's group and becomes group-readable.
func TestReplacePreservingOwnerAdoptsDirGroupForNewFile(t *testing.T) {
	gid, ok := otherGroup(t)
	if !ok {
		t.Skip("no supplementary group to stand in for a service group")
	}
	dir := t.TempDir()
	if err := os.Chown(dir, -1, gid); err != nil {
		t.Skipf("cannot set the directory group to %d: %v", gid, err)
	}

	path := filepath.Join(dir, "users.yaml")
	if err := ReplacePreservingOwner(path, 0o600, 0o640, func(w io.Writer) error {
		_, err := w.Write([]byte("version: 1\n"))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no unix stat available")
	}
	if int(sys.Gid) != gid {
		t.Fatalf("gid = %d, want the directory's %d", sys.Gid, gid)
	}
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %#o, want 0640 (the service group cannot read it otherwise)", st.Mode().Perm())
	}
}

// The adoption is conditional: a directory owned by the caller's own
// group designates no service account, so a new file stays 0600.
func TestReplacePreservingOwnerKeepsPrivateModeWithoutServiceGroup(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chown(dir, -1, os.Getegid()); err != nil {
		t.Skipf("cannot set the directory group: %v", err)
	}
	path := filepath.Join(dir, "users.yaml")
	if err := ReplacePreservingOwner(path, 0o600, 0o640, func(w io.Writer) error {
		_, err := w.Write([]byte("version: 1\n"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %#o, want 0600", st.Mode().Perm())
	}
}

// otherGroup returns a group the test process belongs to that is not its
// effective group, so a chown to it succeeds unprivileged.
func otherGroup(t *testing.T) (int, bool) {
	t.Helper()
	groups, err := os.Getgroups()
	if err != nil {
		return 0, false
	}
	for _, g := range groups {
		if g != os.Getegid() {
			return g, true
		}
	}
	return 0, false
}
