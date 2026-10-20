// Clusterheart desktop app.
//
// Launch flow:
//   1. Show login.html. The user signs in (email + password) against the
//      account server (CLUSTERHEART_SERVER).
//   2. The server's reply tells us whether this user has been APPROVED by the
//      owner. Not approved -> the login screen says "pending". Approved -> we
//      store the session token and boot the dashboard.
//   3. Booting = spawn the bundled Go binary on a free localhost port (it reads
//      the user's LOCAL kubeconfig/SSO) and point the window at it.
//   4. The app menu has "Log out", which kills the binary, clears the token,
//      and returns to the login screen.

const { app, BrowserWindow, ipcMain, shell, Menu, Notification } = require('electron');
const { spawn } = require('child_process');
const path = require('path');
const fs = require('fs');
const net = require('net');
const os = require('os');

const SERVER = (process.env.CLUSTERHEART_SERVER || 'https://www.clusterheart.com').replace(/\/+$/, '');

// Never let a stray error take the whole app down silently - log it and fall
// back to the login screen instead of the window vanishing.
process.on('uncaughtException', (err) => {
  console.error('[clusterheart] uncaught exception:', err);
  if (win && !win.isDestroyed()) win.loadFile('login.html').catch(() => {});
});
process.on('unhandledRejection', (err) => {
  console.error('[clusterheart] unhandled rejection:', err);
});

let win = null;
let serverProc = null;
let sessionToken = null;
let heartbeatTimer = null;
let lastNoticeAt = null;
let lastNudgedVersion = null;

// isNewer reports whether version a is newer than b (dotted numeric compare).
function isNewer(a, b) {
  const pa = String(a).split('.').map((n) => parseInt(n, 10) || 0);
  const pb = String(b).split('.').map((n) => parseInt(n, 10) || 0);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const x = pa[i] || 0, y = pb[i] || 0;
    if (x !== y) return x > y;
  }
  return false;
}

// --- token persistence ---------------------------------------------------
function tokenFile() {
  return path.join(app.getPath('userData'), 'session.token');
}
function saveToken(t) {
  sessionToken = t;
  try { fs.writeFileSync(tokenFile(), t, { mode: 0o600 }); } catch (_) {}
}
function loadToken() {
  try { return fs.readFileSync(tokenFile(), 'utf8').trim(); } catch (_) { return ''; }
}
function clearToken() {
  sessionToken = null;
  try { fs.unlinkSync(tokenFile()); } catch (_) {}
}

// --- bundled Go binary ---------------------------------------------------
function binaryName() {
  const arch = process.arch === 'arm64' ? 'arm64' : 'amd64';
  if (process.platform === 'darwin') return `clusterheart-darwin-${arch}`;
  if (process.platform === 'win32') return 'clusterheart-windows-amd64.exe';
  return `clusterheart-linux-${arch}`;
}
function binaryPath() {
  if (app.isPackaged) return path.join(process.resourcesPath, binaryName());
  // dev: the binary built in ../app
  const dev = path.join(__dirname, '..', 'app', process.platform === 'win32' ? 'clusterheart-app.exe' : 'clusterheart-app');
  if (fs.existsSync(dev)) return dev;
  return path.join(__dirname, 'resources', binaryName());
}

// GUI apps don't inherit the login shell's PATH, so exec-auth kubeconfigs
// (aws / gcloud / kubectl) can't be found. Prepend the usual locations.
function patchedEnv() {
  const extra = ['/opt/homebrew/bin', '/usr/local/bin', '/usr/bin', '/bin',
    path.join(os.homedir(), '.local/bin'), path.join(os.homedir(), 'google-cloud-sdk/bin')];
  const env = { ...process.env };
  const sep = process.platform === 'win32' ? ';' : ':';
  env.PATH = [...new Set([...extra, ...(env.PATH ? env.PATH.split(sep) : [])])].join(sep);
  return env;
}

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.unref();
    s.on('error', reject);
    s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => resolve(p)); });
  });
}
function waitForPort(port, timeout = 15000) {
  const end = Date.now() + timeout;
  return new Promise((resolve, reject) => {
    const tick = () => {
      const c = net.connect(port, '127.0.0.1');
      c.on('connect', () => { c.destroy(); resolve(); });
      c.on('error', () => { c.destroy(); Date.now() > end ? reject(new Error('server did not start')) : setTimeout(tick, 200); });
    };
    tick();
  });
}

