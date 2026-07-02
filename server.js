import crypto from 'node:crypto';
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import fsp from 'node:fs/promises';
import http from 'node:http';
import path from 'node:path';
import { Transform } from 'node:stream';
import { pipeline } from 'node:stream/promises';
import { fileURLToPath } from 'node:url';

import express from 'express';
import pty from 'node-pty';
import { Server } from 'socket.io';

import { HttpError, isInside, makePathGuard } from './src/security.js';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const HOST = process.env.HOST || '127.0.0.1';
const PORT = Number(process.env.PORT || 8787);
const ROOT = process.env.WEB_WORKER_ROOT || process.cwd();
const TOKEN = process.env.WEB_WORKER_TOKEN || crypto.randomBytes(24).toString('base64url');
const MAX_UPLOAD = Number(process.env.WEB_WORKER_MAX_UPLOAD_MB || 100) * 1024 * 1024;
const HISTORY_LIMIT = 1024 * 1024;
const TMUX_SOCKET = process.env.WEB_WORKER_TMUX_SOCKET || 'web-worker-shell';
const TMUX_PREFIX = 'webworker_';
const TITLE_FILE = path.join(__dirname, 'session-titles.json');
const guard = makePathGuard(ROOT);
const sessions = new Map();
let sessionTitles = {};

if (!process.env.WEB_WORKER_TOKEN && !['127.0.0.1', '::1', 'localhost'].includes(HOST)) {
  console.error('Refusing remote bind without WEB_WORKER_TOKEN.');
  process.exit(1);
}

const app = express();
const server = http.createServer(app);
const io = new Server(server);

app.disable('x-powered-by');
app.use((req, res, next) => {
  res.setHeader('X-Content-Type-Options', 'nosniff');
  res.setHeader('Referrer-Policy', 'no-referrer');
  res.setHeader('Content-Security-Policy', "default-src 'self'; connect-src 'self' ws: wss:; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' blob: data:; object-src 'none'; base-uri 'none'");
  next();
});

app.use('/vendor/xterm', express.static(path.join(__dirname, 'node_modules/@xterm/xterm')));
app.use('/vendor/xterm-fit', express.static(path.join(__dirname, 'node_modules/@xterm/addon-fit/lib')));
app.use(express.static(path.join(__dirname, 'public'), {
  etag: false,
  maxAge: 0,
  setHeaders(res) {
    res.setHeader('Cache-Control', 'no-store');
  }
}));

function validToken(value) {
  if (typeof value !== 'string' || !value) return false;
  const got = Buffer.from(value);
  const want = Buffer.from(TOKEN);
  return got.length === want.length && crypto.timingSafeEqual(got, want);
}

function auth(req, res, next) {
  const header = req.get('authorization') || '';
  const bearer = header.match(/^Bearer\s+(.+)$/i)?.[1];
  const token = bearer || req.get('x-web-worker-token');
  if (!validToken(token)) return next(new HttpError(401, 'Unauthorized'));
  next();
}

function asyncRoute(fn) {
  return (req, res, next) => Promise.resolve(fn(req, res, next)).catch(next);
}

function loadTitles() {
  try {
    const parsed = JSON.parse(fs.readFileSync(TITLE_FILE, 'utf8'));
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) sessionTitles = parsed;
  } catch (err) {
    if (err.code !== 'ENOENT') console.error(`Failed to read ${TITLE_FILE}:`, err.message);
  }
}

function saveTitles() {
  if (!Object.keys(sessionTitles).length) {
    fs.rmSync(TITLE_FILE, { force: true });
    return;
  }
  fs.writeFileSync(TITLE_FILE, `${JSON.stringify(sessionTitles, null, 2)}\n`);
}

function defaultTitle(id) {
  return `tmux ${id.slice(0, 8)}`;
}

function cleanTitle(title) {
  return String(title || '').replace(/\s+/g, ' ').trim().slice(0, 80);
}

function listSessions() {
  syncTmuxSessions();
  return [...sessions.values()].map(({ id, title, createdAt, lastActive }) => ({
    id,
    title,
    createdAt,
    lastActive
  })).sort((a, b) => b.lastActive - a.lastActive);
}

function broadcastSessions() {
  io.emit('sessions', listSessions());
}

