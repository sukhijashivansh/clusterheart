# Running Clusterheart (local dev)

How to build and run the whole app on your machine, plus the setup the
dashboard needs (kubeconfig, AWS costs, AI provider) and fixes for the errors
you're likely to hit.

Clusterheart has three parts:

| Part | Dir | What it is | Port |
|------|-----|-----------|------|
| Account server | `server/` | auth + approve/revoke admin panel (MongoDB) | `127.0.0.1:9000` |
| Dashboard binary | `app/` | Go web server that reads your **local** kubeconfig and serves the UI | random localhost port (spawned by the desktop app) |
| Desktop app | `electron/` | Electron login gate that spawns the dashboard once you're approved | — |

The flow: **desktop app → logs in against the account server → if approved,
spawns the dashboard binary and points its window at it.** Kubernetes/AWS work
happens **locally** in the dashboard binary; credentials never leave the machine.

---

## Prerequisites

- **Go** 1.26+ (`go version`)
- **Node** 20+ and npm (`node --version`)
- **MongoDB** — the account server needs a connection string. `server/.env`
  already points at a MongoDB Atlas cluster, so no local install is required.
- **kubectl-style kubeconfig** at `~/.kube/config` for a cluster you can reach
  (the dashboard reads this).
- **aws CLI** on PATH (`aws --version`) — only needed for the FinOps cost panel.

---

## 1. Start the account server

```bash
cd server
go build -o clusterheart-server .
./clusterheart-server            # reads server/.env
#  -> http://127.0.0.1:9000
```

Config lives in `server/.env` (copy from `.env.example`):

| Var | Default | Purpose |
|-----|---------|---------|
| `MONGO_URI` | *(required)* | MongoDB connection string |
| `MONGO_DB` | `clusterheart` | database name (`users` collection) |
| `ADMIN_EMAILS` | *(empty)* | comma-separated emails allowed at `/admin` |
| `ADDR` | `127.0.0.1:9000` | listen address |
| `SESSION_SECRET` / `SECRET_FILE` | auto | session signing key (auto-written to `server/secret.key`) |

You'll see `connected to MongoDB (db=clusterheart)` and
`clusterheart account server on http://127.0.0.1:9000` when it's up.

- Anyone signs up at `http://127.0.0.1:9000/signup`.
- An admin (email in `ADMIN_EMAILS`) opens `http://127.0.0.1:9000/admin` to
  **Approve / Revoke** users.

## 2. Build the dashboard binary

The desktop app spawns this; it just needs to exist and be current.

```bash
cd app
go build -o clusterheart-app .
```

