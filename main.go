package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

//go:embed public
var embedded embed.FS

const (
	historyLimit = 1024 * 1024
)

var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type config struct {
	host         string
	port         int
	root         string
	maxUpload    int64
	titleFile    string
	workTmp      string
	sessionDir   string
	sessiondSock string
}

type appState struct {
	cfg      config
	guard    *pathGuard
	mu       sync.Mutex
	sessions map[string]*session
	clients  map[*client]bool
	titles   map[string]string
}

type session struct {
	ID           string
	Title        string
	Worker       net.Conn
	WorkerMu     sync.Mutex
	SockPath     string
	Viewer       *client
	History      []string
	HistoryBytes int
	CreatedAt    int64
	LastActive   int64
}

type sessionInfo struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	CreatedAt  int64  `json:"createdAt"`
	LastActive int64  `json:"lastActive"`
}

type sessionMeta struct {
	ID         string `json:"id"`
	CreatedAt  int64  `json:"createdAt"`
	LastActive int64  `json:"lastActive"`
}

type workerMessage struct {
	Type       string   `json:"type"`
	Data       string   `json:"data,omitempty"`
	Cols       int      `json:"cols,omitempty"`
	Rows       int      `json:"rows,omitempty"`
	History    []string `json:"history,omitempty"`
	ExitCode   int      `json:"exitCode,omitempty"`
	Path       string   `json:"path,omitempty"`
	Outside    bool     `json:"outside,omitempty"`
	CreatedAt  int64    `json:"createdAt,omitempty"`
	LastActive int64    `json:"lastActive,omitempty"`
}

type daemonMessage struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	Error     string `json:"error,omitempty"`
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
	CreatedAt int64  `json:"createdAt,omitempty"`
}

type fileEntry struct {
	Name  string  `json:"name"`
	Path  string  `json:"path"`
	Type  string  `json:"type"`
	Size  *int64  `json:"size,omitempty"`
	Mtime float64 `json:"mtime"`
}

type httpError struct {
	Status  int
	Message string
}

func (e *httpError) Error() string { return e.Message }

type pathGuard struct{ root string }

type guardedPath struct {
	full string
	real string
	rel  string
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "session-worker":
			if err := runSessionWorker(os.Args[2:]); err != nil {
				log.Fatal(err)
			}
			return
		case "sessiond":
			cfg, err := newConfig()
			if err != nil {
				log.Fatal(err)
			}
			if err := runSessionDaemon(cfg); err != nil {
				log.Fatal(err)
			}
			return
		default:
			log.Fatalf("unknown command: %s", os.Args[1])
		}
	}
	app, err := newApp()
	if err != nil {
		log.Fatal(err)
	}
	if err := app.serve(); err != nil {
		log.Fatal(err)
	}
}

func newConfig() (config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return config{}, err
	}
	root := getenv("WEB_WORKER_ROOT", cwd)
	guard, err := newPathGuard(root)
	if err != nil {
		return config{}, err
	}
	cfg := config{
		host:         getenv("HOST", "127.0.0.1"),
		port:         intEnv("PORT", 8787),
		root:         guard.root,
		maxUpload:    int64(intEnv("WEB_WORKER_MAX_UPLOAD_MB", 100)) * 1024 * 1024,
		titleFile:    filepath.Join(cwd, "session-titles.json"),
		workTmp:      filepath.Join(guard.root, ".web-worker-tmp"),
		sessionDir:   filepath.Join(cwd, ".web-worker-sessions"),
		sessiondSock: filepath.Join(cwd, ".web-worker-sessions", "sessiond.sock"),
	}
	if err := os.MkdirAll(cfg.workTmp, 0o700); err != nil {
		return config{}, err
	}
	if err := os.MkdirAll(cfg.sessionDir, 0o700); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func newApp() (*appState, error) {
	cfg, err := newConfig()
	if err != nil {
		return nil, err
	}
	guard, err := newPathGuard(cfg.root)
	if err != nil {
		return nil, err
	}
	_ = os.Setenv("TMPDIR", cfg.workTmp)
	_ = os.Setenv("TMP", cfg.workTmp)
	_ = os.Setenv("TEMP", cfg.workTmp)

	app := &appState{cfg: cfg, guard: guard, sessions: map[string]*session{}, clients: map[*client]bool{}, titles: map[string]string{}}
	app.loadTitles()
	app.loadWorkers()
	return app, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func intEnv(key string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func (a *appState) serve() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/me", a.wrap(a.handleMe))
	mux.HandleFunc("/api/tree", a.wrap(a.handleTree))
	mux.HandleFunc("/api/file", a.wrap(a.handleFile))
	mux.HandleFunc("/ws", a.handleWS)

	public, err := fs.Sub(embedded, "public")
	if err != nil {
		return err
	}
	files := http.FileServer(http.FS(public))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		setHeaders(w)
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})

	addr := fmt.Sprintf("%s:%d", a.cfg.host, a.cfg.port)
	log.Printf("Web worker shell: http://%s/", addr)
	log.Printf("Workspace root: %s", a.cfg.root)
	return http.ListenAndServe(addr, mux)
}

func setHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' ws: wss:; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' blob: data:; object-src 'none'; base-uri 'none'")
}

