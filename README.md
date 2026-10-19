# Clusterheart

A clean, git-free workspace with three parts:

```
clusterheart/
├── app/        the dashboard - Go binary that serves the web UI (reads LOCAL kubeconfig/SSO)
├── electron/   the desktop app - login screen that gates the dashboard
└── server/     the account server - real auth + your approve/revoke admin panel
```

## Download

The desktop app is served **from the home page itself** - the server hosts the
installers under `/downloads/` and the landing page's **Download** section links
to them. The repo stays private; nothing is published to a public GitHub
release.

- **Where files live:** `DOWNLOADS_DIR` (default `downloads/` next to the server
  binary; on the VPS: `/root/clusterheart/server/downloads`).
- **How they get there:** [`.github/workflows/release.yml`](.github/workflows/release.yml)
  builds macOS/Windows/Linux on a `v*` tag and **scp's the installers to that
  folder** using the same `DEPLOY_SSH_*` secrets as the server deploy. No GitHub
  release involved.
- To cut a build: bump `version` in `electron/package.json`, then
  `git tag v1.0.1 && git push origin v1.0.1`.

Canonical download URLs the landing page links to:
`/downloads/Clusterheart-macOS-AppleSilicon.dmg`, `…-macOS-Intel.dmg`,
`…-Windows.exe`, `…-Linux.AppImage` (browse all at `/downloads/`).

## How it fits together

```
  Desktop app (Electron)                      Account server (Go)
  ┌────────────────────────┐                  ┌─────────────────────────┐
  │  login screen          │── POST /api/login ─▶ verify password (bcrypt)│
  │   email + password     │◀── approved? ─────│ is this user approved?  │
  │        │               │                  │                         │
  │   approved → spawn ─────┼─▶ Go binary       │  /admin  ← you approve  │
  │   Go dashboard (local)  │   reads LOCAL     │          / revoke users │
  │        │               │   kubeconfig/SSO  └─────────────────────────┘
  │   not approved → "pending"                   (signup / login / logout,
  └────────────────────────┘                     signed sessions, JSON store)
```

- **The user only ever learns their access status at login**, inside the app.
  Approved → the dashboard opens. Not approved → a "pending approval" screen.
- The Go binary does all Kubernetes work **locally** - credentials never leave
  the machine. The server only handles accounts and approvals.
- Passwords are bcrypt-hashed; sessions are signed tokens (cookie for the web
  pages, bearer token for the desktop app).

---

## 1. Run the account server

Config lives in `server/.env` (copy from `.env.example`): `MONGO_URI`,
`MONGO_DB=clusterheart`, `ADMIN_EMAILS`.

```bash
cd server
go build -o clusterheart-server .
./clusterheart-server            # reads server/.env
#  -> http://127.0.0.1:9000
```

- Users are stored in **MongoDB** (`MONGO_DB` database, `users` collection).
- Anyone can **sign up** at `http://127.0.0.1:9000/signup`.
- **You** (any email in `ADMIN_EMAILS`) sign up too, then open
  `http://127.0.0.1:9000/admin` to **Approve / Revoke** users.
- The session secret is auto-generated to `server/secret.key`
  (set `SESSION_SECRET` to pin it).

## 2. Build the dashboard binary

```bash
cd app
go build -o clusterheart-app .
```

(The Electron app spawns this. It needs a working kubeconfig - same as before.)

## 3. Run the desktop app

```bash
cd electron
CLUSTERHEART_SERVER=http://127.0.0.1:9000 npm start
```

- Sign in with an account. If it's **not approved yet**, you'll see the pending
  screen - approve it at the server's `/admin`, then hit **Check again**.
- Once approved, the dashboard boots.
- **Account → Log out** in the menu bar returns to the login screen.
- The session is remembered, so an approved user skips the login next launch
  (until the owner revokes them or the 30-day token expires).

## Build installers (later)

```bash
cd electron
npm run dist:mac   # -> dist/*.dmg
npm run dist:win   # -> dist/*.exe
```

Packaging bundles per-platform binaries from `electron/resources/`. Build those
from `app/` for each OS (`GOOS=windows GOARCH=amd64 go build ...`) and drop them
in `electron/resources/` named `clusterheart-<os>-<arch>[.exe]`.

## Going live

- Deploy `server/` (Fly, a VPS) behind HTTPS. Set `ADDR=0.0.0.0:9000`,
  `ADMIN_EMAILS`, and a fixed `SESSION_SECRET`.
- Point every app build at it: `CLUSTERHEART_SERVER=https://your-server`.

## Env reference

| Program | Var | Default | Purpose |
|---------|-----|---------|---------|
| server  | `MONGO_URI` | *(required)* | MongoDB Atlas connection string |
| server  | `MONGO_DB` | `clusterheart` | database name (`users` collection) |
| server  | `ADMIN_EMAILS` | *(empty)* | comma-separated admin emails (can reach /admin) |
| server  | `ADDR` | `127.0.0.1:9000` | listen address |
| server  | `SESSION_SECRET` / `SECRET_FILE` | auto | session signing key |
| electron| `CLUSTERHEART_SERVER` | `http://127.0.0.1:9000` | where to authenticate |
