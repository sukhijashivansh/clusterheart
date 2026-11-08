# ClusterHeart Autonomous SRE - Architecture Assessment

Status: living document. Grounded in the repository as of this branch.

> **Update 2026-11-09:** the desktop app (`electron/`) and the local dashboard
> engine (`app/`) were retired; the product is now the web app in `server/`
> backed by this control plane. Rows below that mention them are historical.

## 1. What exists today

| Component | Location | Role | Notes |
|---|---|---|---|
| Dashboard engine | `app/` (Go module `kubepulse-ui`) | Reads a live cluster, serves the UI | client-go v0.34 + metrics; runs **in-cluster** (`rest.InClusterConfig`) or from a kubeconfig |
| Usage history | `app/history.go` | Rolling usage time-series | MongoDB `clusterheart.usage_samples`, 8h TTL |
| Account server | `server/` (Go) | Sign-up / login / approval gate | MongoDB; hosted on AWS (EC2 + Caddy), fronted by Vercel; approves who may run the desktop app |
| Desktop app | `electron/` | Wraps the dashboard, gates on approval | |
| Landing page | `index.html` + `vercel.json` | Marketing site | Vercel + Cloudflare |

### Current `app/` capabilities (verified)
- **Read-mostly** API (20 routes): pods, jobs, nodes, hpas, namespaces, stats, rightsizing, costs, gemini (AI), shell (exec), port-forward, contexts.
- **Mutating surface is tiny**: pod `Delete` and a namespace "cleanse"; plus `exec` / `port-forward` access. Rightsizing is **recommend-only** (no `Patch`/`Update`/`Scale` in the backend).
- **RBAC**: a single combined ClusterRole (read + exec + delete) - not the separated observe/execute model the spec requires.
- **AI**: `/api/gemini` → direct Gemini/OpenRouter call. No structured-action schema, no validation, no guardrails.
- **Auth**: the backend trusts localhost / in-cluster; it has no multi-tenant authn/authz of its own.

## 2. Target (from the product spec) & the gap

The spec describes a **multi-tenant SaaS Control Plane + per-cluster Agents + a narrowly-privileged Remediation Controller**, with deterministic policy, approvals, audit, incident detection/correlation, AI root-cause analysis emitting *structured* actions, verification/rollback, SLOs, runbooks, and notifications.

Today's product is a **single-user, single-cluster, read-mostly dashboard**. The re-platform (chosen direction) introduces three new long-lived services and a strict trust boundary between AI reasoning and cluster mutation.

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

## 4. Why the existing `app/` is not discarded
`app/` already has working, battle-tested client-go collection, in-cluster deploy, a usage time-series, and an AI integration. The agent reuses its collection patterns; rightsizing math and the dashboard remain valuable. The re-platform wraps these in the control-plane/agent trust model rather than rewriting them.

## 5. Data stores
- **MongoDB** is already the project's datastore (account server + usage history). The control plane reuses it for tenants, clusters, agents, incidents, proposals, approvals, executions, and audit - consistent with existing conventions.
- Raw logs/metrics are **not** dumped into the primary DB. Incidents reference bounded evidence snapshots; long-horizon metrics/logs are delegated to purpose-built backends (Prometheus/OTel/Loki) via integrations in a later phase.

## 6. Security posture (summary; full threat model: `threat-model.md`)
- Per-cluster agent identity (bearer token now, mTLS as the hardening step).
- Separated observe/remediate RBAC; no cluster-admin.
- Redaction of secrets/tokens before any external model call.
- Full audit trail for every decision and mutation.
- Emergency pause / kill switch at org, cluster, and workload scope.