Rebuild this **whenever you change anything in `app/`** (including
`app/static/*` — the frontend is embedded into the binary at build time, so a
CSS/JS edit doesn't show up until you rebuild).

> **Tip — live frontend editing:** run the binary with `KUBEPULSE_DEV=1` and it
> serves `app/static` from disk (no rebuild needed on refresh):
> `KUBEPULSE_DEV=1 ./clusterheart-app -addr 127.0.0.1:8088`, then open
> `http://127.0.0.1:8088` in a browser. Good for iterating on the UI without the
> desktop app.

## 3. Start the desktop app

```bash
cd electron
npm install                                    # first time only
CLUSTERHEART_SERVER=http://127.0.0.1:9000 npm start
```

- Sign in. If **not approved yet** → "pending" screen; approve at the server's
  `/admin`, then hit **Check again**.
- Once approved, the dashboard boots. An approved session is remembered, so you
  skip the login next launch (until revoked or the 30-day token expires).
- **Account → Log out** returns to the login screen.

`CLUSTERHEART_SERVER` defaults to a hosted server if unset — always set it to
`http://127.0.0.1:9000` when running the local server.

---

## One-shot: start everything

From the repo root:

```bash
# 1. server (own terminal)
( cd server && go build -o clusterheart-server . && ./clusterheart-server )

# 2. dashboard binary (build once, or after any app/ change)
( cd app && go build -o clusterheart-app . )

# 3. desktop app (own terminal)
( cd electron && CLUSTERHEART_SERVER=http://127.0.0.1:9000 npm start )
```

To restart the desktop app after rebuilding the dashboard binary: quit the app
(or `pkill -f "clusterheart/electron/node_modules/electron"`) and run step 3
again. The server can keep running.

---

## Feature setup

### AWS cost panel (FinOps → AWS Daily Spend)

The cost panel runs `aws ce get-cost-and-usage`. It authenticates using the
**same AWS profile your current kube context uses** (read from the context's
`exec` auth block, e.g. `AWS_PROFILE=shivansh`). So if your cluster loads, costs
should load too — **as long as that profile's credentials are valid**.

If you see `NoCredentials` / a cost error:

```bash
# log in to the SSO profile your kube context uses
aws sso login --profile <profile>      # e.g. shivansh
# sanity check
aws --profile <profile> sts get-caller-identity
```

Then click **load costs** again. (Cost Explorer calls cost ~$0.01 each.)

### AI chat (AI tab)

Set everything in the **AI tab toolbar** — no env vars or restarts needed. Keys
are stored in the browser's `localStorage` (client-side only) and sent with each
request:

- **provider** — `gemini` or `openrouter`.
- **api key** — paste your key (⌘V works; the app has an Edit menu). Stored
  per-provider, shows **saved ✓**.
- **model** *(OpenRouter)* — leave **blank** to use free models (no credits
  needed). The server tries several `:free` models in order:
  `llama-3.3-70b → qwen3-next-80b → gpt-oss-20b → gemma-4-31b`. Or type any
  OpenRouter model id (e.g. a paid `anthropic/claude-*` if you have credits).
- **max tokens** — reply-length cap. Blank = 1500. Lower it only if a paid model
  says you can't afford the tokens.

Env-var fallbacks still work for headless/dev use: `OPENROUTER_API_KEY`,
`OPENROUTER_MODEL`, `OPENROUTER_MAX_TOKENS`, `GEMINI_API_KEY`.

---

## Troubleshooting

| Symptom | Cause / fix |
|---------|-------------|
| `Cannot reach the server` at login | Account server isn't running, or `CLUSTERHEART_SERVER` is wrong. Start step 1; set the env var to `http://127.0.0.1:9000`. |
| Login says **pending** | Your account isn't approved. Approve it at `http://127.0.0.1:9000/admin`, then **Check again**. |
| `connected to MongoDB` never prints | Bad `MONGO_URI` or no network to Atlas. |
| Frontend change didn't show up | Rebuild `app/` (`go build -o clusterheart-app .`) and restart the desktop app — static is embedded. Or use `KUBEPULSE_DEV=1`. |
| **Can't paste** the API key | Fixed — the app now has an Edit menu, so ⌘V works everywhere. If you're on an old build, restart the desktop app. |
| Cost panel: `NoCredentials` | The kube context's AWS profile has no live creds. `aws sso login --profile <profile>`. |
| AI: `no OpenRouter API key` | Paste a key in the AI tab's **api key** field. |
| AI: `can only afford N tokens` | You're on a **paid** model with low OpenRouter credits. Clear the **model** field to use free models, or add credits, or lower **max tokens**. |
| AI: `Provider returned error` / `all free models are busy` | A free model's upstream host is rate-limited/down. Wait ~30s and retry, or pin a specific `:free` model in the **model** field. Free models are rate-limited (a few req/min). |

---

## Building installers (later)

```bash
cd electron
npm run dist:mac   # -> dist/*.dmg
npm run dist:win   # -> dist/*.exe
```

Packaging bundles per-platform dashboard binaries from `electron/resources/`,
named `clusterheart-<os>-<arch>[.exe]`. Build them from `app/` per target OS,
e.g. `GOOS=windows GOARCH=amd64 go build -o clusterheart-windows-amd64.exe .`.