func (a *appState) wrap(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeaders(w)
		if err := fn(w, r); err != nil {
			writeError(w, err)
		}
	}
}

func writeError(w http.ResponseWriter, err error) {
	status, msg := http.StatusInternalServerError, "Internal server error"
	var he *httpError
	var tooBig *http.MaxBytesError
	if errors.As(err, &he) {
		status, msg = he.Status, he.Message
	} else if errors.As(err, &tooBig) {
		status, msg = http.StatusRequestEntityTooLarge, "Upload too large"
	} else {
		log.Println(err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func newPathGuard(rootDir string) (*pathGuard, error) {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	return &pathGuard{root: real}, nil
}

func isInside(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel))
}

func (g *pathGuard) lexical(userPath string) (string, error) {
	if userPath == "" {
		userPath = "."
	}
	var full string
	if filepath.IsAbs(userPath) {
		full = filepath.Clean(userPath)
	} else {
		full = filepath.Clean(filepath.Join(g.root, userPath))
	}
	abs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if !isInside(g.root, abs) {
		return "", &httpError{http.StatusForbidden, "Path escapes workspace root"}
	}
	return abs, nil
}

func (g *pathGuard) rel(full string) string {
	rel, err := filepath.Rel(g.root, full)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

func (g *pathGuard) existing(userPath string) (guardedPath, error) {
	full, err := g.lexical(userPath)
	if err != nil {
		return guardedPath{}, err
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		if os.IsNotExist(err) {
			return guardedPath{}, &httpError{http.StatusNotFound, "Path not found"}
		}
		return guardedPath{}, err
	}
	if !isInside(g.root, real) {
		return guardedPath{}, &httpError{http.StatusForbidden, "Path escapes workspace root"}
	}
	return guardedPath{full: full, real: real, rel: g.rel(full)}, nil
}

func (g *pathGuard) writable(userPath string) (guardedPath, error) {
	full, err := g.lexical(userPath)
	if err != nil {
		return guardedPath{}, err
	}
	parentReal, err := filepath.EvalSymlinks(filepath.Dir(full))
	if err != nil {
		if os.IsNotExist(err) {
			return guardedPath{}, &httpError{http.StatusNotFound, "Parent directory not found"}
		}
		return guardedPath{}, err
	}
	if !isInside(g.root, parentReal) {
		return guardedPath{}, &httpError{http.StatusForbidden, "Path escapes workspace root"}
	}
	return guardedPath{full: full, rel: g.rel(full)}, nil
}

func (a *appState) handleMe(w http.ResponseWriter, r *http.Request) error {
	return json.NewEncoder(w).Encode(map[string]any{"root": a.cfg.root, "maxUpload": a.cfg.maxUpload, "sessions": a.listSessions()})
}

func (a *appState) handleTree(w http.ResponseWriter, r *http.Request) error {
	dir, err := a.guard.existing(r.URL.Query().Get("path"))
	if err != nil {
		return err
	}
	st, err := os.Stat(dir.real)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return &httpError{http.StatusBadRequest, "Not a directory"}
	}
	items, err := os.ReadDir(dir.real)
	if err != nil {
		return err
	}
	rows := make([]fileEntry, 0, len(items))
	for _, item := range items {
		name := item.Name()
		rel := strings.Trim(strings.Join([]string{dir.rel, name}, "/"), "/")
		typ := "file"
		if item.IsDir() {
			typ = "dir"
		} else if item.Type()&os.ModeSymlink != 0 {
			typ = "link"
		}
		var size *int64
		var mtime float64
		if info, err := item.Info(); err == nil {
			mtime = float64(info.ModTime().UnixMilli())
			if typ == "file" {
				n := info.Size()
				size = &n
			}
		}
		rows = append(rows, fileEntry{Name: name, Path: rel, Type: typ, Size: size, Mtime: mtime})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Type == "dir" && rows[j].Type != "dir" {
			return true
		}
		if rows[i].Type != "dir" && rows[j].Type == "dir" {
			return false
		}
		return rows[i].Name < rows[j].Name
	})
	return json.NewEncoder(w).Encode(map[string]any{"path": dir.rel, "entries": rows})
}

func (a *appState) handleFile(w http.ResponseWriter, r *http.Request) error {
	switch r.Method {
	case http.MethodGet:
		file, err := a.guard.existing(r.URL.Query().Get("path"))
		if err != nil {
			return err
		}
		st, err := os.Stat(file.real)
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return &httpError{http.StatusBadRequest, "Not a file"}
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(file.full)))
		http.ServeFile(w, r, file.real)
		return nil
	case http.MethodPut:
		target, err := a.guard.writable(r.URL.Query().Get("path"))
		if err != nil {
			return err
		}
		if st, err := os.Lstat(target.full); err == nil && st.IsDir() {
			return &httpError{http.StatusBadRequest, "Cannot overwrite a directory"}
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		tmp := filepath.Join(filepath.Dir(target.full), fmt.Sprintf(".upload-%d-%d.tmp", os.Getpid(), time.Now().UnixNano()))
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer os.Remove(tmp)
		defer f.Close()
		_, err = io.Copy(f, http.MaxBytesReader(w, r.Body, a.cfg.maxUpload))
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := os.Rename(tmp, target.full); err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(map[string]any{"ok": true, "path": target.rel})
	default:
		return &httpError{http.StatusMethodNotAllowed, "Method not allowed"}
	}
}

