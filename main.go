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
	host      string
	port      int
	root      string
	maxUpload int64
	titleFile string
	workTmp   string
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
	Cmd          *exec.Cmd
	PTY          *os.File
	Viewer       *client
	History      []string
	HistoryBytes int
	CwdKey       string
	CwdStop      chan struct{}
	CreatedAt    int64
	LastActive   int64
}

type sessionInfo struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	CreatedAt  int64  `json:"createdAt"`
	LastActive int64  `json:"lastActive"`
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
	app, err := newApp()
	if err != nil {
		log.Fatal(err)
	}
	if err := app.serve(); err != nil {
		log.Fatal(err)
	}
}

func newApp() (*appState, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	root := getenv("WEB_WORKER_ROOT", cwd)
	guard, err := newPathGuard(root)
	if err != nil {
		return nil, err
	}
	cfg := config{
		host:      getenv("HOST", "127.0.0.1"),
		port:      intEnv("PORT", 8787),
		root:      guard.root,
		maxUpload: int64(intEnv("WEB_WORKER_MAX_UPLOAD_MB", 100)) * 1024 * 1024,
		titleFile: filepath.Join(cwd, "session-titles.json"),
		workTmp:   filepath.Join(guard.root, ".web-worker-tmp"),
	}
	if err := os.MkdirAll(cfg.workTmp, 0o700); err != nil {
		return nil, err
	}
	_ = os.Setenv("TMPDIR", cfg.workTmp)
	_ = os.Setenv("TMP", cfg.workTmp)
	_ = os.Setenv("TEMP", cfg.workTmp)

	app := &appState{cfg: cfg, guard: guard, sessions: map[string]*session{}, clients: map[*client]bool{}, titles: map[string]string{}}
	app.loadTitles()
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
	if s.Cmd != nil && s.PTY != nil {
		old := s.Viewer
		if s.CwdStop != nil {
			close(s.CwdStop)
		}
		stop := make(chan struct{})
		ptmx := s.PTY
		s.Viewer = c
		s.LastActive = time.Now().UnixMilli()
		s.CwdStop = stop
		s.CwdKey = ""
		a.mu.Unlock()
		_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
		if old != nil && old != c {
			old.emit("terminal:detached", map[string]any{"id": s.ID})
		}
		go a.startCwdWatch(s.ID, c, stop)
		return nil
	}
	a.mu.Unlock()

	shell := getenv("SHELL", "/bin/bash")
	cmd := exec.Command(shell)
	cmd.Dir = a.cfg.root
	cmd.Env = shellEnv(a.cfg.workTmp, s.ID)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return err
	}

	a.mu.Lock()
	if s.Viewer != nil {
		old := a.detachViewerLocked(s, s.Viewer != c)
		if old != nil {
			old.emit("terminal:detached", map[string]any{"id": s.ID})
		}
	}
	s.History = nil
	s.HistoryBytes = 0
	s.Cmd = cmd
	s.PTY = ptmx
	s.Viewer = c
	s.LastActive = time.Now().UnixMilli()
	_ = quiet
	stop := make(chan struct{})
	s.CwdStop = stop
	s.CwdKey = ""
	a.mu.Unlock()

	go a.readPTY(s.ID, cmd, ptmx)
	go a.startCwdWatch(s.ID, c, stop)
	return nil
}

func (a *appState) releaseViewerLocked(s *session) {
	if s.CwdStop != nil {
		close(s.CwdStop)
		s.CwdStop = nil
	}
	s.Viewer = nil
	s.CwdKey = ""
}

func (a *appState) detachViewerLocked(s *session, notify bool) *client {
	viewer := s.Viewer
	if s.CwdStop != nil {
		close(s.CwdStop)
		s.CwdStop = nil
	}
	if s.PTY != nil {
		_ = s.PTY.Close()
		s.PTY = nil
	}
	if s.Cmd != nil && s.Cmd.Process != nil {
		_ = s.Cmd.Process.Kill()
	}
	s.Cmd = nil
	s.Viewer = nil
	s.CwdKey = ""
	if notify {
		return viewer
	}
	return nil
}

