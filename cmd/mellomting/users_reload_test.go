package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUsersWatcherPoll pins the poll state machine: a change of mtime,
// mode, or identity is read once; unchanged metadata is not; a missing
// or unreadable file is reported once and retried when it changes.
func TestUsersWatcherPoll(t *testing.T) {
	const content = "version: 1\nkeys: []\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "users.yaml")
	w := &usersWatcher{path: path}

	expect := func(t *testing.T, what string, force, wantChanged, wantErr bool) {
		t.Helper()
		data, changed, err := w.poll(force)
		if (err != nil) != wantErr || changed != wantChanged {
			t.Fatalf("%s: changed=%v err=%v, want changed=%v err=%v", what, changed, err, wantChanged, wantErr)
		}
		if changed && string(data) != content {
			t.Fatalf("%s: data = %q", what, data)
		}
	}

	expect(t, "missing file", false, false, true)
	expect(t, "still missing", false, false, false)
	expect(t, "still missing, forced", true, false, true)

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	expect(t, "file appeared", false, true, false)
	expect(t, "unchanged", false, false, false)
	expect(t, "unchanged, forced", true, true, false)

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	touched := info.ModTime().Add(time.Second)
	if err := os.Chtimes(path, touched, touched); err != nil {
		t.Fatal(err)
	}
	expect(t, "mtime changed", false, true, false)

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	expect(t, "mode changed", false, true, false)

	// Atomic replacement preserving size, mode, and mtime must reload.
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(replacement, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, touched, touched); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	expect(t, "replaced by rename", false, true, false)

	// Unreadable (world-readable mode): reported once, retried on change.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, "unreadable", false, false, true)
	expect(t, "still unreadable", false, false, false)
	expect(t, "still unreadable, forced", true, false, true)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	expect(t, "readable again", false, true, false)

	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&usersWatcher{path: link}).poll(true); err == nil {
		t.Fatal("the watcher followed a symlink")
	}
	if _, err := (&usersWatcher{path: link}).load(); err == nil {
		t.Fatal("load followed a symlink")
	}
}

func TestSameUsersFileTreatsAbsentAsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nkeys: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !sameUsersFile(nil, nil) || sameUsersFile(nil, info) || sameUsersFile(info, nil) {
		t.Fatal("absent compares equal only to absent")
	}
}
