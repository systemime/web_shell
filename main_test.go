package main

import (
	"os"
	"os/user"
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

func TestSessionSnapshotCopiesHistory(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	app := &appState{sessions: map[string]*session{
		id: {ID: id, Title: "shell", CreatedAt: 1, LastActive: 2, History: []string{"hello"}},
	}}
	info, history := app.sessionSnapshot(id)
	if info.ID != id || info.Title != "shell" || len(history) != 1 || history[0] != "hello" {
		t.Fatalf("bad snapshot: %#v %#v", info, history)
	}
	history[0] = "changed"
	if app.sessions[id].History[0] != "hello" {
		t.Fatal("history snapshot aliases session history")
	}
}

func TestSessionSnapshotLimitsReplayHistory(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	app := &appState{sessions: map[string]*session{
		id: {ID: id, Title: "shell", History: []string{strings.Repeat("x", replayLimit), "new"}},
	}}
	_, history := app.sessionSnapshot(id)
	if len(history) != 1 || history[0] != "new" {
		t.Fatalf("history = %#v", history)
	}
}

func TestTailHistoryKeepsRecentChunks(t *testing.T) {
	history := tailHistory([]string{"old", "middle", "new"}, len("middle")+len("new"))
	if strings.Join(history, ":") != "middle:new" {
		t.Fatalf("history = %#v", history)
	}
}

func TestShellEnvFillsEmptyHome(t *testing.T) {
	t.Setenv("HOME", "")
	env := shellEnv("/tmp/web-worker", "0123456789abcdef0123456789abcdef")
	home := envValue(env, "HOME")
	current, err := user.Current()
	if err == nil && current.HomeDir != "" && home != current.HomeDir {
		t.Fatalf("HOME = %q, want %q", home, current.HomeDir)
	}
	if err != nil && os.Getuid() == 0 && home != "/root" {
		t.Fatalf("HOME = %q, want /root", home)
	}
	if got := envValue(env, "TERM"); got != "xterm-256color" {
		t.Fatalf("TERM = %q", got)
	}
}

func TestShellEnvKeepsExistingHome(t *testing.T) {
	t.Setenv("HOME", "/custom/home")
	env := shellEnv("/tmp/web-worker", "0123456789abcdef0123456789abcdef")
	if got := envValue(env, "HOME"); got != "/custom/home" {
		t.Fatalf("HOME = %q", got)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	value := ""
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			value = strings.TrimPrefix(item, prefix)
		}
	}
	return value
}
