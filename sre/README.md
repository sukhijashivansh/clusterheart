# ClusterHeart Autonomous SRE — `sre/` module

One Go module, two binaries:

- `cmd/controlplane` — multi-tenant API: cluster registration, agent heartbeats,
  telemetry ingestion, operator read-back.
- `cmd/agent` — in-cluster, read-only collector that forwards bounded cluster-health
  snapshots to the control plane.

**Milestone 1 status: observe-only plumbing.** No detection, AI, policy, or
remediation yet — see `../docs/autonomous-sre/implementation-plan.md`.

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

Agent:

| Env | Meaning |
|---|---|
| `CONTROLPLANE_URL` | control-plane base URL (required) |
| `CLUSTER_ID`, `AGENT_TOKEN` | from registration (required) |
| `KUBECONFIG` | path; empty → in-cluster config |
| `COLLECT_INTERVAL` | e.g. `30s` (min `5s`) |

## In-cluster install (agent, observe-only)

See `../deploy/k8s/agent/agent.yaml` — read-only RBAC, hardened securityContext.
Create the `clusterheart-agent` Secret with the registration values first.

## Build & test

```bash
cd sre
go build ./...
go test ./...
```

## Tested vs. not (be honest)
- **Tested:** control-plane HTTP flow (register/heartbeat/telemetry/list) via
  httptest + a compiled-binary smoke run; tenant isolation; version/bounds
  validation; redaction; backoff/jitter.
- **Not yet tested against a live cluster:** `agent.Collector.Snapshot` (needs a
  real kube API). Verify on a disposable kind/minikube cluster before relying on it.
