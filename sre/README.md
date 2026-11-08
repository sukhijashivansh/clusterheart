# ClusterHeart Autonomous SRE - `sre/` module

One Go module, four binaries:

- `cmd/controlplane` - multi-tenant API: cluster registration, agent heartbeats,
  telemetry ingestion, incident detection, remediation policy/approval/audit,
  operator read-back. Never touches a cluster directly.
- `cmd/agent` - in-cluster, read-only collector that forwards bounded cluster-health
  snapshots to the control plane.
- `cmd/hostagent` - read-only Linux server agent (systemd unit installed by
  `/install-host.sh`): CPU, memory, disks, load, network, processes, error
  logs, NVIDIA/AMD GPUs, and leaked credentials in the journal or services' Environment= settings (masked, never the value).
- `cmd/remediation` - in-cluster, narrowly-privileged controller (opt-in, separate
  credential) that pulls already-approved typed actions, executes them (dry-run or
  real per policy), verifies, and reports back. No detection, no AI, no arbitrary
  commands.

**Status:** observe (M1) → deterministic detection (M2 slice A) → remediation
safety spine + in-cluster executor (M3 slices A/B). See
`../docs/autonomous-sre/implementation-plan.md` for what is / isn't done.

## Local development

Run the control plane with the in-memory store (no MongoDB needed):

```bash
cd sre
CONTROLPLANE_ADMIN_TOKEN=dev-admin ADDR=127.0.0.1:9100 go run ./cmd/controlplane
```

Register a cluster (operator / admin):

```bash
curl -s -X POST localhost:9100/api/v1/clusters \
  -H "Authorization: Bearer dev-admin" -H "Content-Type: application/json" \
  -d '{"tenantId":"acme","clusterName":"dev","environment":"dev"}'
# -> { "clusterId": "clu_...", "agentToken": "..." }   (token shown ONCE)
```

Run the agent against a kubeconfig cluster (e.g. kind/minikube) from your laptop:

```bash
CONTROLPLANE_URL=http://localhost:9100 \
CLUSTER_ID=clu_... AGENT_TOKEN=... \
KUBECONFIG=$HOME/.kube/config \
go run ./cmd/agent
```

Read back status:

```bash
curl -s "localhost:9100/api/v1/clusters?tenantId=acme" -H "Authorization: Bearer dev-admin" | jq
```

## Production config

Control plane (durable store):

| Env | Meaning |
|---|---|
| `CONTROLPLANE_ADMIN_TOKEN` | bootstrap operator token (required) |
| `MONGO_URI` | MongoDB connection string (omit → in-memory, non-durable) |
| `MONGO_DB` | database name (default `clusterheart`) |
| `ADDR` / `PORT` | listen address |
| `SRE_AI_PROVIDER` / `SRE_AI_API_KEY` / `SRE_AI_MODEL` / `SRE_AI_BASE_URL` | opt-in external AI for root-cause analysis (OpenAI/OpenRouter-style). Unset → deterministic diagnoser only |
| `SRE_SLACK_WEBHOOK_URL` | Slack incoming webhook for incident/remediation notifications (optional) |
| `SRE_WEBHOOK_URL` | generic JSON webhook for notifications (optional) |

Agent:

| Env | Meaning |
|---|---|
| `CONTROLPLANE_URL` | control-plane base URL (required) |
| `CLUSTER_ID`, `AGENT_TOKEN` | from registration (required) |
| `KUBECONFIG` | path; empty → in-cluster config |
| `COLLECT_INTERVAL` | e.g. `30s` (min `5s`) |

## Remediation controller (opt-in)

A cluster is opted into remediation by minting a SEPARATE remediation token
(shown once), distinct from the observe-agent token:

```bash
curl -s -X POST localhost:9100/api/v1/clusters/clu_.../remediation-token \
  -H "Authorization: Bearer dev-admin" | jq
# -> { "clusterId": "clu_...", "remediationToken": "..." }
```

Run the controller against a kubeconfig cluster. It polls for already-approved
actions, executes them (dry-run or real per policy), verifies, and reports back:

```bash
CONTROLPLANE_URL=http://localhost:9100 \
CLUSTER_ID=clu_... REMEDIATION_TOKEN=... \
KUBECONFIG=$HOME/.kube/config \
go run ./cmd/remediation
```

| Env | Meaning |
|---|---|
| `CONTROLPLANE_URL` | control-plane base URL (required) |
| `CLUSTER_ID`, `REMEDIATION_TOKEN` | from the mint call above (required) |
| `KUBECONFIG` | path; empty → in-cluster config |
| `POLL_INTERVAL` | e.g. `15s` (min `5s`) |

## In-cluster install

Agent (observe-only): see `../deploy/k8s/agent/agent.yaml` - read-only RBAC,
hardened securityContext. Create the `clusterheart-agent` Secret with the
registration values first.

Remediation controller (opt-in): see `../deploy/k8s/remediation/remediation.yaml`
- separate ServiceAccount, `get,update` on Deployments/StatefulSets/DaemonSets
only. Create the `clusterheart-remediation` Secret with the mint-call values
first.

## Build & test

```bash
cd sre
go build ./...
go test ./...
```

## Tested vs. not (be honest)
- **Tested:** control-plane HTTP flow (register/heartbeat/telemetry/list) via
  httptest + a compiled-binary smoke run; tenant isolation; version/bounds
  validation; redaction; backoff/jitter. Deterministic detectors + incident
  lifecycle. Remediation policy (allow/deny, cooldown, concurrency, production
  downgrade, kill-switch) + approval→pending→report flow. The **executor** against
  a `fake.Clientset`: RolloutRestart apply + verify, dry-run, not-found,
  verification-failure, unsupported kind, StatefulSet.
- **Validated on a live `kind` cluster (Kubernetes v1.31.0):** the full loop -
  agent collector → deterministic detection (CrashLoopBackOff/RolloutStuck) →
  policy → approval → controller → real `RolloutRestart` → verification (pass
  *and* fail paths) → durable audit, through a Mongo-backed control plane,
  surviving a control-plane restart. Reproducible runbook + observed output:
  `../docs/autonomous-sre/e2e-kind-validation.md`.
- **Durable when `MONGO_URI` is set:** clusters/telemetry, incidents, and
  remediation proposals/policy/audit all persist to MongoDB; without it the
  control plane falls back to in-memory stores (local dev only). The Mongo
  remediation stores are integration-tested against a real MongoDB
  (`MONGO_TEST_URI=... go test ./internal/controlplane -run Mongo`).