func (a *appState) loadTitles() {
	b, err := os.ReadFile(a.cfg.titleFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Failed to read %s: %v", a.cfg.titleFile, err)
		}
		return
	}
	_ = json.Unmarshal(b, &a.titles)
}

func (a *appState) saveTitlesLocked() {
	if len(a.titles) == 0 {
		_ = os.Remove(a.cfg.titleFile)
		return
	}
	b, _ := json.MarshalIndent(a.titles, "", "  ")
	_ = os.WriteFile(a.cfg.titleFile, append(b, '\n'), 0o600)
}

func (a *appState) sessionSock(id string) string {
	return sessionSockPath(a.cfg.sessionDir, id)
}

func (a *appState) sessionMetaPath(id string) string {
	return sessionMetaPath(a.cfg.sessionDir, id)
}

func sessionSockPath(sessionDir, id string) string {
	return filepath.Join(sessionDir, id+".sock")
}

func sessionMetaPath(sessionDir, id string) string {
	return filepath.Join(sessionDir, id+".json")
}

func (a *appState) loadWorkers() {
	files, err := os.ReadDir(a.cfg.sessionDir)
	if err != nil {
		return
	}
	for _, file := range files {
		name := file.Name()
		if file.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		var meta sessionMeta
		b, err := os.ReadFile(filepath.Join(a.cfg.sessionDir, name))
		if err != nil || json.Unmarshal(b, &meta) != nil || !idPattern.MatchString(meta.ID) {
			continue
		}
		sock := a.sessionSock(meta.ID)
		if !pingWorker(sock) {
			_ = os.Remove(sock)
			_ = os.Remove(a.sessionMetaPath(meta.ID))
			continue
		}
		a.mu.Lock()
		s := a.ensureSessionLocked(meta.ID, meta.CreatedAt, meta.LastActive)
		s.SockPath = sock
		a.mu.Unlock()
	}
}

func dialWorker(sock string) (net.Conn, *json.Encoder, *json.Decoder, error) {
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return nil, nil, nil, err
	}
	return conn, json.NewEncoder(conn), json.NewDecoder(conn), nil
}

func pingWorker(sock string) bool {
	conn, enc, dec, err := dialWorker(sock)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if enc.Encode(workerMessage{Type: "ping"}) != nil {
		return false
	}
	var msg workerMessage
	return dec.Decode(&msg) == nil && msg.Type == "pong"
}

func sendWorker(conn net.Conn, msg workerMessage) error {
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	return json.NewEncoder(conn).Encode(msg)
}

func stopWorker(sock string) {
	conn, _, _, err := dialWorker(sock)
	if err != nil {
		return
	}
	_ = sendWorker(conn, workerMessage{Type: "close"})
	_ = conn.Close()
}

func dialDaemon(sock string) (net.Conn, *json.Encoder, *json.Decoder, error) {
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return nil, nil, nil, err
	}
	return conn, json.NewEncoder(conn), json.NewDecoder(conn), nil
}

func pingDaemon(sock string) bool {
	conn, enc, dec, err := dialDaemon(sock)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if enc.Encode(daemonMessage{Type: "ping"}) != nil {
		return false
	}
	var msg daemonMessage
	return dec.Decode(&msg) == nil && msg.Type == "pong"
}

func sendDaemonRequest(sock string, req daemonMessage) error {
	conn, enc, dec, err := dialDaemon(sock)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := enc.Encode(req); err != nil {
		return err
	}
	var msg daemonMessage
	if err := dec.Decode(&msg); err != nil {
		return err
	}
	if msg.Error != "" {
		return errors.New(msg.Error)
	}
	return nil
}

func requestSessionStart(cfg config, id string, cols, rows int, createdAt int64) error {
	return sendDaemonRequest(cfg.sessiondSock, daemonMessage{Type: "start", ID: id, Cols: cols, Rows: rows, CreatedAt: createdAt})
}

func requestSessionClose(cfg config, id string) error {
	return sendDaemonRequest(cfg.sessiondSock, daemonMessage{Type: "close", ID: id})
}

func defaultTitle(id string) string { return "shell " + id[:8] }

func cleanTitle(title string) string {
	t := strings.Join(strings.Fields(title), " ")
	r := []rune(t)
	if len(r) > 80 {
		r = r[:80]
	}
	return string(r)
}

func (a *appState) ensureSessionLocked(id string, createdAt, lastActive int64) *session {
	s := a.sessions[id]
	now := time.Now().UnixMilli()
	if s == nil {
		s = &session{ID: id, Title: a.titles[id], CreatedAt: now, LastActive: now}
		if s.Title == "" {
			s.Title = defaultTitle(id)
		}
		a.sessions[id] = s
	}
	if a.titles[id] != "" {
		s.Title = a.titles[id]
	}
	if createdAt != 0 {
		s.CreatedAt = createdAt
	}
	if lastActive != 0 {
		s.LastActive = lastActive
	}
	return s
}