function remember(session, data) {
  session.history.push(data);
  session.historyBytes += Buffer.byteLength(data);
  while (session.historyBytes > HISTORY_LIMIT) {
    session.historyBytes -= Buffer.byteLength(session.history.shift() || '');
  }
}

function tmuxArgs(args) {
  return ['-L', TMUX_SOCKET, ...args];
}

function runTmux(args) {
  const res = spawnSync('tmux', tmuxArgs(args), { encoding: 'utf8' });
  if (res.status === 0) return res.stdout || '';
  throw new Error((res.stderr || res.stdout || 'tmux failed').trim());
}

function tryTmux(args) {
  const res = spawnSync('tmux', tmuxArgs(args), { encoding: 'utf8' });
  return res.status === 0 ? res.stdout || '' : '';
}

function tmuxName(id) {
  return `${TMUX_PREFIX}${id}`;
}

function idFromTmuxName(name) {
  if (!name.startsWith(TMUX_PREFIX)) return '';
  const id = name.slice(TMUX_PREFIX.length);
  return /^[a-f0-9]{32}$/i.test(id) ? id : '';
}

function tmuxHasSession(id) {
  return spawnSync('tmux', tmuxArgs(['has-session', '-t', tmuxName(id)])).status === 0;
}

function setTmuxDefaults() {
  tryTmux(['set-option', '-g', 'mouse', 'on']);
  tryTmux(['set-option', '-g', 'history-limit', '8000']);
}

function captureSessionHistory(id) {
  const out = tryTmux(['capture-pane', '-p', '-S', '-8000', '-E', '-', '-t', tmuxName(id)]);
  return out.replace(/(?:\n[ \t]*)+$/g, '').replace(/\n/g, '\r\n');
}

function readSessionCwd(id) {
  const cwd = tryTmux(['display-message', '-p', '-t', tmuxName(id), '#{pane_current_path}']).trim();
  if (!cwd) return null;
  const full = path.resolve(cwd);
  if (!isInside(guard.root, full)) return { path: '', outside: true, key: `outside:${full}` };
  return { path: guard.rel(full), outside: false, key: full };
}

function pushSessionCwd(session, force = false) {
  if (!session.viewerSocket) return;
  const cwd = readSessionCwd(session.id);
  if (!cwd || (!force && cwd.key === session.cwdKey)) return;
  session.cwdKey = cwd.key;
  io.to(session.viewerSocket).emit('terminal:cwd', { id: session.id, path: cwd.path, outside: cwd.outside });
}

function startCwdWatch(session) {
  if (session.cwdTimer) clearInterval(session.cwdTimer);
  pushSessionCwd(session, true);
  session.cwdTimer = setInterval(() => pushSessionCwd(session), 1000);
  session.cwdTimer.unref?.();
}

function ensureSession(id, meta = {}) {
  let session = sessions.get(id);
  if (!session) {
    session = {
      id,
      title: sessionTitles[id] || defaultTitle(id),
      term: null,
      viewerSocket: '',
      history: [],
      historyBytes: 0,
      cwdKey: '',
      cwdTimer: null,
      createdAt: meta.createdAt || Date.now(),
      lastActive: meta.lastActive || Date.now()
    };
    sessions.set(id, session);
  }
  session.title = sessionTitles[id] || session.title || defaultTitle(id);
  session.createdAt = meta.createdAt || session.createdAt;
  session.lastActive = meta.lastActive || session.lastActive;
  return session;
}

function syncTmuxSessions() {
  const alive = new Set();
  const out = tryTmux(['list-sessions', '-F', '#{session_name}\t#{session_created}\t#{session_activity}']);
  for (const line of out.split('\n')) {
    if (!line) continue;
    const [name, created, activity] = line.split('\t');
    const id = idFromTmuxName(name);
    if (!id) continue;
    alive.add(id);
    ensureSession(id, {
      createdAt: Number(created) ? Number(created) * 1000 : Date.now(),
      lastActive: Number(activity) ? Number(activity) * 1000 : Date.now()
    });
  }
  for (const [id, session] of sessions) {
    if (!alive.has(id) && !session.term) sessions.delete(id);
  }
}

function detachViewer(session, notify = true) {
  const { term, viewerSocket } = session;
  if (session.cwdTimer) clearInterval(session.cwdTimer);
  session.cwdTimer = null;
  session.term = null;
  session.viewerSocket = '';
  if (notify && viewerSocket) io.to(viewerSocket).emit('terminal:detached', { id: session.id });
  if (term) term.kill();
}

