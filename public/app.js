const pageUrl = new URL(location.href);
if (pageUrl.username || pageUrl.password) {
  pageUrl.username = '';
  pageUrl.password = '';
  location.replace(pageUrl.href);
  throw new Error('Redirecting to credential-free URL');
}

const tokenKey = 'web-worker-token';
const authDialog = document.querySelector('#authDialog');
const tokenInput = document.querySelector('#tokenInput');
const tokenButton = document.querySelector('#tokenButton');
const rootLine = document.querySelector('#rootLine');
const statusEl = document.querySelector('#status');
const sessionsEl = document.querySelector('#sessions');
const sessionCount = document.querySelector('#sessionCount');
const sessionTitle = document.querySelector('#sessionTitle');
const newSession = document.querySelector('#newSession');
const closeSession = document.querySelector('#closeSession');
const renameSession = document.querySelector('#renameSession');
const shortcutSelect = document.querySelector('#shortcutSelect');
const fileTree = document.querySelector('#fileTree');
const filePath = document.querySelector('#filePath');
const refreshFiles = document.querySelector('#refreshFiles');
const uploadFile = document.querySelector('#uploadFile');
const terminalFrame = document.querySelector('.terminalFrame');
const terminalEl = document.querySelector('#terminal');

const urlToken = pageUrl.searchParams.get('token');
if (urlToken) {
  localStorage.setItem(tokenKey, urlToken);
  history.replaceState(null, '', location.pathname);
}

let token = localStorage.getItem(tokenKey) || '';
let activeSession = '';
let currentDir = '';
let shellDir = null;
let treeRequest = 0;
let fitFrame = 0;
let lastSize = '';
let terminalObserver;

const term = new Terminal({
  cursorBlink: true,
  cursorStyle: 'bar',
  fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
  fontSize: 14,
  scrollback: 8000,
  theme: {
    background: '#111318',
    foreground: '#edf1f7',
    cursor: '#5cc8a7',
    selectionBackground: '#334155'
  }
});
const fit = new FitAddon.FitAddon();
term.loadAddon(fit);
term.open(terminalEl);

const socket = io({ autoConnect: false, auth: { token } });
const shortcuts = {
  'ctrl-c': '\x03',
  'ctrl-b': '\x02',
  'ctrl-z': '\x1A'
};

function skipClickWhileSelecting() {
  return window.getSelection()?.toString();
}

function focusTerminal(immediate = false) {
  if (immediate) {
    term.focus();
    return;
  }
  requestAnimationFrame(() => term.focus());
}

function clickableRow(onClick, onDblClick) {
  const row = document.createElement('div');
  let pointerDown;
  row.className = 'row';
  row.setAttribute('role', 'button');
  row.tabIndex = 0;
  row.onpointerdown = (event) => {
    pointerDown = { x: event.clientX, y: event.clientY };
  };
  row.onclick = (event) => {
    const moved = pointerDown && Math.hypot(event.clientX - pointerDown.x, event.clientY - pointerDown.y) > 4;
    pointerDown = null;
    if (!moved && !skipClickWhileSelecting()) onClick();
  };
  row.onkeydown = (event) => {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    event.preventDefault();
    onClick();
  };
  if (onDblClick) row.ondblclick = onDblClick;
  return row;
}

function setStatus(text) {
  statusEl.textContent = text;
}

function headers() {
  return { 'X-Web-Worker-Token': token };
}

async function api(url, options = {}) {
  const endpoint = url.startsWith('/') ? new URL(url, location.origin) : url;
  const res = await fetch(endpoint, { ...options, headers: { ...headers(), ...(options.headers || {}) } });
  if (res.status === 401) {
    askToken();
    throw new Error('Unauthorized');
  }
  if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || res.statusText);
  return res;
}

function fitTerminal(force = false) {
  cancelAnimationFrame(fitFrame);
  fitFrame = requestAnimationFrame(() => {
    fit.fit();
    const size = `${term.cols}x${term.rows}`;
    const resizeKey = `${activeSession || ''}:${size}`;
    if (!force && resizeKey === lastSize) return;
    lastSize = resizeKey;
    if (activeSession && socket.connected) {
      socket.emit('terminal:resize', { id: activeSession, cols: term.cols, rows: term.rows });
    }
  });
}