func (a *appState) listSessions() []sessionInfo {
	a.mu.Lock()
	rows := make([]sessionInfo, 0, len(a.sessions))
	for _, s := range a.sessions {
		rows = append(rows, sessionInfo{ID: s.ID, Title: s.Title, CreatedAt: s.CreatedAt, LastActive: s.LastActive})
	}
	a.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].LastActive > rows[j].LastActive })
	return rows
}

func (a *appState) broadcastSessions() {
	rows := a.listSessions()
	a.mu.Lock()
	clients := make([]*client, 0, len(a.clients))
	for c := range a.clients {
		clients = append(clients, c)
	}
	a.mu.Unlock()
	for _, c := range clients {
		c.emit("sessions", rows)
	}
}

func (a *appState) rememberLocked(s *session, data string) {
	s.History = append(s.History, data)
	s.HistoryBytes += len([]byte(data))
	for s.HistoryBytes > historyLimit && len(s.History) > 0 {
		s.HistoryBytes -= len([]byte(s.History[0]))
		s.History = s.History[1:]
	}
}

func (a *appState) sessionSnapshot(id string) (sessionInfo, []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[id]
	if s == nil {
		return sessionInfo{}, nil
	}
	history := tailHistory(s.History, replayLimit)
	return sessionInfo{ID: s.ID, Title: s.Title, CreatedAt: s.CreatedAt, LastActive: s.LastActive}, history
}

func (a *appState) createSession() (*session, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	a.mu.Lock()
	s := a.ensureSessionLocked(id, now, now)
	a.mu.Unlock()
	a.broadcastSessions()
	return s, nil
}

func randomID() (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}

func (a *appState) attachSession(c *client, s *session, cols, rows int, quiet bool) error {
	if cols <= 0 {
		cols = 100
	}
	if rows <= 0 {
		rows = 30
	}

	a.mu.Lock()
	if s == nil || a.sessions[s.ID] != s {
		a.mu.Unlock()
		return errors.New("Session not found")
	}
	id := s.ID
	createdAt := s.CreatedAt
	sock := s.SockPath
	if sock == "" {
		sock = a.sessionSock(id)
		s.SockPath = sock
	}
	a.mu.Unlock()

	if !pingWorker(sock) {
		if err := requestSessionStart(a.cfg, id, cols, rows, createdAt); err != nil {
			return err
		}
	}
	conn, enc, dec, err := dialDaemon(a.cfg.sessiondSock)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if err := enc.Encode(daemonMessage{Type: "attach", ID: id, Cols: cols, Rows: rows}); err != nil {
		_ = conn.Close()
		return err
	}
	var hello workerMessage
	if err := dec.Decode(&hello); err != nil {
		_ = conn.Close()
		return err
	}
	if hello.Type == "error" {
		_ = conn.Close()
		return errors.New(hello.Data)
	}
	if hello.Type != "history" {
		_ = conn.Close()
		return fmt.Errorf("unexpected sessiond reply: %s", hello.Type)
	}
	_ = conn.SetDeadline(time.Time{})

	a.mu.Lock()
	if s == nil || a.sessions[id] != s {
		a.mu.Unlock()
		_ = conn.Close()
		return errors.New("Session not found")
	}
	oldViewer, oldWorker := s.Viewer, s.Worker
	s.History = append([]string(nil), hello.History...)
	s.HistoryBytes = 0
	for _, chunk := range s.History {
		s.HistoryBytes += len([]byte(chunk))
	}
	s.Worker = conn
	s.Viewer = c
	if hello.LastActive != 0 {
		s.LastActive = hello.LastActive
	} else {
		s.LastActive = time.Now().UnixMilli()
	}
	a.mu.Unlock()

	if oldWorker != nil && oldWorker != conn {
		_ = oldWorker.Close()
	}
	if oldViewer != nil && oldViewer != c {
		oldViewer.emit("terminal:detached", map[string]any{"id": id})
	}
	_ = quiet
	go a.readWorker(id, conn, dec)
	return nil
}

func startSessionWorker(cfg config, id string, cols, rows int, createdAt int64) error {
	if !idPattern.MatchString(id) {
		return errors.New("invalid session id")
	}
	sock := sessionSockPath(cfg.sessionDir, id)
	_ = os.Remove(sock)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "session-worker", id, sock, cfg.root, cfg.workTmp, cfg.sessionDir, strconv.Itoa(cols), strconv.Itoa(rows), strconv.FormatInt(createdAt, 10))
	cmd.Env = shellEnv(cfg.workTmp, id)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pingWorker(sock) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("session worker did not start")
}

func (a *appState) readWorker(id string, conn net.Conn, dec *json.Decoder) {
	for {
		var msg workerMessage
		if err := dec.Decode(&msg); err != nil {
			a.mu.Lock()
			if s := a.sessions[id]; s != nil && s.Worker == conn {
				s.Worker = nil
				s.Viewer = nil
			}
			a.mu.Unlock()
			return
		}
		switch msg.Type {
		case "output":
			var viewer *client
			a.mu.Lock()
			if s := a.sessions[id]; s != nil && s.Worker == conn {
				s.LastActive = time.Now().UnixMilli()
				a.rememberLocked(s, msg.Data)
				viewer = s.Viewer
			}
			a.mu.Unlock()
			if viewer != nil {
				viewer.emit("terminal:data", map[string]any{"id": id, "data": msg.Data})
			}
		case "cwd":
			var viewer *client
			a.mu.Lock()
			if s := a.sessions[id]; s != nil && s.Worker == conn {
				viewer = s.Viewer
			}
			a.mu.Unlock()
			if viewer != nil {
				viewer.emit("terminal:cwd", map[string]any{"id": id, "path": msg.Path, "outside": msg.Outside})
			}
		case "exit":
			a.finishWorker(id, conn, msg.ExitCode)
			return
		}
	}
}