function createSession(size = {}) {
  const id = crypto.randomUUID().replaceAll('-', '');
  runTmux(['new-session', '-d', '-s', tmuxName(id), '-c', guard.root]);
  setTmuxDefaults();
  const session = ensureSession(id, { createdAt: Date.now(), lastActive: Date.now() });
  broadcastSessions();
  return session;
}

function attachSession(socket, session, size = {}) {
  if (!tmuxHasSession(session.id)) throw new Error('Session not found');
  setTmuxDefaults();
  if (session.term) detachViewer(session, session.viewerSocket !== socket.id);
  session.history = [];
  session.historyBytes = 0;

  const term = pty.spawn('tmux', tmuxArgs(['attach-session', '-t', tmuxName(session.id)]), {
    name: 'xterm-256color',
    cols: Number(size.cols) || 100,
    rows: Number(size.rows) || 30,
    cwd: guard.root,
    env: { ...process.env, TERM: 'xterm-256color' }
  });

  session.term = term;
  session.viewerSocket = socket.id;
  session.lastActive = Date.now();
  startCwdWatch(session);

  term.onData((data) => {
    if (session.term !== term) return;
    session.lastActive = Date.now();
    remember(session, data);
    socket.emit('terminal:data', { id: session.id, data });
  });
  term.onExit(({ exitCode }) => {
    if (session.term !== term) return;
    if (session.cwdTimer) clearInterval(session.cwdTimer);
    session.cwdTimer = null;
    session.term = null;
    session.viewerSocket = '';
    if (tmuxHasSession(session.id)) {
      socket.emit('terminal:detached', { id: session.id });
    } else {
      remember(session, `\r\n[process exited: ${exitCode}]\r\n`);
      sessions.delete(session.id);
      socket.emit('terminal:exit', { id: session.id, exitCode });
      broadcastSessions();
    }
  });

  return term;
}

loadTitles();

app.get('/api/me', auth, (req, res) => {
  res.json({ root: guard.root, maxUpload: MAX_UPLOAD, sessions: listSessions() });
});

app.get('/api/tree', auth, asyncRoute(async (req, res) => {
  const dir = await guard.existing(req.query.path || '.');
  const stat = await fsp.stat(dir.real);
  if (!stat.isDirectory()) throw new HttpError(400, 'Not a directory');

  const entries = await fsp.readdir(dir.real, { withFileTypes: true });
  const rows = await Promise.all(entries.map(async (entry) => {
    const childRel = [dir.rel, entry.name].filter(Boolean).join('/');
    const isFile = entry.isFile();
    const st = isFile ? await fsp.lstat(path.join(dir.real, entry.name)) : null;
    return {
      name: entry.name,
      path: childRel,
      type: entry.isDirectory() ? 'dir' : entry.isSymbolicLink() ? 'link' : 'file',
      size: st?.size,
      mtime: st?.mtimeMs
    };
  }));

  rows.sort((a, b) => (a.type === 'dir' ? -1 : 1) - (b.type === 'dir' ? -1 : 1) || a.name.localeCompare(b.name));
  res.json({ path: dir.rel, entries: rows });
}));

app.get('/api/file', auth, asyncRoute(async (req, res) => {
  const file = await guard.existing(req.query.path || '');
  const stat = await fsp.stat(file.real);
  if (!stat.isFile()) throw new HttpError(400, 'Not a file');
  res.download(file.real, path.basename(file.full));
}));

class LimitUpload extends Transform {
  constructor(limit) {
    super();
    this.limit = limit;
    this.bytes = 0;
  }

  _transform(chunk, enc, cb) {
    this.bytes += chunk.length;
    if (this.bytes > this.limit) cb(new HttpError(413, 'Upload too large'));
    else cb(null, chunk);
  }
}