func (a *appState) readPTY(id string, cmd *exec.Cmd, ptmx *os.File) {
	buf := make([]byte, 8192)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			data := string(buf[:n])
			var viewer *client
			a.mu.Lock()
			if s := a.sessions[id]; s != nil && s.Cmd == cmd {
				s.LastActive = time.Now().UnixMilli()
				a.rememberLocked(s, data)
				viewer = s.Viewer
			}
			a.mu.Unlock()
			if viewer != nil {
				viewer.emit("terminal:data", map[string]any{"id": id, "data": data})
			}
		}
		if err != nil {
			break
		}
	}
	exitCode := 0
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = 1
		}
	}
	a.finishPTY(id, cmd, exitCode)
}

func (a *appState) finishPTY(id string, cmd *exec.Cmd, exitCode int) {
	var viewer *client
	a.mu.Lock()
	s := a.sessions[id]
	if s == nil || s.Cmd != cmd {
		a.mu.Unlock()
		return
	}
	viewer = s.Viewer
	if s.CwdStop != nil {
		close(s.CwdStop)
	}
	s.CwdStop = nil
	s.Cmd = nil
	s.PTY = nil
	s.Viewer = nil
	a.rememberLocked(s, fmt.Sprintf("\r\n[process exited: %d]\r\n", exitCode))
	delete(a.sessions, id)
	a.mu.Unlock()
	if viewer != nil {
		viewer.emit("terminal:exit", map[string]any{"id": id, "exitCode": exitCode})
	}
	a.broadcastSessions()
}

type cwdInfo struct {
	Path    string `json:"path"`
	Outside bool   `json:"outside"`
	Key     string `json:"-"`
}

func (a *appState) readSessionCwd(id string) *cwdInfo {
	a.mu.Lock()
	s := a.sessions[id]
	pid := 0
	if s != nil && s.Cmd != nil && s.Cmd.Process != nil {
		pid = s.Cmd.Process.Pid
	}
	a.mu.Unlock()
	if pid == 0 {
		return nil
	}
	cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil || cwd == "" {
		return nil
	}
	full, err := filepath.Abs(cwd)
	if err != nil {
		return nil
	}
	if !isInside(a.cfg.root, full) {
		return &cwdInfo{Path: "", Outside: true, Key: "outside:" + full}
	}
	return &cwdInfo{Path: a.guard.rel(full), Outside: false, Key: full}
}

func (a *appState) startCwdWatch(id string, c *client, stop <-chan struct{}) {
	a.pushSessionCwd(id, c, true)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			a.pushSessionCwd(id, c, false)
		}
	}
}

func (a *appState) pushSessionCwd(id string, c *client, force bool) {
	cwd := a.readSessionCwd(id)
	if cwd == nil {
		return
	}
	a.mu.Lock()
	s := a.sessions[id]
	if s == nil || s.Viewer != c || (!force && s.CwdKey == cwd.Key) {
		a.mu.Unlock()
		return
	}
	s.CwdKey = cwd.Key
	a.mu.Unlock()
	c.emit("terminal:cwd", map[string]any{"id": id, "path": cwd.Path, "outside": cwd.Outside})
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
	ptmx := (*os.File)(nil)
	if s != nil && s.Viewer == c {
		ptmx = s.PTY
	}
	a.mu.Unlock()
	if ptmx != nil && data != "" {
		_, _ = ptmx.Write([]byte(data))
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
	ptmx := (*os.File)(nil)
	if s != nil && s.Viewer == c {
		ptmx = s.PTY
	}
	a.mu.Unlock()
	if ptmx != nil {
		_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
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
	a.detachViewerLocked(s, false)
	delete(a.sessions, id)
	if a.titles[id] != "" {
		delete(a.titles, id)
		a.saveTitlesLocked()
	}
	a.mu.Unlock()
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