func (a *appState) finishWorker(id string, conn net.Conn, exitCode int) {
	var viewer *client
	a.mu.Lock()
	s := a.sessions[id]
	if s == nil || s.Worker != conn {
		a.mu.Unlock()
		return
	}
	viewer = s.Viewer
	s.Worker = nil
	s.Viewer = nil
	a.rememberLocked(s, fmt.Sprintf("\r\n[process exited: %d]\r\n", exitCode))
	delete(a.sessions, id)
	a.mu.Unlock()
	_ = conn.Close()
	if viewer != nil {
		viewer.emit("terminal:exit", map[string]any{"id": id, "exitCode": exitCode})
	}
	a.broadcastSessions()
}

func (a *appState) releaseViewerLocked(s *session) {
	if s.Worker != nil {
		_ = s.Worker.Close()
		s.Worker = nil
	}
	s.Viewer = nil
}

type cwdInfo struct {
	Path    string `json:"path"`
	Outside bool   `json:"outside"`
	Key     string `json:"-"`
}

func runSessionDaemon(cfg config) error {
	_ = os.Remove(cfg.sessiondSock)
	listener, err := net.Listen("unix", cfg.sessiondSock)
	if err != nil {
		return err
	}
	defer listener.Close()
	_ = os.Chmod(cfg.sessiondSock, 0o600)
	log.Printf("Web worker sessiond: %s", cfg.sessiondSock)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go handleSessionDaemonConn(cfg, conn)
	}
}

func handleSessionDaemonConn(cfg config, conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	var msg daemonMessage
	if dec.Decode(&msg) != nil {
		return
	}
	switch msg.Type {
	case "ping":
		_ = enc.Encode(daemonMessage{Type: "pong"})
	case "start":
		err := startSessionWorker(cfg, msg.ID, msg.Cols, msg.Rows, msg.CreatedAt)
		replyDaemon(enc, err)
	case "close":
		if idPattern.MatchString(msg.ID) {
			stopWorker(sessionSockPath(cfg.sessionDir, msg.ID))
			_ = os.Remove(sessionSockPath(cfg.sessionDir, msg.ID))
			_ = os.Remove(sessionMetaPath(cfg.sessionDir, msg.ID))
		}
		replyDaemon(enc, nil)
	case "attach":
		proxySessionAttach(cfg, conn, dec, enc, msg)
	default:
		replyDaemon(enc, fmt.Errorf("unknown sessiond request: %s", msg.Type))
	}
}

func replyDaemon(enc *json.Encoder, err error) {
	reply := daemonMessage{Type: "ok"}
	if err != nil {
		reply.Error = err.Error()
	}
	_ = enc.Encode(reply)
}

func proxySessionAttach(cfg config, webConn net.Conn, webDec *json.Decoder, webEnc *json.Encoder, req daemonMessage) {
	if !idPattern.MatchString(req.ID) {
		_ = webEnc.Encode(workerMessage{Type: "error", Data: "Invalid session id"})
		return
	}
	workerConn, workerEnc, workerDec, err := dialWorker(sessionSockPath(cfg.sessionDir, req.ID))
	if err != nil {
		_ = webEnc.Encode(workerMessage{Type: "error", Data: err.Error()})
		return
	}
	defer workerConn.Close()
	if err := workerEnc.Encode(workerMessage{Type: "attach", Cols: req.Cols, Rows: req.Rows}); err != nil {
		_ = webEnc.Encode(workerMessage{Type: "error", Data: err.Error()})
		return
	}
	var hello workerMessage
	if err := workerDec.Decode(&hello); err != nil {
		_ = webEnc.Encode(workerMessage{Type: "error", Data: err.Error()})
		return
	}
	if err := webEnc.Encode(hello); err != nil || hello.Type != "history" {
		return
	}

	done := make(chan struct{}, 2)
	go relayWorkerMessages(webDec, workerEnc, done)
	go relayWorkerMessages(workerDec, webEnc, done)
	<-done
}

func relayWorkerMessages(dec *json.Decoder, enc *json.Encoder, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	for {
		var msg workerMessage
		if err := dec.Decode(&msg); err != nil {
			return
		}
		if err := enc.Encode(msg); err != nil {
			return
		}
		if msg.Type == "close" || msg.Type == "exit" {
			return
		}
	}
}

type sessionWorker struct {
	id         string
	sock       string
	root       string
	workTmp    string
	sessionDir string
	createdAt  int64
	lastActive int64
	metaSaved  int64
	cmd        *exec.Cmd
	ptmx       *os.File
	listener   net.Listener
	done       chan struct{}
	once       sync.Once

	mu           sync.Mutex
	client       net.Conn
	enc          *json.Encoder
	history      []string
	historyBytes int
	cwdKey       string
}