app.put('/api/file', auth, asyncRoute(async (req, res) => {
  const target = await guard.writable(req.query.path || '');
  const parent = path.dirname(target.full);
  const tmp = path.join(parent, `.upload-${process.pid}-${Date.now()}-${crypto.randomUUID()}.tmp`);

  try {
    const existing = await fsp.lstat(target.full).catch((err) => (err.code === 'ENOENT' ? null : Promise.reject(err)));
    if (existing?.isDirectory()) throw new HttpError(400, 'Cannot overwrite a directory');
    await pipeline(req, new LimitUpload(MAX_UPLOAD), fs.createWriteStream(tmp, { flags: 'wx', mode: 0o600 }));
    await fsp.rename(tmp, target.full);
    res.json({ ok: true, path: target.rel });
  } catch (err) {
    await fsp.rm(tmp, { force: true }).catch(() => {});
    throw err;
  }
}));

io.use((socket, next) => {
  if (validToken(socket.handshake.auth?.token)) next();
  else next(new Error('unauthorized'));
});

io.on('connection', (socket) => {
  socket.emit('sessions', listSessions());

  socket.on('session:create', (size, reply) => {
    const session = createSession(size || {});
    attachSession(socket, session, size || {});
    reply?.({ session: listSessions().find((item) => item.id === session.id), history: session.history });
  });

  socket.on('session:attach', (payload, reply) => {
    const id = typeof payload === 'string' ? payload : payload?.id;
    const size = typeof payload === 'object' ? payload?.size || {} : {};
    if (typeof id !== 'string' || !idFromTmuxName(tmuxName(id))) return reply?.({ error: 'Invalid session id' });
    const session = ensureSession(id);
    if (!tmuxHasSession(id)) return reply?.({ error: 'Session not found' });
    const history = captureSessionHistory(id);
    attachSession(socket, session, size);
    session.lastActive = Date.now();
    reply?.({ session: listSessions().find((item) => item.id === id), history: history ? [history] : [] });
  });

  socket.on('session:close', (id, reply) => {
    if (typeof id !== 'string') return reply?.({ ok: false });
    const session = sessions.get(id);
    if (!session) return reply?.({ ok: false });
    const viewerSocket = session.viewerSocket;
    detachViewer(session, false);
    if (tmuxHasSession(id)) runTmux(['kill-session', '-t', tmuxName(id)]);
    sessions.delete(id);
    if (sessionTitles[id]) {
      delete sessionTitles[id];
      saveTitles();
    }
    if (viewerSocket) io.to(viewerSocket).emit('terminal:exit', { id, exitCode: null });
    broadcastSessions();
    reply?.({ ok: true });
  });

  socket.on('session:rename', (payload = {}, reply) => {
    const { id } = payload || {};
    const title = cleanTitle(payload?.title);
    if (typeof id !== 'string' || !idFromTmuxName(tmuxName(id))) return reply?.({ ok: false, error: 'Invalid session id' });
    if (!title) return reply?.({ ok: false, error: 'Name is required' });
    if (!tmuxHasSession(id)) return reply?.({ ok: false, error: 'Session not found' });
    const session = ensureSession(id);
    session.title = title;
    sessionTitles[id] = title;
    saveTitles();
    broadcastSessions();
    reply?.({ ok: true, session: listSessions().find((item) => item.id === id) });
  });

  socket.on('terminal:input', (payload = {}) => {
    const { id, data } = payload || {};
    const session = sessions.get(id);
    if (session && session.viewerSocket === socket.id && typeof data === 'string') session.term?.write(data);
  });

  socket.on('terminal:resize', (payload = {}) => {
    const { id, cols, rows } = payload || {};
    const session = sessions.get(id);
    if (session && session.viewerSocket === socket.id) session.term?.resize(Number(cols) || 100, Number(rows) || 30);
  });

  socket.on('disconnect', () => {
    for (const session of sessions.values()) {
      if (session.viewerSocket === socket.id) detachViewer(session, false);
    }
  });
});

app.use((err, req, res, next) => {
  if (res.headersSent) return next(err);
  const status = err.status || 500;
  res.status(status).json({ error: status === 500 ? 'Internal server error' : err.message });
  if (status === 500) console.error(err);
});

server.listen(PORT, HOST, () => {
  const url = `http://${HOST}:${PORT}/`;
  console.log(`Web worker shell: ${url}`);
  console.log(`Workspace root: ${guard.root}`);
  console.log(`Token: ${TOKEN}`);
  if (!process.env.WEB_WORKER_TOKEN) console.log(`Open: ${url}?token=${encodeURIComponent(TOKEN)}`);
});
