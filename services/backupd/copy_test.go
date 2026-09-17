package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyLocalBackup(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "backups")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "hello.txt"), []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "data.bin"), []byte{1, 2, 3}, 0o600); err != nil {
		t.Fatal(err)
	}

	bytes, err := copyLocalBackup(source, target, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes != 8 {
		t.Fatalf("bytes = %d, want 8", bytes)
	}
	if got, err := os.ReadFile(filepath.Join(target, "run-1", "hello.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("copied file = %q, err = %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "run-1", "nested", "data.bin")); err != nil || len(got) != 3 {
		t.Fatalf("nested file = %v, err = %v", got, err)
	}
}

func TestCopyLocalBackupRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct{ name, source, target string }{
		{"relative source", "source", filepath.Join(root, "target")},
		{"relative target", filepath.Join(root, "source"), "target"},
		{"target inside source", filepath.Join(root, "source"), filepath.Join(root, "source", "backups")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := copyLocalBackup(tc.source, tc.target, "run"); err == nil {
				t.Fatal("expected safety error")
			}
		})
	}
}

func TestCopyLocalBackupRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("source"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := copyLocalBackup(link, filepath.Join(root, "target"), "run"); err == nil {
		t.Fatal("expected symlink rejection")
	}
}
