# ClusterHeart Autonomous SRE — Threat Model (living)

Scope grows with the implementation. This reflects Milestone 1 (observe-only
control plane + agent) and flags what later milestones must address.

## Trust boundaries
1. **Agent ↔ Control plane** — crosses the network / tenant boundary. Highest risk.
2. **Telemetry ↔ Reasoning** — telemetry is attacker-influenceable (logs, events,
   annotations). It must never authorize actions or alter policy.
3. **Reasoning (AI) ↔ Execution** — the AI proposes; deterministic code decides and
   executes. The AI has no credentials to mutate a cluster.
4. **Observe SA ↔ Remediate SA** — separate identities; remediation installed only on opt-in.

## Milestone-1 threats & mitigations (implemented)
| Threat | Mitigation | Status |
|---|---|---|
| Stolen/forged agent credential writes as another cluster | Per-cluster bearer token; server pins requests to the authenticated cluster (`X-Cluster-ID` ignored for authz); constant-time hash compare | Implemented + tested (`TestTenantIsolation`, runtime smoke 401) |
| Token theft from the DB | Only SHA-256 hash stored; plaintext shown once | Implemented |
| Cross-tenant data read | Listing is tenant-scoped by `tenantId` | Implemented + tested |
| Secrets/PII exfiltrated via event text | Redaction in the agent before telemetry leaves the cluster | Implemented + tested (`TestRedact`) |
| Unbounded telemetry / memory growth | Bounded agent buffer (drop-oldest), per-batch cap, DB TTL index | Implemented |
| Agent cannot mutate the cluster | Observe-only RBAC (no mutating verbs, no exec/portforward, no secrets) | Implemented (`deploy/k8s/agent/agent.yaml`) |
| DoS via huge request bodies | 1 MiB body cap; `DisallowUnknownFields` | Implemented |
| Incompatible agent version | `apiVersion` checked on heartbeat/telemetry (409) | Implemented |
| Thundering-herd reconnect | Exponential backoff + jitter | Implemented + tested |

## Known gaps / deferred (must address before production automation)
- **mTLS + per-cluster certs** — currently bearer-over-TLS. (M6)
- **Admin authz** — single bootstrap admin token; integrate with the account
  server / RBAC and per-tenant admin scoping. (M2+)
- **Prompt injection** — no AI yet; when added, telemetry is data-only, never
  instructions; structured output schemas; policy decisions independent of the model. (M2)
- **Remediation safety** — no mutation exists yet; the full guardrail subsystem
  (policy engine, approvals, rate limits, circuit breakers, kill switch,
  rollback, verification) lands with M3/M4 before any action is executable.
- **Signed agent updates**, **replay protection (nonce/timestamp)**, **rate
  limiting per cluster**. (M6)
- **At-rest encryption** of sensitive fields beyond token hashing. (M6)

## Non-goals (M1)
No detection, no incidents, no AI, no remediation, no cluster mutation. Nothing
in M1 can change a cluster.
