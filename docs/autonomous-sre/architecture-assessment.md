# ClusterHeart Autonomous SRE - Architecture Assessment

Status: living document. Grounded in the repository as of this branch.

## 1. What exists today

| Component | Location | Role | Notes |
|---|---|---|---|
| Web app + accounts | `server/` (Go) | Dashboard at `/app`, sign-up/login/approval, teams, admin | MongoDB; hosted on AWS (EC2 + Caddy), fronted by Vercel; reads everything from the control plane |
| Control plane + agents | `sre/` (Go) | Clusters, servers, metric history, incidents, remediation | MongoDB; see below |
| Landing page | `index.html` + `vercel.json` | Marketing site | Vercel + Cloudflare |

## 2. Target (from the product spec) & the gap

The spec describes a **multi-tenant SaaS Control Plane + per-cluster Agents + a narrowly-privileged Remediation Controller**, with deterministic policy, approvals, audit, incident detection/correlation, AI root-cause analysis emitting *structured* actions, verification/rollback, SLOs, runbooks, and notifications.

The original product was a single-user, single-cluster, read-mostly dashboard. The re-platform (chosen direction) introduces three new long-lived services and a strict trust boundary between AI reasoning and cluster mutation.

## 3. Chosen architecture (re-platform)

```
                         ┌──────────────────────────────────────────┐
                         │            Control Plane (sre/)           │
  operators / UI  ─────▶ │  multi-tenant API · incident store ·      │
                         │  policy engine · AI RCA orchestration ·   │
                         │  approvals · audit · analytics            │
                         └───────▲───────────────────────┬──────────┘
                                 │ telemetry/heartbeat    │ approved, typed
                                 │ (agent token, TLS)     │ remediation intents
          per cluster:           │                        ▼
   ┌─────────────────────────────┴───┐        ┌──────────────────────────────┐
   │  Agent (read-only SA)            │        │  Remediation Controller       │
   │  collects events/metrics/state   │        │  (narrow remediate SA)        │
   │  buffers + forwards telemetry     │        │  validates vs policy, checks  │
   │  reports health                   │        │  preconditions, executes,     │
   └──────────────────────────────────┘        │  verifies, rolls back, audits │
                                                 └──────────────────────────────┘
```

Key invariants (enforced in code, not convention):
- **The AI never executes anything.** It proposes schema-validated, typed actions. Deterministic policy code decides; the Remediation Controller executes.
- **Observation and remediation use different ServiceAccounts.** Default deployment installs only the read-only observe SA.
- **Tenant isolation at the trust boundary**: an agent credential can only write telemetry for its own cluster/tenant; queries are tenant-scoped.
- **Telemetry is untrusted input** (possible prompt injection); it can never authorize an action or alter policy.

## 4. Data stores
- **MongoDB** is already the project's datastore (account server + usage history). The control plane reuses it for tenants, clusters, agents, incidents, proposals, approvals, executions, and audit - consistent with existing conventions.
- Raw logs/metrics are **not** dumped into the primary DB. Incidents reference bounded evidence snapshots; long-horizon metrics/logs are delegated to purpose-built backends (Prometheus/OTel/Loki) via integrations in a later phase.

## 5. Security posture (summary; full threat model: `threat-model.md`)
- Per-cluster agent identity (bearer token now, mTLS as the hardening step).
- Separated observe/remediate RBAC; no cluster-admin.
- Redaction of secrets/tokens before any external model call.
- Full audit trail for every decision and mutation.
- Emergency pause / kill switch at org, cluster, and workload scope.
