# ClusterHeart Autonomous SRE — Implementation Plan

Incremental, reviewable milestones. Each milestone compiles and is tested before the next.
`[x]` = implemented & tested on this branch · `[ ]` = planned.

## Milestone 1 — Foundation (this slice)
Control plane + agent skeleton with a real register → heartbeat → telemetry → read-back loop.

- [x] New `sre/` Go module (one module, two binaries: `cmd/controlplane`, `cmd/agent`).
- [x] Wire contracts (`internal/contract`): registration, heartbeat, telemetry snapshot.
- [x] Control-plane storage behind a `Store` interface (Mongo impl + in-memory impl for tests).
- [x] Multi-tenant data model: org/tenant → cluster → agent; telemetry snapshots.
- [x] Auth: bootstrap admin token (register/list) + per-cluster agent token (hashed at rest).
- [x] **Tenant isolation enforced at the agent boundary** (agent for cluster A cannot write as B) — unit tested.
- [x] Agent: in-cluster/kubeconfig client-go collector (node/pod/event health snapshot), bounded buffer, backoff+jitter, heartbeat.
- [x] Least-privilege **observe-only** RBAC + agent Deployment manifest (`deploy/k8s/agent/`).
- [x] Health endpoints; structured logging.
- [x] Unit tests: auth, tenant isolation, telemetry validation, agent backoff, collector snapshot shape.

## Milestone 2 — Incident Intelligence
Slice A (detection) — done & tested on this branch:
- [x] Incident data model + bounded evidence items.
- [x] Deterministic detectors: CrashLoopBackOff, OOMKilled, ImagePullBackOff, Unschedulable, RolloutStuck (from pod/deployment status reported by the agent).
- [x] Dedup via stable fingerprint; ReplicaSet→Deployment collapse so incidents survive rollouts.
- [x] Open/resolved lifecycle (auto-resolve when a fault clears); timeline (firstSeen/lastSeen/occurrences); deterministic severity.
- [x] Defense-in-depth redaction at the control-plane ingestion boundary (not just the agent).
- [x] Incident API (list + get), tenant-scoped; unit-tested + runtime E2E (create → dedup → resolve → redaction).

Slice B (remaining in M2) — not started:
- [ ] Probe-failure + missing-telemetry/agent-down detectors.
- [ ] Cross-signal correlation (group by node/namespace/dependency); cascading-failure + symptom-vs-cause heuristics.
- [ ] Explicit confidence field; maintenance-window suppression; alert inhibition.
- [ ] **AI RCA path** emitting schema-validated structured proposals (never free-form patches/shell); provider abstraction + redaction + "telemetry is data, not instructions".

## Milestone 3 — Remediation Framework
- [ ] Typed remediation registry; first action: `rollout restart` (reversible, controller-owned).
- [ ] Deterministic **policy engine** (independent of the LLM): allowlists, scope, risk class, rate/cooldown/concurrency.
- [ ] Modes: dry-run, recommend, approval-required.
- [ ] Remediation Controller service (separate `remediate` SA) with precondition + resourceVersion checks.
- [ ] Post-action health verification + audit records + rollback for reversible actions.

## Milestone 4 — Guarded Autonomy
- [ ] Allowlisted automatic execution; circuit breakers, loop detection, cooldowns.
- [ ] Emergency pause / kill switch (org/cluster/workload); global automation pause.
- [ ] Failure-injection tests for the catalog.

## Milestone 5 — Advanced
- [ ] Multi-cluster incident correlation; SLOs/error budgets; runbooks; notifications (Slack/PagerDuty/email).
- [ ] FinOps integration (reuse existing cost engine); provider-abstracted AI with eval harness.

## Milestone 6 — Production Hardening
- [ ] mTLS + per-cluster certs; signed agent updates; threat-model review.
- [ ] Load/resilience/tenant-isolation tests; upgrade/rollback runbooks; ops docs.

## Operational modes (spec §20) — rollout
Observe (M1) → Diagnose (M2) → Recommend (M2/3) → Approval-Required (M3) → Guarded Autonomy (M4) → Emergency Pause (M4).

## Non-goals for M1 (explicitly deferred)
No detection, no incidents, no AI, no remediation, no mutation. M1 is observe-only plumbing. This is called out so the implementation report never overstates what exists.
