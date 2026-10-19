# End-to-end validation on a live `kind` cluster

This runbook reproduces the full observe → detect → propose → approve → execute →
verify → audit loop against a **real** Kubernetes API, with a **Mongo-backed**
control plane. It is the evidence behind the "tested against a live cluster"
claims in `implementation-plan.md`. Last run: 2026-10-11, kind v0.24.0 /
Kubernetes v1.31.0, mongo:7.

## What it proves

- The agent collector reads a real kube API (reports `k8sVersion`, telemetry).
- Deterministic detection fires on real workloads (`CrashLoopBackOff`,
  `RolloutStuck`) with evidence, dedup (occurrence counting), and
  resolve/reopen lifecycle.
- The policy engine gates on the namespace allowlist and approval-required mode.
- The in-cluster controller pulls only approved actions and performs a **real**
  `RolloutRestart` (sets the `kubectl.kubernetes.io/restartedAt` annotation),
  with before-state capture and resourceVersion optimistic concurrency.
- Post-action verification works **both** ways: a healthy Deployment verifies
  and is marked `executed`; a never-healthy Deployment applies but fails
  verification within the timeout and is marked `failed`.
- Proposals, policy, and audit persist across a control-plane restart (Mongo).

## Prerequisites

`docker`, `kubectl`, `go`, and `kind` (`go install sigs.k8s.io/kind@v0.24.0`).

## Steps

```bash
# 0) Infra: a kind cluster + a throwaway MongoDB.
kind create cluster --name ch-sre --wait 90s
export KUBECONFIG="$(mktemp)"; kind get kubeconfig --name ch-sre > "$KUBECONFIG"
docker run -d --name ch-mongo-test -p 27019:27017 mongo:7

# 1) Test workloads: one healthy, one crashlooping.
kubectl create namespace web
kubectl -n web create deployment healthy-api --image=registry.k8s.io/pause:3.9 --replicas=2
kubectl -n web create deployment crasher --image=busybox --replicas=1 -- /bin/sh -c 'exit 1'

# 2) Control plane (durable, Mongo-backed).
cd sre
CONTROLPLANE_ADMIN_TOKEN=dev-admin ADDR=127.0.0.1:9100 \
  MONGO_URI=mongodb://127.0.0.1:27019 MONGO_DB=ch_e2e go run ./cmd/controlplane &

# 3) Register the cluster; capture clusterId + agentToken.
curl -s -X POST localhost:9100/api/v1/clusters -H "Authorization: Bearer dev-admin" \
  -H "Content-Type: application/json" \
  -d '{"tenantId":"acme","clusterName":"kind-ch-sre","environment":"dev"}'

# 4) Agent against kind (observe-only). Reports k8sVersion + telemetry.
CONTROLPLANE_URL=http://localhost:9100 CLUSTER_ID=<clu_...> AGENT_TOKEN=<...> \
  KUBECONFIG=$KUBECONFIG COLLECT_INTERVAL=10s go run ./cmd/agent &

# 5) After ~30s the crasher hits CrashLoopBackOff; incidents appear:
curl -s "localhost:9100/api/v1/incidents?tenantId=acme" -H "Authorization: Bearer dev-admin"

# 6) Allow the web namespace (mode stays approval-required).
curl -s -X PUT "localhost:9100/api/v1/policy?tenantId=acme" -H "Authorization: Bearer dev-admin" \
  -H "Content-Type: application/json" \
  -d '{"mode":"approval-required","allowedActions":["RolloutRestart"],"namespaceAllowlist":["web"],"protectedEnvironments":["production","prod"],"maxConcurrent":5,"perWorkloadCooldownSeconds":0,"paused":false}'

# 7) Opt the cluster into remediation; capture the one-time remediationToken.
curl -s -X POST "localhost:9100/api/v1/clusters/<clu_...>/remediation-token" \
  -H "Authorization: Bearer dev-admin"

# 8) Propose RolloutRestart on each Deployment (reference an open incident id),
#    then approve each. Status goes proposed -> approved.
curl -s -X POST "localhost:9100/api/v1/incidents/<inc_...>/proposals?tenantId=acme" \
  -H "Authorization: Bearer dev-admin" -H "Content-Type: application/json" \
  -d '{"action":"RolloutRestart","target":{"kind":"Deployment","namespace":"web","name":"healthy-api"},"rationale":"e2e"}'
curl -s -X POST "localhost:9100/api/v1/proposals/<rem_...>/approve?tenantId=acme" \
  -H "Authorization: Bearer dev-admin"

# 9) Remediation controller against kind (separate credential). Pulls approved
#    actions, executes, verifies, reports.
CONTROLPLANE_URL=http://localhost:9100 CLUSTER_ID=<clu_...> REMEDIATION_TOKEN=<...> \
  KUBECONFIG=$KUBECONFIG POLL_INTERVAL=5s go run ./cmd/remediation &

# 10) Observe terminal states + the live-cluster mutation.
curl -s "localhost:9100/api/v1/proposals?tenantId=acme" -H "Authorization: Bearer dev-admin"
kubectl -n web get deploy healthy-api -o jsonpath='{.spec.template.metadata.annotations.kubectl\.kubernetes\.io/restartedAt}'
```

## Observed result (2026-10-11)

Incidents (real detection):

```
CrashLoopBackOff  Deployment/crasher in web   severity=high  (open; a prior one auto-resolved then reopened)
RolloutStuck      Deployment/crasher in web   severity=high  occurrences=9
```

Proposals (real execution):

```
healthy-api  executed  dryRun=false  verificationPassed=true   beforeState="resourceVersion=947, replicas=2, restartedAt=\"\""   result="applied: rolling restart triggered"
crasher      failed    dryRun=false  verificationPassed=false  beforeState="resourceVersion=1760, replicas=1, restartedAt=\"\""  error="rollout did not become healthy within timeout"
```

Live cluster (proof of real mutation - both annotations set, even for the one
that failed verification):

```
healthy-api restartedAt: 2026-10-11T18:23:08Z
crasher     restartedAt: 2026-10-11T18:21:07Z
```

Controller log (crasher verified for the full ~120s timeout before failing,
then healthy-api verified in ~2s):

```
23:51:07  executing action  crasher      dryRun=false
23:53:08  reported result    crasher      success=false
23:53:08  executing action   healthy-api  dryRun=false
23:53:10  reported result    healthy-api  success=true
```

Audit trail (tenant-scoped, Mongo-persisted):

```
policy.updated · proposal.created ×2 · proposal.approved ×2 · remediation.failed · remediation.executed
```

Durability: after `kill`-ing the control plane and restarting it against the
same Mongo, both proposals survived with their terminal states and execution
results, and the policy (web allowlist) persisted.

## Teardown

```bash
kill %1 %2 %3 2>/dev/null                 # control plane, agent, controller
kind delete cluster --name ch-sre
docker rm -f ch-mongo-test
```
