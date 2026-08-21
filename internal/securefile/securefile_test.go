package securefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadRegularFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pepper")
	if err := os.WriteFile(path, []byte("pepper-bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := Read(path, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "pepper-bytes\n" {
		t.Fatalf("data = %q", data)
	}
}

func TestReadRefusesSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(link, 0); err == nil {
		t.Fatal("symlink read succeeded")
	}
}

func TestReadRefusesDirectory(t *testing.T) {
	t.Parallel()

	if _, err := Read(t.TempDir(), 0); err == nil {
		t.Fatal("directory read succeeded")
	}
}

func TestReadBounded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	data := make([]byte, 101)
	for i := range data {
		data[i] = 'x'
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	// Exactly at the bound: ok.
	got, err := Read(path, 101)
	if err != nil {
		t.Fatalf("Read at bound: %v", err)
	}
	if len(got) != 101 {
		t.Fatalf("len = %d, want 101", len(got))
	}

	// One over: rejected (truncating a sensitive file is worse than
	// refusing it).
	if _, err := Read(path, 100); err == nil {
		t.Fatal("oversized read succeeded")
	}
}

func TestWorldAccessible(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	set := func(name string, perm os.FileMode) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, perm); err != nil { // umask-proof
			t.Fatal(err)
		}
	}
	// Reject: any "other" access bit exposes the secret (T-M8).
	for _, perm := range []os.FileMode{0o644, 0o666, 0o777, 0o604} {
		if !WorldAccessible(perm) {
			t.Fatalf("mode %04o must be reported world-accessible", perm)
		}
	}
	// Accept: owner-only and owner+group modes.
	for _, perm := range []os.FileMode{0o600, 0o640, 0o660, 0o700} {
		if WorldAccessible(perm) {
			t.Fatalf("mode %04o must not be reported world-accessible", perm)
		}
	}
	// Read refuses a world-accessible file (fail closed).
	set("bad", 0o644)
	if _, err := Read(filepath.Join(dir, "bad"), 0); err == nil {
		t.Fatal("world-readable file read succeeded")
	}
	set("good", 0o640)
	if _, err := Read(filepath.Join(dir, "good"), 0); err != nil {
		t.Fatalf("0640 read failed: %v", err)
	}
}
