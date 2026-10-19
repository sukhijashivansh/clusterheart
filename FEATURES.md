# Clusterheart - Features & Competitive Comparison

Clusterheart is a **desktop Kubernetes dashboard** that runs entirely on your
machine. It reads your **local kubeconfig / SSO**, does all cluster work
locally, and never sends your cluster credentials anywhere. A small hosted
account server handles only login and approve/revoke - it never touches your
clusters.

Three parts:

- **`app/`** - a Go binary that reads your local kubeconfig and serves the web UI.
- **`electron/`** - the desktop shell + login gate that spawns the dashboard.
- **`server/`** - an account server (auth + admin approval, MongoDB-backed).

---

## Features

### Workloads & inspection
- **Pods view** - live table of every pod: namespace, ready count, status,
  restarts, **live CPU/mem** (from metrics-server), age, node.
  - Multi-namespace filter, free-text search (pod / namespace / node),
    sort by CPU / memory / restarts, **"issues only"** filter, cronjob toggle,
    live pod count.
- **Per-pod actions** (keyboard shortcuts in the UI):
  - **`l` logs** - tail container logs (per-container, configurable tail).
  - **`y` yaml** - full pod YAML.
  - **`d` diagnose** - one-click triage: warning **events**, an automatic
    **analysis** (high restarts → crash loop, Pending → capacity/PVC,
    Running-but-not-Ready → probe failing), plus the last log lines.
  - **`s` shell** - interactive exec into a container.
  - **`f` port-forward** - start/stop `kubectl port-forward`, managed in-app.
  - **`x` delete** - delete a pod.
  - **`g` AI** - hand the pod's context to the AI assistant.
- **Jobs view** - jobs (including completed/lingering) with status detail.
- **Nodes view** - node inventory and health.
- **HPA data** - horizontal pod autoscaler readings rendered like
  `kubectl get hpa` (e.g. `cpu: 31%/80%`).
- **Namespace ops** - bulk "cleanse" a namespace (delete all pods).
- **Multi-cluster** - list kube contexts and **switch context** from the UI
  (matching `kubectl config use-context` semantics).

### Rightsizing (history-based, not a snapshot)
- Samples per-workload CPU/memory **every 30s into MongoDB**.
- After a full analysis window (**6h default**, configurable), recommends a new
  request sized to **p95 + peak** usage - so spiky workloads aren't wrongly told
  to shrink.
- Verdicts: **LEARNING / NO REQ / OK / UPSIZE CPU / UPSIZE MEM / DOWNSIZE**,
  with a concrete suggested CPU/mem request.
- **Apply the recommendation** back to the owning workload from the UI (patches
  the top-level owner, not the pod).
- Degrades gracefully - if MongoDB is unreachable the rest of the app still runs.

### FinOps / cost (AWS)
- **AWS daily spend** panel via `aws ce get-cost-and-usage`.
- **Cost by service** breakdown.
- Authenticates using the **same AWS profile your current kube context uses**
  (read from the context's `exec` auth block) - if your cluster loads, costs
  usually load too.

### Traffic / performance testing
- **Load test** - JMeter-backed load generator against a pod: configurable
  threads, ramp-up, duration, method, path, scheme, body/content-type. Knobs are
  clamped so a stray value can't hammer a target. Runs over an in-app
  `kubectl port-forward`.
- **Latency probe / Measure** - times HTTP requests to a pod endpoint,
  auto-discovering endpoints, reporting **p50 / p90 / p95 / p99**, peak RPS, and
  peak average latency.

### AI assistant (built-in SRE copilot)
- Chat tab with a Kubernetes-aware system prompt (workloads, observability,
  metrics, FinOps, Helm, runtimes, DevOps/SRE).
- **Read-only tool use**: the model can run **allowlisted, read-only kubectl**
  (`get, describe, logs, top, events, explain, version, cluster-info,
  api-resources, api-versions`, and `auth can-i` only) to pull fresh detail when
  the snapshot isn't enough. Every mutating verb (apply/delete/patch/scale/
  exec/port-forward/…) is **refused outright**; args are exec'd directly so shell
  metacharacters never reach a shell, and output is capped so a chatty `logs`
  can't blow the context window.
- **Bring-your-own-key, client-side**: pick **Gemini** or **OpenRouter**, paste
  a key stored only in the browser's `localStorage`. OpenRouter defaults to a
  chain of **free** models (no credits needed); configurable max tokens.

