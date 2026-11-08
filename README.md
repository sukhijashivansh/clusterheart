# Clusterheart

Web-first monitoring for Kubernetes clusters and Linux servers: one dashboard
for health, history, incidents and AI help. Live at
[clusterheart.com](https://www.clusterheart.com).

_Status as of 2026-11-09._

## What's in the repo

```
clusterheart/
├── index.html, styles.css, …   landing page (static, Vercel)
├── server/                     web app + accounts (Go): /app, /login, /account, /admin
├── sre/                        control plane + agents (Go module)
│   ├── cmd/controlplane          stores clusters, metrics, incidents, fixes (MongoDB)
│   ├── cmd/agent                 read-only Kubernetes agent (runs in the cluster)
│   ├── cmd/hostagent             read-only Linux server agent (systemd)
│   └── cmd/remediation           opt-in fix controller (narrow RBAC)
├── deploy/                     k8s manifests (agent, remediation) + systemd unit
└── docs/                       connect-a-cluster guide, SRE design docs
```

## How it fits together

```
 Browser ── www.clusterheart.com (Vercel) ──┬── landing page (static)
                                            └── /app /login /account /admin /api/*
                                                    │ rewrite
                                                    ▼
                         clusterheart-server.clusterheart.com (AWS EC2 + Caddy)
                         ├── server/  web app, accounts, teams      ── MongoDB Atlas
                         └── /cp  →  sre control plane              ── MongoDB Atlas
                                          ▲
              agents push snapshots ──────┘
              (k8s agent, host agent; read-only, outbound HTTPS only)
```

- The browser only talks to `server/`. It calls the control plane with an admin
  token, scoped to the tenant of the workspace the user is in.
- Agents never receive credentials for anything else; nothing reaches into a
  cluster or server from outside.

## Features (live)

**Monitoring**
- Kubernetes clusters (EKS, GKE, AKS, k3s, kubeadm): nodes, pods, jobs,
  namespaces, problems and warnings, logs from failing pods, leaked-secret
  detection.
- Linux servers (Ubuntu, Debian, RHEL, Rocky, Amazon Linux, Fedora; x86_64 and
  arm64): CPU, memory, disks, load, network, processes, error logs, NVIDIA and
  AMD GPUs (sampled every second, average and peak per 30s report).
- Timeline: up to 7 days of history, ranges from 15 min to 7 days, a custom
  from/to range, drag-to-zoom, a table view.

**Acting on it**
- Incidents with AI diagnosis, policy-checked fix proposals, approve/reject,
  pause/resume automation.
- Rightsizing (requests vs 6h peak) with one-click apply.
- Latency check against any running pod.
- AI assistant per cluster or server (OpenRouter, free-model fallback).

**Accounts and teams**
- Sign-up with admin approval, password reset by email, force-out and revoke.
- Teams: a main account adds people in **Account & team** as Member (view + AI)
  or Admin (also add/remove/rename and act on fixes). They get an email, skip
  the approval wait, and work in the owner's workspace. A switcher lets people
  move between their own workspace and teams they're on.
- Admin panel: users with their clusters, servers and team, and a broadcast
  message shown at the top of everyone's dashboard.

**Look and feel**
- Six colour schemes (click the logo), shared by the landing page and the app.
- Cluster cards and Timeline charts can be dragged into any order (saved per
  browser).

## Deploys

Every push to `main` deploys the part that changed (GitHub Actions):

| Workflow | Triggers on | Does |
|---|---|---|
| `deploy-server.yml` | `server/**` | builds and restarts the account server on EC2 |
| `deploy-controlplane.yml` | control plane, contract, redact, probe | tests against MongoDB, builds and restarts the control plane |
| `publish-agent.yml` | k8s agent code | builds the agent binary and uploads it to Vercel Blob |
| `publish-hostagent.yml` | host agent code | builds `clusterheart-host-agent-linux-{amd64,arm64}` and uploads to Vercel Blob |
| `publish-remediation.yml` | remediation code | builds the remediation controller and uploads it to Vercel Blob |

The landing page deploys through Vercel on push. After changing Tailwind classes
in `index.html`, rebuild the CSS and bump its cache-bust hash:

```bash
npx -y tailwindcss@3 -c tailwind.config.js -i input.css -o styles.css --minify
v=$(md5 -q styles.css | cut -c1-8); sed -i '' -E "s#styles.css\?v=[0-9a-f]+#styles.css?v=$v#" index.html
```

Servers update their agent with:

```bash
curl -fsSL https://clusterheart-server.clusterheart.com/install-host.sh | sudo sh -s -- --update
```

## Run locally

```bash
# control plane (in-memory store, no MongoDB needed)
cd sre && CONTROLPLANE_ADMIN_TOKEN=dev-admin ADDR=127.0.0.1:9100 go run ./cmd/controlplane

# web app + accounts (needs MongoDB; copy server/.env.example to server/.env)
cd server && go run .        # http://127.0.0.1:9000
```

Tests: `go test ./...` in `server/` and `sre/`. The MongoDB-backed tests run
when `MONGO_TEST_URI` is set (e.g. `mongodb://127.0.0.1:27017`).

More: [`sre/README.md`](sre/README.md) (agents and control plane),
[`docs/connect-a-cluster.md`](docs/connect-a-cluster.md),
[`docs/autonomous-sre/`](docs/autonomous-sre/) (design and threat model).
