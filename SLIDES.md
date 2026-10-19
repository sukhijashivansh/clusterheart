# Clusterheart - Slide Deck

> A desktop Kubernetes dashboard. Reads your **local** kubeconfig, does all
> cluster work on your machine - credentials never leave it.

---

## Slide 1 - Title

# Clusterheart
### See your cluster. Fix it. Right-size it. Ask AI.

- One application: pods, jobs, nodes, cost, AI, and resource patching
- Zero cluster footprint - runs against your existing kubeconfig
- Your credentials never leave your machine

---

## Slide 2 - Pods

# Pods, live

- **Live health at a glance** - every pod with ready count, status, restarts,
  and **real-time CPU/memory** from metrics-server
- **Find problems fast** - multi-namespace filter, search, sort by CPU/mem/
  restarts, and an **"issues only"** toggle
- **Act in one click** - logs, YAML, shell, port-forward, delete, and one-click
  **diagnose** (events + auto-analysis + recent logs)

---

## Slide 3 - Jobs

# Jobs

- **Full picture** - every job including completed/lingering ones, with status
  and completion detail
- **Spot failures** - see which jobs errored or never finished, right next to
  their pods
- **Drill down instantly** - jump from a job to its pod's logs, YAML, or
  diagnosis without leaving the app

---

## Slide 4 - Nodes

# Nodes

- **Capacity vs. usage** - allocatable CPU, memory, and ephemeral storage
  against **live usage** per node
- **Health & role** - Ready status and node role (control-plane / worker) at a
  glance
- **Packing view** - pods-per-node vs. the pod limit, so you see crowding and
  headroom

---

## Slide 5 - FinOps

# FinOps - AWS cost

- **Daily spend, in-app** - pulls AWS Cost Explorer right beside the workloads
  driving it
- **Cost by service** - see exactly which AWS services are running up the bill
- **Zero extra setup** - reuses the **same AWS profile your kube context already
  uses**; if the cluster loads, cost usually does too

---

## Slide 6 - Ask the AI

# Ask questions from AI

- **Cluster-aware copilot** - chat that understands your workloads, metrics,
  events, and cost context
- **Reads your live cluster** - runs **allowlisted, read-only kubectl**
  (get / describe / logs / top / events…) to answer with real data
- **Safe & flexible** - every mutating command is refused; bring your own key
  (Gemini or OpenRouter, free models work out of the box)

---

## Slide 7 - Resource allocation & patching

# Rightsizing - recommend & apply

- **History, not a guess** - samples CPU/mem every 30s and analyzes a full
  window (6h default), sizing to **p95 + peak** so spiky apps aren't shrunk
- **Clear verdicts** - OK / UPSIZE CPU / UPSIZE MEM / DOWNSIZE, each with a
  concrete suggested request
- **One-click apply** - patch the new resource request straight to the owning
  workload, right from the UI

---

## Slide 8 - Why Clusterheart

# Why Clusterheart

- **All-in-one** - inspect, diagnose, cost, AI, and right-size without stitching
  five tools together
- **Nothing to deploy** - a desktop install pointed at your kubeconfig; no
  in-cluster agents
- **Private by design** - cluster and AWS calls stay local; the server only
  gates *who's allowed in*
