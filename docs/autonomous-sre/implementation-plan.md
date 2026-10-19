# ClusterHeart Autonomous SRE - Implementation Plan

Incremental, reviewable milestones. Each milestone compiles and is tested before the next.
`[x]` = implemented & tested on this branch · `[ ]` = planned.

## Milestone 1 - Foundation (this slice)
Control plane + agent skeleton with a real register → heartbeat → telemetry → read-back loop.

- [x] New `sre/` Go module (one module, two binaries: `cmd/controlplane`, `cmd/agent`).
- [x] Wire contracts (`internal/contract`): registration, heartbeat, telemetry snapshot.
- [x] Control-plane storage behind a `Store` interface (Mongo impl + in-memory impl for tests).
- [x] Multi-tenant data model: org/tenant → cluster → agent; telemetry snapshots.
- [x] Auth: bootstrap admin token (register/list) + per-cluster agent token (hashed at rest).
- [x] **Tenant isolation enforced at the agent boundary** (agent for cluster A cannot write as B) - unit tested.
- [x] Agent: in-cluster/kubeconfig client-go collector (node/pod/event health snapshot), bounded buffer, backoff+jitter, heartbeat.
- [x] Least-privilege **observe-only** RBAC + agent Deployment manifest (`deploy/k8s/agent/`).
- [x] Health endpoints; structured logging.
- [x] Unit tests: auth, tenant isolation, telemetry validation, agent backoff, collector snapshot shape.

## Milestone 2 - Incident Intelligence
Slice A (detection) - done & tested on this branch:
- [x] Incident data model + bounded evidence items.
- [x] Deterministic detectors: CrashLoopBackOff, OOMKilled, ImagePullBackOff, Unschedulable, RolloutStuck (from pod/deployment status reported by the agent).
- [x] Dedup via stable fingerprint; ReplicaSet→Deployment collapse so incidents survive rollouts.
- [x] Open/resolved lifecycle (auto-resolve when a fault clears); timeline (firstSeen/lastSeen/occurrences); deterministic severity.
- [x] Defense-in-depth redaction at the control-plane ingestion boundary (not just the agent).
- [x] Incident API (list + get), tenant-scoped; unit-tested + runtime E2E (create → dedup → resolve → redaction).

Slice B (AI root-cause analysis) - done & tested on this branch:
- [x] **AI RCA** attaches an evidence-backed `Diagnosis` to an incident (summary, measured observed-facts, ranked hypotheses with supporting/contradicting evidence, recommended action, risks, verification criteria, rollback, confidence). Facts vs. inferred hypotheses kept distinct.
- [x] **Provider abstraction** (`Diagnoser`): a deterministic diagnoser (always on, default, covers the 5 detected types) + an opt-in external-AI diagnoser (OpenAI/OpenRouter-style, env-gated). External AI disabled unless configured; on any error/invalid output the service **falls back** to deterministic (graceful degradation).
- [x] **Safety**: AI returns schema-validated structured JSON only - never shell/patches; a recommended executable action must be a known action type and is **forced** onto the incident's own workload (a hallucinated action/target is neutralised to advisory "investigate"). A diagnosis NEVER creates/authorizes a remediation - the policy-checked proposal+approval path is still required. Telemetry is wrapped as untrusted DATA (prompt-injection hardening) and re-redacted before inference. Model/promptVersion/latency recorded.
- [x] Durable `DiagnosisStore` (mem + Mongo, one-per-incident upsert, tenant-scoped). Endpoints `POST /incidents/{id}/diagnose` + `GET /incidents/{id}/diagnosis`. Surfaced in the dashboard (incidents view → "diagnose" → readable RCA). Tested: deterministic per-type, LLM parse/validate/fallback/injection (stubbed chat, no network), forged-action/target rejection, Mongo round-trip, HTTP E2E; dashboard proxy unit-tested.

Slice C (remaining detection work in M2) - partly done on this branch:
- [x] **Agent-down / missing-telemetry detection** (spec §5, §17): a control-plane background sweep (`RunAgentHealthSweeps`) raises an `AgentDown` incident when a previously-connected cluster stops heartbeating beyond `agentDownAfter` (3m) - so the platform never silently reports a silent cluster as healthy. Deduped by fingerprint; auto-resolves when telemetry resumes (normal resolve pass). Notifies on open. Needs `Store.AllClusters` (mem + Mongo). Unit-tested (never-connected not alarmed, stale → one incident, dedup, recovery).
- [ ] Probe-failure detector.
- [ ] Cross-signal correlation (group by node/namespace/dependency); cascading-failure + symptom-vs-cause heuristics.
- [ ] Maintenance-window suppression; alert inhibition.
- [ ] Evaluation harness measuring diagnostic quality / false-diagnosis rate against a labelled set (spec §15, §21).