async function startDashboard() {
  const bin = binaryPath();
  if (!fs.existsSync(bin)) throw new Error(`dashboard binary not found at ${bin}`);
  try { fs.chmodSync(bin, 0o755); } catch (_) {}
  const port = await freePort();
  // Pass the account server + the user's session token so the dashboard can
  // store rightsizing usage history through our server (no DB creds in the app).
  const env = patchedEnv();
  env.CLUSTERHEART_SERVER = SERVER;
  if (sessionToken) env.CLUSTERHEART_TOKEN = sessionToken;
  serverProc = spawn(bin, ['-addr', `127.0.0.1:${port}`], { env, stdio: 'ignore' });
  // Without an 'error' listener, a failed spawn throws and crashes the app.
  let spawnErr = null;
  serverProc.on('error', (err) => { spawnErr = err; });
  serverProc.on('exit', () => { serverProc = null; });
  try {
    await waitForPort(port);
  } catch (e) {
    throw new Error(spawnErr ? `could not launch dashboard: ${spawnErr.message}` : e.message);
  }
  await win.loadURL(`http://127.0.0.1:${port}`);
}

function stopDashboard() {
  stopHeartbeat();
  if (serverProc) { try { serverProc.kill(); } catch (_) {} serverProc = null; }
}

// --- session heartbeat ---------------------------------------------------
// While the dashboard is open, re-check the session server-side every 30s so
// an admin Revoke / Force-out drops the user back to the login screen within
// ~30s (not just at next launch), and so broadcast notices get delivered.
function stopHeartbeat() {
  if (heartbeatTimer) { clearInterval(heartbeatTimer); heartbeatTimer = null; }
}
function startHeartbeat() {
  stopHeartbeat();
  heartbeatTimer = setInterval(async () => {
    if (!sessionToken) return;
    let s;
    try { s = await apiSession(sessionToken); }
    catch (_) { return; } // transient network blip - don't kick on it
    if (!s.valid || !s.approved) {
      stopHeartbeat();
      await doLogout();
      return;
    }
    if (s.notice && s.notice.message && s.notice.updatedAt !== lastNoticeAt) {
      lastNoticeAt = s.notice.updatedAt;
      try {
        if (Notification.isSupported()) {
          new Notification({ title: 'Clusterheart', body: s.notice.message }).show();
        }
      } catch (_) {}
    }
    // Nudge to update when the server advertises a newer version.
    if (s.latestVersion && isNewer(s.latestVersion, app.getVersion()) && lastNudgedVersion !== s.latestVersion) {
      lastNudgedVersion = s.latestVersion;
      try {
        if (Notification.isSupported()) {
          const n = new Notification({
            title: 'Update available',
            body: `Clusterheart ${s.latestVersion} is available. Click to download the latest version.`,
          });
          n.on('click', () => shell.openExternal(`${SERVER}/#download`));
          n.show();
        }
      } catch (_) {}
    }
  }, 30000);
}