func runSessionWorker(args []string) error {
	if len(args) != 8 {
		return errors.New("bad session worker args")
	}
	cols, _ := strconv.Atoi(args[5])
	rows, _ := strconv.Atoi(args[6])
	createdAt, _ := strconv.ParseInt(args[7], 10, 64)
	if cols <= 0 {
		cols = 100
	}
	if rows <= 0 {
		rows = 30
	}
	if createdAt == 0 {
		createdAt = time.Now().UnixMilli()
	}
	w := &sessionWorker{
		id:         args[0],
		sock:       args[1],
		root:       args[2],
		workTmp:    args[3],
		sessionDir: args[4],
		createdAt:  createdAt,
		lastActive: createdAt,
		done:       make(chan struct{}),
	}
	return w.run(cols, rows)
}

func (w *sessionWorker) run(cols, rows int) error {
	if !idPattern.MatchString(w.id) {
		return errors.New("invalid session id")
	}
	if err := os.MkdirAll(w.sessionDir, 0o700); err != nil {
		return err
	}
	shell := getenv("SHELL", "/bin/bash")
	cmd := exec.Command(shell)
	cmd.Dir = w.root
	cmd.Env = shellEnv(w.workTmp, w.id)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return err
	}
	w.cmd, w.ptmx = cmd, ptmx

	_ = os.Remove(w.sock)
	listener, err := net.Listen("unix", w.sock)
	if err != nil {
		_ = ptmx.Close()
		_ = cmd.Process.Kill()
		return err
	}
	w.listener = listener
	_ = os.Chmod(w.sock, 0o600)
	w.saveMeta()

	go w.readPTY()
	go w.cwdLoop()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			select {
			case <-w.done:
				return nil
			default:
				return err
			}
		}
		go w.handleConn(conn)
	}
}

func (w *sessionWorker) metaPath() string {
	return filepath.Join(w.sessionDir, w.id+".json")
}

func (w *sessionWorker) saveMeta() {
	meta := sessionMeta{ID: w.id, CreatedAt: w.createdAt, LastActive: w.lastActive}
	b, _ := json.MarshalIndent(meta, "", "  ")
	_ = os.WriteFile(w.metaPath(), append(b, '\n'), 0o600)
	w.metaSaved = w.lastActive
}

func (w *sessionWorker) touchLocked() {
	w.lastActive = time.Now().UnixMilli()
	if w.lastActive-w.metaSaved > 10_000 {
		w.saveMeta()
	}
}

func (w *sessionWorker) handleConn(conn net.Conn) {
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	var msg workerMessage
	if dec.Decode(&msg) != nil {
		_ = conn.Close()
		return
	}
	switch msg.Type {
	case "ping":
		_ = enc.Encode(workerMessage{Type: "pong"})
		_ = conn.Close()
	case "close":
		w.kill()
		_ = conn.Close()
	case "attach":
		w.attachConn(conn, enc, msg.Cols, msg.Rows)
		defer func() {
			w.mu.Lock()
			if w.client == conn {
				w.client = nil
				w.enc = nil
			}
			w.mu.Unlock()
			_ = conn.Close()
		}()
		for dec.Decode(&msg) == nil {
			switch msg.Type {
			case "input":
				w.input(msg.Data)
			case "resize":
				w.resize(msg.Cols, msg.Rows)
			case "close":
				w.kill()
				return
			}
		}
	default:
		_ = conn.Close()
	}
}

func (w *sessionWorker) attachConn(conn net.Conn, enc *json.Encoder, cols, rows int) {
	w.mu.Lock()
	old := w.client
	w.client = conn
	w.enc = enc
	w.cwdKey = ""
	history := tailHistory(w.history, replayLimit)
	lastActive := w.lastActive
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = enc.Encode(workerMessage{Type: "history", History: history, CreatedAt: w.createdAt, LastActive: lastActive})
	w.mu.Unlock()
	if old != nil && old != conn {
		_ = old.Close()
	}
	w.resize(cols, rows)
	w.pushCwd(true)
}

func (w *sessionWorker) input(data string) {
	if data == "" {
		return
	}
	w.mu.Lock()
	w.touchLocked()
	ptmx := w.ptmx
	w.mu.Unlock()
	if ptmx != nil {
		_, _ = ptmx.Write([]byte(data))
	}
}

func (w *sessionWorker) resize(cols, rows int) {
	if cols <= 0 {
		cols = 100
	}
	if rows <= 0 {
		rows = 30
	}
	w.mu.Lock()
	ptmx := w.ptmx
	w.mu.Unlock()
	if ptmx != nil {
		_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	}
}

func (w *sessionWorker) readPTY() {
	buf := make([]byte, 8192)
	for {
		n, err := w.ptmx.Read(buf)
		if n > 0 {
			data := string(buf[:n])
			w.mu.Lock()
			w.history = append(w.history, data)
			w.historyBytes += len([]byte(data))
			for w.historyBytes > historyLimit && len(w.history) > 0 {
				w.historyBytes -= len([]byte(w.history[0]))
				w.history = w.history[1:]
			}
			w.touchLocked()
			w.mu.Unlock()
			w.send(workerMessage{Type: "output", Data: data})
		}
		if err != nil {
			break
		}
	}
	exitCode := 0
	if err := w.cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = 1
		}
	}
	w.finish(exitCode)
}