function writeTerminal(data, fast = false) {
  term.write(data);
}

function sendShortcut(name) {
  const data = shortcuts[name];
  if (!data) return;
  if (!activeSession || !socket.connected) {
    setStatus('No active shell.');
    return;
  }
  socket.emit('terminal:input', { id: activeSession, data });
  focusTerminal(true);
}

function renderSessions(items) {
  sessionCount.textContent = items.length;
  const frag = document.createDocumentFragment();
  if (!items.length) frag.append(emptyState('No shells yet.'));
  for (const item of items) {
    const row = clickableRow(() => attachSession(item.id));
    if (item.id === activeSession) row.classList.add('active');
    row.innerHTML = `<span>$</span><span class="name"></span><span class="meta"></span>`;
    row.querySelector('.name').textContent = item.title;
    row.querySelector('.meta').textContent = new Date(item.lastActive).toLocaleTimeString();
    if (item.id === activeSession) sessionTitle.textContent = item.title;
    frag.append(row);
  }
  sessionsEl.replaceChildren(frag);
}

function attachSession(id) {
  fit.fit();
  socket.emit('session:attach', { id, size: { cols: term.cols, rows: term.rows } }, (reply) => {
    if (reply?.error) return setStatus(reply.error);
    activeSession = id;
    shellDir = null;
    term.reset();
    sessionTitle.textContent = reply.session.title;
    setStatus('Attached. Closing this tab keeps the shell running.');
    fitTerminal(true);
    requestAnimationFrame(() => {
      writeTerminal((reply.history || []).join(''), true);
      focusTerminal();
    });
  });
}

function createSession() {
  fit.fit();
  socket.emit('session:create', { cols: term.cols, rows: term.rows }, (reply) => {
    activeSession = reply.session.id;
    shellDir = null;
    sessionTitle.textContent = reply.session.title;
    term.reset();
    setStatus('Shell started.');
    fitTerminal(true);
    focusTerminal();
  });
}

function askToken() {
  tokenInput.value = token;
  authDialog.showModal();
}

async function connect() {
  if (!token) return askToken();
  socket.auth = { token };
  socket.connect();
  try {
    const me = await (await api('/api/me')).json();
    rootLine.textContent = me.root;
    loadTree(currentDir);
  } catch (err) {
    setStatus(err.message);
  }
}

function dirname(p) {
  const parts = String(p || '').split('/').filter(Boolean);
  parts.pop();
  return parts.join('/');
}

function showPath(path) {
  currentDir = path || '';
  filePath.textContent = `/${currentDir}`;
}

function emptyState(text) {
  const empty = document.createElement('div');
  empty.className = 'empty';
  empty.textContent = text;
  return empty;
}

async function loadTree(path = '') {
  const request = ++treeRequest;
  showPath(path);
  fileTree.replaceChildren(emptyState('Loading files...'));
  const frag = document.createDocumentFragment();
  if (path) {
    const up = clickableRow(() => loadTree(dirname(path)));
    up.innerHTML = '<span>..</span><span class="name">Parent</span><span></span>';
    frag.append(up);
  }
  try {
    const data = await (await api(`/api/tree?path=${encodeURIComponent(path)}`)).json();
    if (request !== treeRequest) return;
    if (!data.entries.length) frag.append(emptyState('No files here.'));
    for (const entry of data.entries) {
      const row = clickableRow(
        () => {
          if (entry.type === 'dir') {
            loadTree(entry.path);
          } else {
            fileTree.querySelectorAll('.selected').forEach((item) => item.classList.remove('selected'));
            row.classList.add('selected');
            setStatus(`Selected ${entry.name}. Double-click to download.`);
          }
        },
        entry.type === 'dir' ? null : () => downloadFile(entry.path)
      );
      row.innerHTML = `<span></span><span class="name"></span><span class="meta"></span>`;
      row.children[0].textContent = entry.type === 'dir' ? '>' : '-';
      row.querySelector('.name').textContent = entry.name;
      row.querySelector('.meta').textContent = entry.type === 'dir' ? 'dir' : entry.size == null ? '' : formatSize(entry.size);
      frag.append(row);
    }
    fileTree.replaceChildren(frag);
  } catch (err) {
    if (request !== treeRequest) return;
    fileTree.replaceChildren(emptyState(err.message));
    setStatus(err.message);
  }
}

