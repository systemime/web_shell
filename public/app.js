const pageUrl = new URL(location.href);
if (pageUrl.username || pageUrl.password) {
  pageUrl.username = '';
  pageUrl.password = '';
  location.replace(pageUrl.href);
  throw new Error('Redirecting to credential-free URL');
}

const sessionKey = 'web-worker-session';
const rootLine = document.querySelector('#rootLine');
const statusEl = document.querySelector('#status');
const sessionsEl = document.querySelector('#sessions');
const sessionCount = document.querySelector('#sessionCount');
const sessionTitle = document.querySelector('#sessionTitle');
const sessionSelect = document.querySelector('#sessionSelect');
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
const commandDock = document.querySelector('.commandDock');
const commandInput = document.querySelector('#commandInput');
const mobileKeys = document.querySelector('#mobileKeys');

let activeSession = '';
let pendingSession = '';
let currentDir = '';
let shellDir = null;
let treeRequest = 0;
let fitFrame = 0;
let dockFrame = 0;
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
term.attachCustomWheelEventHandler((event) => {
  if (event.ctrlKey || event.metaKey) return false;
  if (event.deltaY === 0 || event.shiftKey) return true;
  event.preventDefault();
  if (term.buffer.active.type === 'normal') term.scrollLines(event.deltaY > 0 ? 5 : -5);
  else sendTerminalControl(event.deltaY > 0 ? 'scroll-down' : 'scroll-up');
  return false;
});

