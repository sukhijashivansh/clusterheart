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

const { app, BrowserWindow, ipcMain, shell, Menu } = require('electron');
const { spawn } = require('child_process');
const path = require('path');
const fs = require('fs');
const net = require('net');
const os = require('os');

const SERVER = (process.env.CLUSTERHEART_SERVER || 'http://168.144.82.205:9000').replace(/\/+$/, '');

// Never let a stray error take the whole app down silently — log it and fall
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
  serverProc = spawn(bin, ['-addr', `127.0.0.1:${port}`], { env: patchedEnv(), stdio: 'ignore' });
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
  if (serverProc) { try { serverProc.kill(); } catch (_) {} serverProc = null; }
}

// --- server calls --------------------------------------------------------
async function apiLogin(email, password) {
  const res = await fetch(`${SERVER}/api/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password }),
    signal: AbortSignal.timeout(10000),
  });
  const data = await res.json().catch(() => ({}));
  return { status: res.status, data };
}
async function apiSession(token) {
  const res = await fetch(`${SERVER}/api/session`, {
    headers: { Authorization: `Bearer ${token}` },
    signal: AbortSignal.timeout(10000),
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
  try { await startDashboard(); return { ok: true }; }
  catch (err) { return { ok: false, error: String(err.message || err) }; }
});

ipcMain.handle('auth:signup-url', async () => { shell.openExternal(`${SERVER}/signup`); });

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