function followShellPath(path, outside = false) {
  if (outside) {
    shellDir = null;
    setStatus('Shell path is outside the file root.');
    return;
  }
  if (typeof path !== 'string' || path === shellDir) return;
  shellDir = path;
  if (path !== currentDir) loadTree(path);
}

function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}

async function downloadFile(path) {
  const res = await api(`/api/file?path=${encodeURIComponent(path)}`);
  const blob = await res.blob();
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = path.split('/').pop();
  a.click();
  URL.revokeObjectURL(a.href);
  setStatus(`Downloaded ${a.download}.`);
}

socket.on('connect', () => setStatus('Connected.'));
socket.on('connect_error', () => askToken());
socket.on('sessions', (items) => {
  renderSessions(items);
  if (!activeSession && items[0]) attachSession(items[0].id);
  if (!items.length && socket.connected) createSession();
});
socket.on('terminal:data', ({ id, data }) => {
  if (id === activeSession) writeTerminal(data);
});
socket.on('terminal:cwd', ({ id, path, outside }) => {
  if (id === activeSession) followShellPath(path, outside);
});
socket.on('terminal:exit', ({ id }) => {
  if (id === activeSession) {
    activeSession = '';
    shellDir = null;
    sessionTitle.textContent = 'No shell';
    setStatus('Shell closed.');
  }
});
socket.on('terminal:detached', ({ id }) => {
  if (id !== activeSession) return;
  activeSession = '';
  shellDir = null;
  sessionsEl.querySelectorAll('.active').forEach((item) => item.classList.remove('active'));
  sessionTitle.textContent = 'Detached';
  setStatus('Opened in another browser.');
  writeTerminal('\r\n[detached: opened in another browser]\r\n');
});

term.onData((data) => {
  if (activeSession && socket.connected) socket.emit('terminal:input', { id: activeSession, data });
});

terminalFrame.addEventListener('pointerdown', () => focusTerminal(true));

newSession.onclick = createSession;
closeSession.onclick = () => activeSession && socket.emit('session:close', activeSession);
renameSession.onclick = () => {
  if (!activeSession) return setStatus('No active shell.');
  const title = prompt('Rename shell', sessionTitle.textContent || '');
  if (title == null) return;
  socket.emit('session:rename', { id: activeSession, title }, (reply) => {
    if (!reply?.ok) return setStatus(reply?.error || 'Rename failed.');
    sessionTitle.textContent = reply.session.title;
    setStatus('Renamed.');
  });
};
refreshFiles.onclick = () => loadTree(currentDir);
tokenButton.onclick = askToken;
shortcutSelect.onchange = () => {
  sendShortcut(shortcutSelect.value);
  shortcutSelect.value = '';
};
if (window.ResizeObserver) {
  terminalObserver = new ResizeObserver(fitTerminal);
  terminalObserver.observe(document.querySelector('#terminal'));
} else {
  window.addEventListener('resize', fitTerminal);
}

uploadFile.onchange = async () => {
  const file = uploadFile.files[0];
  if (!file) return;
  const target = [currentDir, file.name].filter(Boolean).join('/');
  try {
    await api(`/api/file?path=${encodeURIComponent(target)}`, { method: 'PUT', body: file });
    setStatus(`Uploaded ${file.name}.`);
    loadTree(currentDir);
  } catch (err) {
    setStatus(err.message);
  } finally {
    uploadFile.value = '';
  }
};

authDialog.addEventListener('close', () => {
  if (authDialog.returnValue !== 'default') return;
  token = tokenInput.value.trim();
  localStorage.setItem(tokenKey, token);
  connect();
});

fitTerminal();
connect();
