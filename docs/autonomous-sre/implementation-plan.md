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

**Live-cluster validation (2026-10-11):** M1–M3 were exercised end-to-end on a
real `kind` cluster (Kubernetes v1.31.0) with a Mongo-backed control plane —
agent telemetry → real incident detection → policy/approval → in-cluster
controller → real `RolloutRestart` (verify pass *and* fail) → durable audit,
surviving a control-plane restart. Reproducible runbook + observed output:
`e2e-kind-validation.md`.

## Milestone 3 — Remediation Framework
Slice A (safety spine) — done & tested on this branch. **No live cluster mutation**: execution is dry-run only, read-only by default.
- [x] Typed action registry; first action `RolloutRestart` (reversible, controller-owned). Unknown/invalid actions rejected — no arbitrary patches/shell.
- [x] Deterministic **policy engine** (no LLM): action + namespace allowlists (deny-by-default), risk class, max-concurrent, per-workload cooldown, production protection (auto→approval), emergency pause/kill-switch.
- [x] Full action contract (`RemediationProposal`): evidence/rationale, risk, reversibility, policy decision, before-state, execution result, rollback availability.
- [x] Proposal lifecycle + API: create → policy-evaluate → blocked/proposed; approve (re-evaluates preconditions) → dry-run execute; reject. Policy get/put + pause/resume; audit API.
- [x] Append-only **audit trail** for every decision + (dry-run) execution; tenant-scoped.
- [x] Tested: policy allow/deny, approval + dry-run execution, unknown/invalid action rejection, kill-switch, cooldown, production downgrade, reject flow, tenant isolation, audit. Runtime E2E through the HTTP binary.

Slice B (the executor) — done & tested on this branch. Execution now reaches a real cluster via a separate, narrowly-privileged controller; the control plane itself still never touches a cluster.
- [x] In-cluster **Remediation Controller** binary (`cmd/remediation`) with a separate narrow `remediate` ServiceAccount + RBAC manifest (`deploy/k8s/remediation/` — `get,update` on Deployments/StatefulSets/DaemonSets only).
- [x] Separate per-cluster **remediation token** (distinct identity from the observe agent), minted on explicit opt-in (`POST /clusters/{id}/remediation-token`); controller pull/report protocol (`GET /remediation/pending`, `POST /remediation/{id}/result`).
- [x] Real `RolloutRestart` via client-go for Deployment/StatefulSet/DaemonSet; resourceVersion (optimistic concurrency) check immediately before apply; before-state capture; dry-run validated against the API server (no persisted change).
- [x] Post-action health verification (rollout completion for Deployments); failure handling → `failed` status; cross-cluster/replay report rejection. Rollback availability recorded (restart is reversible); automatic rollback execution deferred to M4.
- [x] Tested: apply + verify, dry-run (no verification), not-found, verification-failure → failed, unsupported kind/resource rejected, StatefulSet apply; control-plane side approval→pending→report (success + failure), replay/cross-cluster rejection, cooldown keyed on reported execution time.
Slice C (durable state) — done & tested on this branch:
- [x] Mongo persistence for proposals/policy/audit (`remediation_mongo.go`); domain structs carry bson tags (no parallel doc type). Tenant-scoped reads; indexes for the policy engine's concurrency/cooldown queries. In-memory stores remain the local-dev/test default; the caller injects the implementation via `NewServer`.
- [x] Integration-tested against a real MongoDB (gated on `MONGO_TEST_URI`, skipped in the hermetic suite): proposal round-trip + tenant scoping, `ActiveCount`, `LastExecutionForTarget` sort, `map[ActionType]bool` policy round-trip, audit scoping.

## Milestone 4 — Guarded Autonomy
Slice A (guardrails) — done & tested on this branch:
- [x] Allowlisted automatic execution (mode=auto → proposal created pre-approved → the in-cluster controller applies it; established in M3 slice B). Per-workload cooldown (M3 slice A) and max-concurrent caps already enforced deterministically.
- [x] **Circuit breaker / loop detection**: once a target accumulates N failed remediations within a window (default 3 / 30m, policy-configurable), further proposals for that target are denied — a recurring incident can no longer drive an endless restart loop. Store-backed failure count (mem + Mongo), evaluated in the deterministic engine; unit-tested (breaker opens, other targets unaffected) + Mongo-integration-tested.
- [x] Emergency pause / kill switch at the **tenant** level + global per-tenant automation pause (M3 slice A).

Slice B (remaining) — not started:
- [ ] Cluster- and workload-scoped kill switches (currently tenant-scoped only).
- [ ] Failure-injection test matrix across the catalog (spec §19 scenarios) as the action set grows beyond RolloutRestart.

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