func (w *sessionWorker) send(msg workerMessage) {
	w.mu.Lock()
	conn, enc := w.client, w.enc
	if conn == nil || enc == nil {
		w.mu.Unlock()
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	if err := enc.Encode(msg); err != nil {
		if w.client == conn {
			w.client = nil
			w.enc = nil
		}
		_ = conn.Close()
	}
	w.mu.Unlock()
}

func (w *sessionWorker) kill() {
	w.mu.Lock()
	ptmx, cmd := w.ptmx, w.cmd
	w.mu.Unlock()
	if ptmx != nil {
		_ = ptmx.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (w *sessionWorker) finish(exitCode int) {
	w.once.Do(func() {
		w.send(workerMessage{Type: "exit", ExitCode: exitCode})
		w.mu.Lock()
		if w.client != nil {
			_ = w.client.Close()
			w.client = nil
			w.enc = nil
		}
		w.mu.Unlock()
		if w.listener != nil {
			_ = w.listener.Close()
		}
		_ = os.Remove(w.sock)
		_ = os.Remove(w.metaPath())
		close(w.done)
	})
}

func (w *sessionWorker) cwdLoop() {
	w.pushCwd(true)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			w.pushCwd(false)
		}
	}
}

func (w *sessionWorker) pushCwd(force bool) {
	cwd := workerCwd(w.root, w.cmd)
	if cwd == nil {
		return
	}
	w.mu.Lock()
	if !force && w.cwdKey == cwd.Key {
		w.mu.Unlock()
		return
	}
	w.cwdKey = cwd.Key
	w.mu.Unlock()
	w.send(workerMessage{Type: "cwd", Path: cwd.Path, Outside: cwd.Outside})
}

func workerCwd(root string, cmd *exec.Cmd) *cwdInfo {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", cmd.Process.Pid))
	if err != nil || cwd == "" {
		return nil
	}
	full, err := filepath.Abs(cwd)
	if err != nil {
		return nil
	}
	if !isInside(root, full) {
		return &cwdInfo{Path: "", Outside: true, Key: "outside:" + full}
	}
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == "." {
		return &cwdInfo{Path: "", Key: full}
	}
	return &cwdInfo{Path: filepath.ToSlash(rel), Key: full}
}

type client struct {
	app  *appState
	conn *websocket.Conn
	send chan []byte
	done chan struct{}
	once sync.Once
}

type inbound struct {
	Type string          `json:"type"`
	Req  string          `json:"req,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

type outbound struct {
	Type string `json:"type"`
	Req  string `json:"req,omitempty"`
	Data any    `json:"data,omitempty"`
}

var upgrader = websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 8192}

func (a *appState) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &client{app: a, conn: conn, send: make(chan []byte, 64), done: make(chan struct{})}
	a.mu.Lock()
	a.clients[c] = true
	a.mu.Unlock()
	go c.writeLoop()
	c.emit("sessions", a.listSessions())
	c.readLoop()
	c.cleanup()
}

func (c *client) emit(typ string, data any) {
	b, err := json.Marshal(outbound{Type: typ, Data: data})
	if err != nil {
		return
	}
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.send <- b:
	case <-c.done:
	default:
	}
}

func (c *client) reply(req string, data any) {
	if req == "" {
		return
	}
	b, err := json.Marshal(outbound{Type: "reply", Req: req, Data: data})
	if err != nil {
		return
	}
	select {
	case c.send <- b:
	case <-c.done:
	default:
	}
}

func (c *client) writeLoop() {
	for {
		select {
		case b := <-c.send:
			if err := c.conn.WriteMessage(websocket.TextMessage, b); err != nil {
				c.close()
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *client) readLoop() {
	for {
		_, b, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg inbound
		if json.Unmarshal(b, &msg) == nil {
			c.handle(msg)
		}
	}
}

func (c *client) cleanup() {
	c.close()
	a := c.app
	a.mu.Lock()
	delete(a.clients, c)
	for _, s := range a.sessions {
		if s.Viewer == c {
			a.releaseViewerLocked(s)
		}
	}
	a.mu.Unlock()
}

func (c *client) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

func (c *client) handle(msg inbound) {
	switch msg.Type {
	case "session:create":
		var p struct{ Cols, Rows int }
		_ = json.Unmarshal(msg.Data, &p)
		s, err := c.app.createSession()
		if err != nil {
			c.reply(msg.Req, map[string]any{"error": err.Error()})
			return
		}
		if err := c.app.attachSession(c, s, p.Cols, p.Rows, false); err != nil {
			c.app.closeSession(s.ID)
			c.reply(msg.Req, map[string]any{"error": err.Error()})
			return
		}
		info, history := c.app.sessionSnapshot(s.ID)
		c.reply(msg.Req, map[string]any{"session": info, "history": history})
	case "session:attach":
		var p struct {
			ID   string                   `json:"id"`
			Size struct{ Cols, Rows int } `json:"size"`
		}
		_ = json.Unmarshal(msg.Data, &p)
		if !idPattern.MatchString(p.ID) {
			c.reply(msg.Req, map[string]any{"error": "Invalid session id"})
			return
		}
		c.app.mu.Lock()
		s := c.app.sessions[p.ID]
		c.app.mu.Unlock()
		if s == nil {
			c.reply(msg.Req, map[string]any{"error": "Session not found"})
			return
		}
		if err := c.app.attachSession(c, s, p.Size.Cols, p.Size.Rows, true); err != nil {
			c.reply(msg.Req, map[string]any{"error": err.Error()})
			return
		}
		info, history := c.app.sessionSnapshot(s.ID)
		c.reply(msg.Req, map[string]any{"session": info, "history": history})
	case "session:close":
		var id string
		_ = json.Unmarshal(msg.Data, &id)
		ok := c.app.closeSession(id)
		c.reply(msg.Req, map[string]any{"ok": ok})
	case "session:rename":
		var p struct{ ID, Title string }
		_ = json.Unmarshal(msg.Data, &p)
		c.reply(msg.Req, c.app.renameSession(p.ID, p.Title))
	case "terminal:input":
		var p struct{ ID, Data string }
		_ = json.Unmarshal(msg.Data, &p)
		c.app.writeTerminal(c, p.ID, p.Data)
	case "terminal:resize":
		var p struct {
			ID         string
			Cols, Rows int
		}
		_ = json.Unmarshal(msg.Data, &p)
		c.app.resizeTerminal(c, p.ID, p.Cols, p.Rows)
	}
}

func (a *appState) ownsSession(c *client, id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[id]
	return s != nil && s.Viewer == c
}

func (a *appState) writeTerminal(c *client, id, data string) {
	a.mu.Lock()
	s := a.sessions[id]
	var conn net.Conn
	if s != nil && s.Viewer == c {
		conn = s.Worker
	}
	a.mu.Unlock()
	if conn != nil && data != "" {
		s.WorkerMu.Lock()
		_ = sendWorker(conn, workerMessage{Type: "input", Data: data})
		s.WorkerMu.Unlock()
	}
}

func (a *appState) resizeTerminal(c *client, id string, cols, rows int) {
	if cols <= 0 {
		cols = 100
	}
	if rows <= 0 {
		rows = 30
	}
	a.mu.Lock()
	s := a.sessions[id]
	var conn net.Conn
	if s != nil && s.Viewer == c {
		conn = s.Worker
	}
	a.mu.Unlock()
	if conn != nil {
		s.WorkerMu.Lock()
		_ = sendWorker(conn, workerMessage{Type: "resize", Cols: cols, Rows: rows})
		s.WorkerMu.Unlock()
	}
}

func (a *appState) closeSession(id string) bool {
	if !idPattern.MatchString(id) {
		return false
	}
	a.mu.Lock()
	s := a.sessions[id]
	if s == nil {
		a.mu.Unlock()
		return false
	}
	viewer := s.Viewer
	sock := s.SockPath
	if s.Worker != nil {
		_ = s.Worker.Close()
		s.Worker = nil
	}
	s.Viewer = nil
	delete(a.sessions, id)
	if a.titles[id] != "" {
		delete(a.titles, id)
		a.saveTitlesLocked()
	}
	a.mu.Unlock()
	if sock != "" {
		if err := requestSessionClose(a.cfg, id); err != nil {
			stopWorker(sock)
		}
		_ = os.Remove(sock)
		_ = os.Remove(a.sessionMetaPath(id))
	}
	if viewer != nil {
		viewer.emit("terminal:exit", map[string]any{"id": id, "exitCode": nil})
	}
	a.broadcastSessions()
	return true
}

func (a *appState) renameSession(id, title string) map[string]any {
	if !idPattern.MatchString(id) {
		return map[string]any{"ok": false, "error": "Invalid session id"}
	}
	title = cleanTitle(title)
	if title == "" {
		return map[string]any{"ok": false, "error": "Name is required"}
	}
	a.mu.Lock()
	s := a.sessions[id]
	if s == nil {
		a.mu.Unlock()
		return map[string]any{"ok": false, "error": "Session not found"}
	}
	s.Title = title
	a.titles[id] = title
	a.saveTitlesLocked()
	info := sessionInfo{ID: s.ID, Title: s.Title, CreatedAt: s.CreatedAt, LastActive: s.LastActive}
	a.mu.Unlock()
	a.broadcastSessions()
	return map[string]any{"ok": true, "session": info}
}

// ponytail: cap replay so busy shells switch fast; add paging only if full scrollback is needed.
const replayLimit = 256 * 1024

func tailHistory(history []string, limit int) []string {
	if len(history) == 0 || limit <= 0 {
		return nil
	}
	start, bytes := len(history), 0
	for start > 0 {
		size := len(history[start-1])
		if bytes > 0 && bytes+size > limit {
			break
		}
		bytes += size
		start--
		if bytes >= limit {
			break
		}
	}
	return append([]string(nil), history[start:]...)
}

func shellEnv(workTmp, sessionID string) []string {
	env := os.Environ()
	if os.Getenv("HOME") == "" {
		home := ""
		if current, err := user.Current(); err == nil {
			home = current.HomeDir
		}
		if home == "" && os.Getuid() == 0 {
			home = "/root"
		}
		if home != "" {
			env = append(env, "HOME="+home)
		}
	}
	return append(env, "TERM=xterm-256color", "TMPDIR="+workTmp, "TMP="+workTmp, "TEMP="+workTmp, "WEB_WORKER_SESSION="+sessionID)
}