**Live-cluster validation (2026-10-11):** M1-M3 were exercised end-to-end on a
real `kind` cluster (Kubernetes v1.31.0) with a Mongo-backed control plane -
agent telemetry → real incident detection → policy/approval → in-cluster
controller → real `RolloutRestart` (verify pass *and* fail) → durable audit,
surviving a control-plane restart. Reproducible runbook + observed output:
`e2e-kind-validation.md`.

## Milestone 3 - Remediation Framework
Slice A (safety spine) - done & tested on this branch. **No live cluster mutation**: execution is dry-run only, read-only by default.
- [x] Typed action registry; first action `RolloutRestart` (reversible, controller-owned). Unknown/invalid actions rejected - no arbitrary patches/shell.
- [x] Deterministic **policy engine** (no LLM): action + namespace allowlists (deny-by-default), risk class, max-concurrent, per-workload cooldown, production protection (auto→approval), emergency pause/kill-switch.
- [x] Full action contract (`RemediationProposal`): evidence/rationale, risk, reversibility, policy decision, before-state, execution result, rollback availability.
- [x] Proposal lifecycle + API: create → policy-evaluate → blocked/proposed; approve (re-evaluates preconditions) → dry-run execute; reject. Policy get/put + pause/resume; audit API.
- [x] Append-only **audit trail** for every decision + (dry-run) execution; tenant-scoped.
- [x] Tested: policy allow/deny, approval + dry-run execution, unknown/invalid action rejection, kill-switch, cooldown, production downgrade, reject flow, tenant isolation, audit. Runtime E2E through the HTTP binary.

Slice B (the executor) - done & tested on this branch. Execution now reaches a real cluster via a separate, narrowly-privileged controller; the control plane itself still never touches a cluster.
- [x] In-cluster **Remediation Controller** binary (`cmd/remediation`) with a separate narrow `remediate` ServiceAccount + RBAC manifest (`deploy/k8s/remediation/` - `get,update` on Deployments/StatefulSets/DaemonSets only).
- [x] Separate per-cluster **remediation token** (distinct identity from the observe agent), minted on explicit opt-in (`POST /clusters/{id}/remediation-token`); controller pull/report protocol (`GET /remediation/pending`, `POST /remediation/{id}/result`).
- [x] Real `RolloutRestart` via client-go for Deployment/StatefulSet/DaemonSet; resourceVersion (optimistic concurrency) check immediately before apply; before-state capture; dry-run validated against the API server (no persisted change).
- [x] Post-action health verification (rollout completion for Deployments); failure handling → `failed` status; cross-cluster/replay report rejection. Rollback availability recorded (restart is reversible); automatic rollback execution deferred to M4.
- [x] Tested: apply + verify, dry-run (no verification), not-found, verification-failure → failed, unsupported kind/resource rejected, StatefulSet apply; control-plane side approval→pending→report (success + failure), replay/cross-cluster rejection, cooldown keyed on reported execution time.
Slice C (durable state) - done & tested on this branch:
- [x] Mongo persistence for proposals/policy/audit (`remediation_mongo.go`); domain structs carry bson tags (no parallel doc type). Tenant-scoped reads; indexes for the policy engine's concurrency/cooldown queries. In-memory stores remain the local-dev/test default; the caller injects the implementation via `NewServer`.
- [x] Integration-tested against a real MongoDB (gated on `MONGO_TEST_URI`, skipped in the hermetic suite): proposal round-trip + tenant scoping, `ActiveCount`, `LastExecutionForTarget` sort, `map[ActionType]bool` policy round-trip, audit scoping.

### Remediation catalog (spec §7) - growing
- [x] `RolloutRestart` (Deployment/StatefulSet/DaemonSet) - M3.
- [x] `Scale` (Deployment/StatefulSet replica count), **bounded** by policy `MaxReplicas` (default 10) + a structural hard ceiling; before-count captured (reversible); dry-run + post-scale verification; executor tested against a fake clientset incl. a status reactor. Uses the existing remediate RBAC (`get,update`) - no new permissions.
- [ ] Resource rightsizing (requests/limits), scheduling/config fixes - future.

## Milestone 4 - Guarded Autonomy
Slice A (guardrails) - done & tested on this branch:
- [x] Allowlisted automatic execution (mode=auto → proposal created pre-approved → the in-cluster controller applies it; established in M3 slice B). Per-workload cooldown (M3 slice A) and max-concurrent caps already enforced deterministically.
- [x] **Circuit breaker / loop detection**: once a target accumulates N failed remediations within a window (default 3 / 30m, policy-configurable), further proposals for that target are denied - a recurring incident can no longer drive an endless restart loop. Store-backed failure count (mem + Mongo), evaluated in the deterministic engine; unit-tested (breaker opens, other targets unaffected) + Mongo-integration-tested.
- [x] Emergency pause / kill switch at the **tenant** level + global per-tenant automation pause (M3 slice A).

