package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathGuardRejectsTraversalAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	guard, err := newPathGuard(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := guard.existing("ok.txt"); got.rel != "ok.txt" {
		t.Fatalf("rel = %q", got.rel)
	}
	if _, err := guard.existing("../etc/passwd"); err == nil {
		t.Fatal("expected traversal error")
	}
	if _, err := guard.existing(filepath.Join(outside, "x")); err == nil {
		t.Fatal("expected absolute outside error")
	}
	if _, err := guard.existing("outside"); err == nil {
		t.Fatal("expected symlink escape error")
	}
}

func TestCleanTitleAndDefaultTitle(t *testing.T) {
	if got := cleanTitle("  hello\n world  "); got != "hello world" {
		t.Fatalf("cleanTitle = %q", got)
	}
	long := cleanTitle(strings.Repeat("中", 90))
	if len([]rune(long)) != 80 {
		t.Fatalf("title length = %d", len([]rune(long)))
	}
	if got := defaultTitle("0123456789abcdef0123456789abcdef"); got != "shell 01234567" {
		t.Fatalf("defaultTitle = %q", got)
	}
}
