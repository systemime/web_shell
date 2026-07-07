package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestSessionWorkerProtocol(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	id := "0123456789abcdef0123456789abcdef"
	root := t.TempDir()
	workTmp := t.TempDir()
	sessionDir := shortTempDir(t)
	sock := filepath.Join(sessionDir, id+".sock")
	done := make(chan error, 1)
	go func() {
		done <- runSessionWorker([]string{id, sock, root, workTmp, sessionDir, "80", "24", "1"})
	}()
	for deadline := time.Now().Add(3 * time.Second); !pingWorker(sock); {
		select {
		case err := <-done:
			t.Fatalf("worker exited: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() { stopWorker(sock) })

	conn, enc, dec, err := dialWorker(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := enc.Encode(workerMessage{Type: "attach", Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	var msg workerMessage
	if err := dec.Decode(&msg); err != nil || msg.Type != "history" {
		t.Fatalf("history reply = %#v, %v", msg, err)
	}
	if err := enc.Encode(workerMessage{Type: "input", Data: "echo worker-ok\r"}); err != nil {
		t.Fatal(err)
	}
	output := ""
	for !strings.Contains(output, "worker-ok") {
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == "output" {
			output += msg.Data
		}
	}
	if err := enc.Encode(workerMessage{Type: "close"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestSessionDaemonStartsAndProxiesWorker(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	sessionDir := shortTempDir(t)
	cfg := config{
		root:         t.TempDir(),
		workTmp:      t.TempDir(),
		sessionDir:   sessionDir,
		sessiondSock: filepath.Join(sessionDir, "sessiond.sock"),
	}
	errs := make(chan error, 1)
	go func() { errs <- runSessionDaemon(cfg) }()
	for deadline := time.Now().Add(3 * time.Second); !pingDaemon(cfg.sessiondSock); {
		if time.Now().After(deadline) {
			select {
			case err := <-errs:
				t.Fatalf("sessiond exited: %v", err)
			default:
				t.Fatal("sessiond did not start")
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	id := "fedcba9876543210fedcba9876543210"
	sock := sessionSockPath(sessionDir, id)
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- runSessionWorker([]string{id, sock, cfg.root, cfg.workTmp, sessionDir, "80", "24", "1"})
	}()
	for deadline := time.Now().Add(3 * time.Second); !pingWorker(sock); {
		select {
		case err := <-workerDone:
			t.Fatalf("worker exited: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	conn, enc, dec, err := dialDaemon(cfg.sessiondSock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := enc.Encode(daemonMessage{Type: "attach", ID: id, Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	var msg workerMessage
	if err := dec.Decode(&msg); err != nil || msg.Type != "history" {
		t.Fatalf("history reply = %#v, %v", msg, err)
	}
	if err := enc.Encode(workerMessage{Type: "input", Data: "echo sessiond-ok\r"}); err != nil {
		t.Fatal(err)
	}
	output := ""
	for !strings.Contains(output, "sessiond-ok") {
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == "output" {
			output += msg.Data
		}
	}
	_ = enc.Encode(workerMessage{Type: "close"})
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ww-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
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