Slice B - partly done on this branch:
- [x] **Cluster- and workload-scoped kill switches** (spec §8): `POST /policy/pause|resume` with optional `?clusterId=` or `?workload=Kind/Namespace/Name` scopes the emergency stop; the deterministic engine denies proposals whose cluster or workload is paused (tenant pause still overrides all). Persisted in policy (mem + Mongo); audited with scope. Unit-tested (workload pause isolates one workload; cluster pause blocks all its workloads).
- [ ] Failure-injection test matrix across the catalog (spec §19 scenarios).

## Milestone 5 - Advanced
Slice A (notifications) - done & tested on this branch:
- [x] **Notifications**: `Notifier` abstraction + Slack incoming-webhook and generic JSON webhook (env-gated: `SRE_SLACK_WEBHOOK_URL`, `SRE_WEBHOOK_URL`; no-op when unset). Fire-and-forget (never blocks/fails detection or remediation; delivery failures logged). Emitted on **incident.opened** (deduped - once per incident via the store's created-signal, not per occurrence → no storms) and on **remediation.executed/failed**. Tested: Slack + generic send (httptest), no-op default, once-per-incident dedup.

Slice B (runbooks) - done & tested on this branch:
- [x] **Runbooks** (§12): operator-authored, versioned playbooks (one per tenant+incidentType → typed action + params + enabled flag). `GET/PUT /api/v1/runbooks`; action+params validated on save (unknown action rejected); version auto-increments. An enabled runbook takes precedence over the AI recommendation in propose-from-diagnosis, but the **target is always the incident's own workload** and proposing still flows through policy-checked CreateProposal - a runbook cannot bypass policy/approval. Durable (mem + Mongo). HTTP-tested (reject bad action, versioning, Scale-runbook overrides AI's RolloutRestart).

Slice C (remaining) - not started:
- [ ] Multi-cluster incident correlation; SLOs/error budgets; PagerDuty/email routing + maintenance-window muting.
- [ ] FinOps integration (reuse existing cost engine); diagnostic-quality eval harness.

## Milestone 6 - Production Hardening
- [ ] mTLS + per-cluster certs; signed agent updates; threat-model review.
- [ ] Load/resilience/tenant-isolation tests; upgrade/rollback runbooks; ops docs.

## Dashboard UI (spec §14) - slice A (done & tested on this branch)
Surfaces the SRE control plane in the existing ClusterHeart dashboard (`app/`),
matching its design system (no new framework).
- [x] **Incident Center**: `incidents` view - severity/type/workload/namespace/count/timeline/status, status filter + search, row → full evidence JSON.
- [x] **Remediation Center**: `remediation` view - approval queue with approve/reject on `proposed` actions, execution result (ok/failed, dry-run, error) for terminal ones, policy mode + emergency pause/resume (kill switch).
- [x] Server-side proxy (`app/sre.go`, `/api/sre/*`): keeps the control-plane **admin token server-side** (never in the renderer) and **pins the tenant** (a forged `tenantId` is ignored); unconfigured → explicit "not configured" notice, never fabricated data; upstream status/errors passed through.
- [x] Tested: proxy unit tests (token-not-leaked, tenant-pinned, approve forwarding, unknown-action reject, 503 when unconfigured, upstream error passthrough) + a live run of the real dashboard binary against the real control plane (config/incidents/proposals/approve/pause/resume/audit all verified over HTTP).
- [x] AI-diagnosis rendering + **one-click "propose fix"**: the incidents view can run RCA and, when the diagnosis is actionable, create a policy-checked proposal in one click (`POST /incidents/{id}/propose` - pre-fills action+target from the diagnosis but still goes through CreateProposal, so policy/approval are never bypassed; non-actionable → 422). Closes the loop diagnose → propose → approve → execute from the UI. HTTP-tested (actionable → 201 proposed; ImagePullBackOff → 422).
- [x] **Global Overview** (§14) + self-stats (§17): `GET /api/v1/stats` aggregates tenant-scoped counts (clusters total/connected, incidents by status/severity/type, proposals by status, policy mode/paused) - all measured. Dashboard "SRE overview" view renders it as cards. HTTP-tested.
- [ ] Slice D (planned): live execution status streaming, before/after diff, dry-run output panel.

## Operational modes (spec §20) - rollout
Observe (M1) → Diagnose (M2) → Recommend (M2/3) → Approval-Required (M3) → Guarded Autonomy (M4) → Emergency Pause (M4).

## Non-goals for M1 (explicitly deferred)
No detection, no incidents, no AI, no remediation, no mutation. M1 is observe-only plumbing. This is called out so the implementation report never overstates what exists.