// --- server calls --------------------------------------------------------
async function apiLogin(email, password) {
  const res = await fetch(`${SERVER}/api/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password }),
    // Long timeout: a free-tier host may be cold-starting on first login.
    signal: AbortSignal.timeout(60000),
  });
  const data = await res.json().catch(() => ({}));
  return { status: res.status, data };
}
async function apiSession(token) {
  const res = await fetch(`${SERVER}/api/session`, {
    headers: { Authorization: `Bearer ${token}` },
    signal: AbortSignal.timeout(20000),
  });
  return res.json().catch(() => ({ valid: false }));
}

// --- IPC -----------------------------------------------------------------
ipcMain.handle('auth:login', async (_e, { email, password }) => {
  try {
    const { status, data } = await apiLogin(email, password);
    if (status !== 200 || !data.ok) return { ok: false, error: data.error || 'Sign in failed.' };
    if (data.approved && data.token) saveToken(data.token);
    return { ok: true, approved: !!data.approved, name: data.name || '' };
  } catch (err) {
    return { ok: false, error: 'Cannot reach the server. Check your connection.' };
  }
});

// Called on startup to skip the form if we already have a valid, approved token.
ipcMain.handle('auth:resume', async () => {
  const t = loadToken();
  if (!t) return { approved: false };
  try {
    const s = await apiSession(t);
    if (s.valid && s.approved) { sessionToken = t; return { approved: true, name: s.name || '' }; }
    if (s.valid) return { approved: false, name: s.name || '', pending: true };
  } catch (_) {}
  return { approved: false };
});

ipcMain.handle('app:start', async () => {
  try { await startDashboard(); startHeartbeat(); return { ok: true }; }
  catch (err) { return { ok: false, error: String(err.message || err) }; }
});

ipcMain.handle('auth:signup-url', async () => { shell.openExternal(`${SERVER}/signup`); });

// Google sign-in (system browser). Opens the OAuth flow in the default browser;
// the callback hands a one-time code back via the clusterheart:// protocol,
// handled by handleDeepLink below.
ipcMain.handle('auth:google', async () => { shell.openExternal(`${SERVER}/auth/google/start?desktop=1`); });

async function completeGoogle(code) {
  try {
    const res = await fetch(`${SERVER}/api/auth/exchange`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code }), signal: AbortSignal.timeout(20000),
    });
    const d = await res.json().catch(() => ({}));
    if (res.ok && d.ok && d.token) saveToken(d.token);
  } catch (_) {}
  // Reload the login screen; its startup resume() boots the dashboard if the
  // account is approved, or shows the pending view, using the token we stored.
  if (win && !win.isDestroyed()) {
    try { await win.loadFile('login.html'); } catch (_) {}
    if (win.isMinimized()) win.restore();
    win.focus();
  }
}

function handleDeepLink(u) {
  try {
    const url = new URL(u);
    if (url.protocol === 'clusterheart:') {
      const code = url.searchParams.get('code');
      if (code) completeGoogle(code);
    }
  } catch (_) {}
}

async function doLogout() {
  stopDashboard();
  clearToken();
  if (win) await win.loadFile('login.html');
}
ipcMain.handle('auth:logout', doLogout);

// --- menu ----------------------------------------------------------------
function buildMenu() {
  const template = [
    ...(process.platform === 'darwin' ? [{ role: 'appMenu' }] : []),
    {
      label: 'Account',
      submenu: [
        { label: 'Log out', click: doLogout },
        { type: 'separator' },
        { role: 'quit' },
      ],
    },
    // Without an Edit menu, macOS never registers the Cmd+C/V/X/A accelerators,
    // so paste doesn't work in any input (e.g. pasting a long API key).
    { role: 'editMenu' },
    { role: 'viewMenu' },
    { role: 'windowMenu' },
  ];
  Menu.setApplicationMenu(Menu.buildFromTemplate(template));
}

// --- window --------------------------------------------------------------
function createWindow() {
  win = new BrowserWindow({
    width: 1180, height: 780, minWidth: 900, minHeight: 620,
    backgroundColor: '#ffffff', title: 'Clusterheart',
    icon: path.join(__dirname, 'build', 'icon.png'),
    webPreferences: { preload: path.join(__dirname, 'preload.js'), contextIsolation: true, nodeIntegration: false },
  });
  win.loadFile('login.html');
  win.on('closed', () => (win = null));
  win.webContents.setWindowOpenHandler(({ url }) => {
    if (url.startsWith('http://127.0.0.1')) return { action: 'allow' };
    shell.openExternal(url);
    return { action: 'deny' };
  });
}

// Single-instance + custom protocol, for the Google (system browser) handback.
if (!app.requestSingleInstanceLock()) {
  app.quit();
}
app.on('second-instance', (_e, argv) => {
  const u = argv.find((a) => typeof a === 'string' && a.startsWith('clusterheart://'));
  if (u) handleDeepLink(u);
  if (win) { if (win.isMinimized()) win.restore(); win.focus(); }
});
app.on('open-url', (e, url) => { e.preventDefault(); handleDeepLink(url); });
try { app.setAsDefaultProtocolClient('clusterheart'); } catch (_) {}

app.whenReady().then(() => {
  // Dock icon in dev (packaged builds get the icon from electron-builder).
  if (process.platform === 'darwin' && app.dock) {
    try { app.dock.setIcon(path.join(__dirname, 'build', 'icon.png')); } catch (_) {}
  }
  buildMenu();
  createWindow();
});
app.on('activate', () => { if (BrowserWindow.getAllWindows().length === 0) createWindow(); });
app.on('window-all-closed', () => { if (process.platform !== 'darwin') app.quit(); });
app.on('before-quit', () => { app.isQuitting = true; stopDashboard(); });