### Security & access model
- **Credentials never leave the machine** - all Kubernetes/AWS work happens in
  the local Go binary. The hosted server only sees accounts.
- **Approve/revoke gating** - users sign up, an admin approves them; the desktop
  app only opens the dashboard for approved accounts (pending screen otherwise).
- **bcrypt-hashed passwords**, signed session tokens (cookie for web,
  30-day bearer token for the desktop app), auto-generated session secret.
- Cross-platform installers (macOS Apple Silicon + Intel, Windows, Linux
  AppImage), delivered from the private server - nothing published publicly.

---

## Comparison with related tools

| Capability | **Clusterheart** | k9s | Lens / OpenLens | Headlamp | Kubernetes Dashboard | Grafana + Prom / Kubecost |
|---|---|---|---|---|---|---|
| Interface | Desktop GUI (Electron + web) | Terminal (TUI) | Desktop GUI | Web / desktop GUI | Web GUI | Web GUI |
| Runs locally, creds stay local | ✅ | ✅ | ✅ (⚠️ some cloud add-ons) | ✅ | ⚠️ in-cluster | ⚠️ in-cluster / SaaS |
| Live pods / logs / exec / port-forward | ✅ | ✅ | ✅ | ✅ | partial | ❌ |
| One-click pod **diagnose** (events+analysis+logs) | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **History-based rightsizing** (p95+peak, apply) | ✅ | ❌ | ❌ (plugin) | ❌ | ❌ | ⚠️ (Kubecost, separate) |
| **AWS cost / FinOps** built in | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ (Kubecost) |
| **Load test + latency percentiles** built in | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **AI copilot with read-only kubectl** | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| Multi-cluster context switch | ✅ | ✅ | ✅ | ✅ | ⚠️ | n/a |
| Central **approve/revoke** access control | ✅ | ❌ | ❌ | ❌ | ⚠️ (RBAC) | ⚠️ |
| Needs anything installed in the cluster | ❌ (metrics-server only) | ❌ | ❌ | optional | ✅ | ✅ |
| Setup effort | one desktop install | one binary | one install | install/deploy | deploy manifests | deploy stack |

### Where each competitor sits
- **k9s** - fast, beloved TUI for navigating resources. Read/operate only; no
  cost, no rightsizing history, no load testing, no AI, no team gating.
- **Lens / OpenLens** - polished desktop GUI. Broad resource coverage, but
  advanced insight (cost, rightsizing) lives in paid/enterprise or plugins, and
  there's no built-in load test or AI copilot.
- **Headlamp** - clean, extensible web/desktop UI. Great base viewer; the extras
  Clusterheart bundles (cost, rightsizing, load test, AI) are not built in.
- **Kubernetes Dashboard** - the official in-cluster web UI. Requires deploying
  into the cluster and wiring RBAC/tokens; viewer-focused, no cost/rightsizing/AI.
- **Grafana + Prometheus / Kubecost** - the powerful "assemble it yourself"
  observability + FinOps stack. Deep, but it's a deployment project (in-cluster
  components, dashboards, retention) - not a grab-and-go desktop tool.

---

## Why choose Clusterheart

1. **Everything in one desktop app.** Inspect + diagnose + rightsize + cost +
   load-test + AI - instead of stitching k9s + Kubecost + JMeter + a Grafana
   stack + an LLM chat window together.
2. **Zero cluster footprint.** No agents or manifests to deploy (beyond the
   standard metrics-server). Install the desktop app and point it at your
   existing kubeconfig.
3. **Credentials never leave your machine.** All cluster and AWS calls run in a
   local binary; the hosted server only knows *who is allowed in*, never your
   clusters.
4. **Insight, not just a viewer.** History-based rightsizing (p95+peak, applied
   with a click), one-click pod triage, and real latency percentiles turn raw
   resource lists into decisions.
5. **Cost where you work.** AWS daily spend and per-service breakdown next to the
   workloads driving them - reusing the same AWS profile your kube context
   already uses.
6. **A safe AI copilot.** It can *read* your live cluster (allowlisted read-only
   kubectl) to answer real questions, but is structurally prevented from
   changing anything. Bring your own key; free models work out of the box.
7. **Built-in team access control.** Central approve/revoke gating for who can
   use the tool - with no shared cluster credentials to hand around.