const socket = (() => {
  let ws;
  let connected = false;
  let seq = 0;
  let reconnectTimer = 0;
  const handlers = new Map();
  const replies = new Map();

  function send(type, data, cb) {
    if (!connected) return false;
    const req = cb ? String(++seq) : '';
    if (cb) replies.set(req, cb);
    ws.send(JSON.stringify({ type, req, data }));
    return true;
  }

  function fire(type, data) {
    for (const fn of handlers.get(type) || []) fn(data);
  }

  function connect() {
    clearTimeout(reconnectTimer);
    ws = new WebSocket(`${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/ws`);
    ws.onopen = () => {
      connected = true;
      fire('connect');
    };
    ws.onmessage = (event) => {
      const msg = JSON.parse(event.data);
      if (msg.type === 'reply') {
        const cb = replies.get(msg.req);
        replies.delete(msg.req);
        if (cb) cb(msg.data);
        return;
      }
      fire(msg.type, msg.data);
    };
    ws.onerror = () => { if (!connected) fire('connect_error'); };
    ws.onclose = () => {
      if (connected) fire('disconnect');
      connected = false;
      fire('reconnect_attempt');
      reconnectTimer = setTimeout(connect, 1000);
    };
  }

  return {
    get connected() { return connected; },
    connect,
    emit: send,
    on(type, fn) { handlers.set(type, [...(handlers.get(type) || []), fn]); },
    onReconnectAttempt(fn) { handlers.set('reconnect_attempt', [...(handlers.get('reconnect_attempt') || []), fn]); }
  };
})();
const shortcuts = {
  'ctrl-c': '\x03',
  'ctrl-b': '\x02',
  'ctrl-z': '\x1A'
};
const terminalKeys = {
  esc: '\x1B',
  tab: '\t',
  up: '\x1B[A',
  down: '\x1B[B',
  right: '\x1B[C',
  left: '\x1B[D',
  'ctrl-c': '\x03',
  'ctrl-d': '\x04'
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

function rememberSession(id) {
  if (id) localStorage.setItem(sessionKey, id);
  else localStorage.removeItem(sessionKey);
}

async function api(url, options = {}) {
  const endpoint = url.startsWith('/') ? new URL(url, location.origin) : url;
  const res = await fetch(endpoint, options);
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

function syncCommandDock() {
  cancelAnimationFrame(dockFrame);
  dockFrame = requestAnimationFrame(() => {
    document.documentElement.style.setProperty('--command-dock-height', `${Math.ceil(commandDock.getBoundingClientRect().height)}px`);
    fitTerminal(true);
  });
}

function writeTerminal(data, fast = false) {
  term.write(data, () => term.scrollToBottom());
}

function sendTerminalInput(data) {
  if (!activeSession || !socket.connected) {
    setStatus('No active shell.');
    return false;
  }
  return socket.emit('terminal:input', { id: activeSession, data });
}

function sendCommandInput() {
  const value = commandInput.value;
  if (!value) return;
  if (sendTerminalInput(`${value.replace(/\r?\n/g, '\r')}\r`)) {
    commandInput.value = '';
  }
}

function sendShortcut(name) {
  const data = shortcuts[name];
  if (!data) return;
  if (!sendTerminalInput(data)) {
    setStatus('No active shell.');
    return;
  }
  focusTerminal(true);
}

function sendTerminalControl(action) {
  if (!activeSession || !socket.connected) {
    setStatus('No active shell.');
    return;
  }
  socket.emit('terminal:control', { id: activeSession, action });
  focusTerminal(true);
}

function renderSessions(items) {
  sessionCount.textContent = items.length;
  const current = activeSession || pendingSession || '';
  sessionSelect.replaceChildren(new Option('Shells', ''), ...items.map((item) => new Option(item.title, item.id)));
  sessionSelect.value = items.some((item) => item.id === current) ? current : '';
  const frag = document.createDocumentFragment();
  if (!items.length) frag.append(emptyState('No shells yet.'));
  for (const item of items) {
    const row = clickableRow(() => attachSession(item.id));
    if (item.id === activeSession || item.id === pendingSession) row.classList.add('active');
    row.innerHTML = `<span>$</span><span class="name"></span><span class="meta"></span>`;
    row.querySelector('.name').textContent = item.title;
    row.querySelector('.meta').textContent = new Date(item.lastActive).toLocaleTimeString();
    if (item.id === activeSession) sessionTitle.textContent = item.title;
    frag.append(row);
  }
  sessionsEl.replaceChildren(frag);
}

function attachSession(id) {
  if (!id || pendingSession === id) return;
  pendingSession = id;
  activeSession = id;
  shellDir = null;
  term.reset();
  fit.fit();
  socket.emit('session:attach', { id, size: { cols: term.cols, rows: term.rows } }, (reply) => {
    pendingSession = '';
    if (reply?.error) {
      if (activeSession === id) activeSession = '';
      if (localStorage.getItem(sessionKey) === id) rememberSession('');
      return setStatus(reply.error);
    }
    activeSession = id;
    rememberSession(id);
    sessionSelect.value = id;
    sessionTitle.textContent = reply.session.title;
    setStatus('Attached. Closing this tab keeps the shell running.');
    fitTerminal(true);
    focusTerminal();
  });
}

function createSession() {
  fit.fit();
  socket.emit('session:create', { cols: term.cols, rows: term.rows }, (reply) => {
    activeSession = reply.session.id;
    rememberSession(activeSession);
    sessionSelect.value = activeSession;
    shellDir = null;
    sessionTitle.textContent = reply.session.title;
    term.reset();
    setStatus('Shell started.');
    fitTerminal(true);
    focusTerminal();
  });
}

async function connect() {
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

socket.onReconnectAttempt(() => setStatus('Reconnecting...'));
socket.on('connect', () => setStatus('Connected.'));
socket.on('disconnect', () => {
  pendingSession = '';
  if (!activeSession) return;
  activeSession = '';
  sessionTitle.textContent = 'Reconnecting...';
  setStatus('Disconnected. Reconnecting...');
});
socket.on('connect_error', (err) => {
  setStatus('Connection failed. Retrying...');
});
socket.on('sessions', (items) => {
  renderSessions(items);
  if (activeSession || pendingSession) return;
  const remembered = localStorage.getItem(sessionKey);
  const target = items.find((item) => item.id === remembered) || items[0];
  if (target) attachSession(target.id);
  else if (socket.connected) createSession();
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
    rememberSession('');
    shellDir = null;
    sessionTitle.textContent = 'No shell';
    setStatus('Shell closed.');
  }
});
socket.on('terminal:detached', ({ id }) => {
  if (id !== activeSession) return;
  activeSession = '';
  rememberSession('');
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
commandInput.addEventListener('keydown', (event) => {
  if (event.key !== 'Enter' || event.shiftKey || event.isComposing) return;
  event.preventDefault();
  sendCommandInput();
});
mobileKeys.onclick = (event) => {
  const button = event.target.closest('button');
  if (!button) return;
  const key = button.dataset.key;
  const control = button.dataset.control;
  if (key && terminalKeys[key] && sendTerminalInput(terminalKeys[key])) focusTerminal(true);
  if (control) sendTerminalControl(control);
};

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
shortcutSelect.onchange = () => {
  sendShortcut(shortcutSelect.value);
  shortcutSelect.value = '';
};
sessionSelect.onchange = () => {
  if (sessionSelect.value) attachSession(sessionSelect.value);
};
if (window.ResizeObserver) {
  terminalObserver = new ResizeObserver((entries) => {
    if (entries.some((entry) => entry.target === commandDock)) syncCommandDock();
    else fitTerminal();
  });
  terminalObserver.observe(document.querySelector('#terminal'));
  terminalObserver.observe(commandDock);
} else {
  window.addEventListener('resize', syncCommandDock);
}
syncCommandDock();

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

fitTerminal();
connect();
