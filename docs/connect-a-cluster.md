# Connect a cluster to Clusterheart

Install the Clusterheart agent into a Kubernetes cluster so it shows up — live —
in your browser dashboard at **[clusterheart.com/app](https://clusterheart.com/app)**,
from any device.

The agent is **read-only**. It runs inside your cluster with a scoped
ServiceAccount, reads nodes / pods / workloads / events (and CPU/memory from
metrics-server, if present), and streams bounded health snapshots to the
control plane. It never mutates anything, never reads Secret values, and **no
kubeconfig ever leaves your cluster**.

---

## Prerequisites

- `kubectl` access to the target cluster, with permission to create a
  Namespace, ServiceAccount, ClusterRole(+Binding), and Deployment
  (cluster-admin or equivalent).
- The cluster has **outbound internet** so the agent can reach:
  - `https://clusterheart-server.clusterheart.com` (the control plane), and
  - the Vercel Blob that hosts the agent binary.
- *(Optional, for live CPU/memory)* **metrics-server** installed. Without it the
  agent still works; CPU/Mem just show as `-`. Check with `kubectl top pods -A`.

---

## 1. Get your cluster credentials

1. Log in at **[clusterheart.com/app](https://clusterheart.com/app)**.
2. Click **Add cluster**, give it a name (e.g. `prod-us-east`).
3. It shows `CLUSTER_ID`, `AGENT_TOKEN`, and the install command.
   > ⚠️ **Copy the token now — it is shown only once.** We store only its hash.

## 2. Point kubectl at the cluster you want to watch

```bash
kubectl config use-context <your-context>   # or pass --context to each command
kubectl config current-context              # confirm it's the right cluster
```

## 3. Create the namespace

```bash
kubectl create namespace clusterheart-sre
```

## 4. Create the agent secret

Paste your real `CLUSTER_ID` and `AGENT_TOKEN` from step 1:

```bash
kubectl -n clusterheart-sre create secret generic clusterheart-agent \
  --from-literal=CONTROLPLANE_URL=https://clusterheart-server.clusterheart.com/cp \
  --from-literal=CLUSTER_ID=clu_xxxxxxxxxxxxxxxx \
  --from-literal=AGENT_TOKEN=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
```

## 5. Deploy the agent

```bash
kubectl apply -f https://clusterheart.com/agent.yaml
```

This creates the ServiceAccount, read-only ClusterRole(+Binding), and the agent
Deployment. A tiny alpine pod fetches the prebuilt agent binary and runs it
(no container registry needed).

## 6. Verify

```bash
kubectl -n clusterheart-sre get pods
# NAME                                  READY   STATUS    RESTARTS   AGE
# clusterheart-agent-xxxxxxxxxx-xxxxx   1/1     Running   0          40s

kubectl -n clusterheart-sre logs -l app=clusterheart-agent --tail=20
# ... "agent starting" ... clusterId=clu_... interval=30s
```

Within ~30 seconds, refresh **[clusterheart.com/app](https://clusterheart.com/app)**
— the cluster flips to **Connected** with node/pod counts. Click it to see the
pod/job tables with live CPU/memory.

---

## What the agent can read (RBAC)

Read-only (`get`, `list`, `watch`) on:

- core: `nodes`, `pods`, `events`
- `apps`: `deployments`, `statefulsets`, `daemonsets`
- `batch`: `jobs`, `cronjobs`
- `metrics.k8s.io`: `pods` (CPU/memory, read-only)
- nonResourceURLs: `/version`

No `create`/`update`/`delete`, no `exec`, no `portforward`, no Secret **values**.

## Uninstall (clean, anytime)

```bash
kubectl delete ns clusterheart-sre
kubectl delete clusterrole clusterheart-agent-observe
kubectl delete clusterrolebinding clusterheart-agent-observe
```

## Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| Cluster stays **Offline** | Agent pod not Running, or no internet egress to `clusterheart-server.clusterheart.com`. Check `kubectl -n clusterheart-sre get pods` and the pod logs. |
| Pod `CrashLoopBackOff` | Check logs. A bad/typo'd `AGENT_TOKEN` or `CLUSTER_ID` in the secret — recreate the secret and restart: `kubectl -n clusterheart-sre rollout restart deploy/clusterheart-agent`. |
| CPU/Mem show `-` | metrics-server isn't installed. Install it (`kubectl top pods -A` should work), then wait a cycle. |
| Lost the token | Tokens are shown once and stored hashed. Add a fresh cluster in `/app` to get a new token, and re-run the install. |

> **Note:** the manifest reads per-cluster values (`CLUSTER_ID`, `AGENT_TOKEN`,
> `CONTROLPLANE_URL`) from the Secret you created in step 4, so the same hosted
> `agent.yaml` works for every cluster — only the Secret differs.
