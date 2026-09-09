package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUsersFileMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nkeys: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, original, err := readUsersSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	_, unchanged, err := readUsersSnapshot(path)
	if err != nil || !sameUsersFile(original, unchanged) {
		t.Fatalf("unchanged file: %v", err)
	}
	modified := original.ModTime().Add(time.Second)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	_, touched, err := readUsersSnapshot(path)
	if err != nil || sameUsersFile(original, touched) {
		t.Fatalf("mtime change was missed: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	_, chmod, err := readUsersSnapshot(path)
	if err != nil || sameUsersFile(touched, chmod) {
		t.Fatalf("permission change was missed: %v", err)
	}
	// Atomic replacement with the same size, mode, and mtime must reload.
	replacement := filepath.Join(filepath.Dir(path), "replacement")
	if err := os.WriteFile(replacement, []byte("version: 1\nkeys: []\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(replacement, chmod.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, chmod.ModTime(), chmod.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	_, replaced, err := readUsersSnapshot(path)
	if err != nil || sameUsersFile(chmod, replaced) {
		t.Fatalf("atomic replacement was missed: %v", err)
	}
	link := filepath.Join(filepath.Dir(path), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readUsersSnapshot(link); err == nil {
		t.Fatal("users snapshot followed a symlink")
	}
}
