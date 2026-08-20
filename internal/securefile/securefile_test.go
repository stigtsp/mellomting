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

	mode, err := LStat(link)
	if err == nil {
		t.Fatalf("LStat symlink succeeded: %v", mode)
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

func TestWorldWritable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	set := func(name string, perm os.FileMode) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, nil, perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, perm); err != nil { // umask-proof
			t.Fatal(err)
		}
	}
	set("a", 0o646)
	mode, err := LStat(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if !WorldWritable(mode) {
		t.Fatal("0646 not reported world-writable")
	}
	set("b", 0o640)
	mode, err = LStat(filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	if WorldWritable(mode) {
		t.Fatal("0640 reported world-writable")
	}
}
