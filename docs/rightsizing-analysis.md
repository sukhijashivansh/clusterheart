# Rightsizing analysis - how the numbers are computed

This document explains, end to end, how Clusterheart's **Rightsizing** tab
produces its CPU/memory recommendations: what it samples, how it stores the
data, how it aggregates it, and the exact rules that turn those statistics into
a verdict (OK / UPSIZE / DOWNSIZE) and a suggested request.

All of the logic lives in `app/history.go`. The frontend (`app/static/app.js`,
the `rightsizing` view) only renders what this code returns.

---

## 1. Why history, not a snapshot

A single `kubectl top` reading is a point in time. A workload that is quiet now
but spikes every few minutes would look over-provisioned in that snapshot, and
downsizing it would cause throttling or OOM kills under load.

So Clusterheart records usage over a rolling window and bases its verdict on the
**p95** and the **peak** across that window, not the instantaneous value. That
is the core idea: size to sustained load (p95) with enough headroom for the real
peak.

---

## 2. What gets sampled (`sampleOnce`)

A background sampler runs every `SAMPLE_INTERVAL` (default **30s**). Each run:

1. Lists all pods and pulls per-pod usage from metrics-server
   (CPU in milli-cores, memory in bytes), summed across the pod's containers.
2. Keeps only **active** pods (phase `Running` or `Pending`).
3. Groups pods by their owning **workload**. The owner is resolved one hop up
   the `ownerReferences` chain; a `ReplicaSet` name has its pod-template hash
   stripped so pods collapse onto their **Deployment** (see `goWorkloadName`).
4. For each workload it records one `usageSample`:

   | Field        | Meaning                                                        |
   |--------------|----------------------------------------------------------------|
   | `ts`         | sample timestamp (UTC)                                          |
   | `ns`         | namespace                                                      |
   | `workload`   | workload name (Deployment/StatefulSet/DaemonSet/...)           |
   | `ownerKind`  | display kind                                                   |
   | `replicas`   | count of active pods in this workload at sample time          |
   | `cpuPerPod`  | **total CPU usage / replicas** (milli-cores)                  |
   | `memPerPod`  | **total memory usage / replicas** (bytes)                     |
   | `cpuReq`     | per-pod CPU request, from a representative pod (milli-cores)   |
   | `memReq`     | per-pod memory request, from a representative pod (bytes)     |

### Per-pod normalization

Usage and requests are stored **per pod** (`cpuPerPod = totalUsage / replicas`).
This is deliberate: if an HPA scales a Deployment from 2 to 10 replicas, total
usage jumps 5x but per-pod usage stays comparable. Normalizing means a replica
count change does not fool the recommendation - the request is a per-pod value,
so we reason about per-pod usage.

---

## 3. Storage and retention

- Database `clusterheart`, collection `usage_samples` (MongoDB).
  Override the connection with `MONGO_URI` (defaults to `mongodb://localhost:27017`).
- A **TTL index** on `ts` prunes samples older than `historyRetention` (**8h**),
  keeping the store a bit larger than the analysis window.
- A compound index `{ns, workload, ts}` supports the windowed aggregation.

---

## 4. Aggregation (`handleRightsizing`)

The endpoint aggregates the **last `minWindow`** of samples
(`RIGHTSIZE_MIN_WINDOW`, default **6h**). `?ns=<namespace>` scopes it; `ALL` or
empty means every namespace.

MongoDB aggregation pipeline, grouped by `(ns, workload)`:

| Output     | Expression                                              |
|------------|---------------------------------------------------------|
| `cpuP95`   | `$percentile(cpuPerPod, p=0.95, method="approximate")`  |
| `memP95`   | `$percentile(memPerPod, p=0.95, method="approximate")`  |
| `cpuPeak`  | `$max(cpuPerPod)`                                        |
| `memPeak`  | `$max(memPerPod)`                                        |
| `cpuReq`   | `$last(cpuReq)` (most recent request)                   |
| `memReq`   | `$last(memReq)`                                          |
| `replicas` | `$last(replicas)`                                       |
| `samples`  | `$sum(1)` (number of samples in the window)             |
| `first`/`last` | `$min(ts)` / `$max(ts)`                             |

- **p95** uses MongoDB's `$percentile` with the `approximate` (t-digest) method.
- **coverage** = `last - first`: how much of the window actually has data. A
  workload created 10 minutes ago has only 10 minutes of coverage even if the
  window is 6h.

---

## 5. The verdict (`rightsizeVerdict`)

Inputs per workload: `cpuP95`, `memP95`, `cpuPeak`, `memPeak`, `cpuReq`,
`memReq`, `samples`, and `coverage`.

Define `pct(used, req) = used / req * 100`, or `-1` when there is no request.

### Step 1 - gates (no actionable verdict)

| Condition                                                   | Verdict    |
|-------------------------------------------------------------|------------|
| `samples < 3` **or** `coverage < 90% of minWindow`          | `LEARNING` |
| no CPU request **and** no memory request                    | `NO REQ`   |

`LEARNING` means "still collecting" - the tool waits for most of a full window
before it will advise, so early/partial data never drives a change.

### Step 2 - under-provisioned (upsize) - checked first

| Condition                                                   | Verdict      |
|-------------------------------------------------------------|--------------|
| `cpuP95% > 90` **or** `cpuPeak% >= 100`                     | `UPSIZE CPU` |
| `memP95% > 90` **or** `memPeak% >= 100`                     | `UPSIZE MEM` |

Rationale: if sustained CPU (p95) is within 10% of the request, or the peak ever
reached/exceeded the request, the workload is at risk of CPU throttling or
(for memory) OOM kills. Upsize wins over downsize, and no suggested size is
given (upsizing is a judgment call the operator should make deliberately).

### Step 3 - platform-managed namespaces

Workloads in `kube-*` namespaces return `OK` and are never flagged for
downsize - they are platform-managed (EKS/control-plane) and cannot be patched
from the UI, so a recommendation there would be noise.

### Step 4 - over-provisioned (downsize)

A resource is a downsize candidate when even its p95 sits **well below** the
request:

- `cpuDown = cpuP95% < 40`
- `memDown = memP95% < 40`

| Candidates        | Verdict         |
|-------------------|-----------------|
| CPU and memory    | `DOWNSIZE`      |
| CPU only          | `DOWNSIZE CPU`  |
| memory only       | `DOWNSIZE MEM`  |
| neither           | `OK`            |

### Suggested new request (`headroom`)

For a downsize, the suggested per-pod request is sized to the **peak plus 20%
headroom**, and never above the current request:

```
suggest = peak + peak/5          // peak + 20%
if suggest <= 0 or suggest >= currentRequest:
    suggest = 0                  // no suggestion (not actually smaller)
```

So the recommendation is: keep enough room above the real observed peak, but
reclaim the slack between that and today's (much larger) request. `suggestCpu`
is in milli-cores, `suggestMem` in bytes; `0` means "no change suggested."

---

## 6. Worked example

A Deployment with a **500m** CPU request, observed over a full 6h window:

- `cpuP95 = 150m` -> `cpuP95% = 30%` (< 40, downsize candidate)
- `cpuPeak = 220m` -> `cpuPeak% = 44%` (< 100, no throttle risk)
- Suggestion: `220m + 20% = 264m`, which is `< 500m`, so **suggest 264m**.
- Verdict: `DOWNSIZE CPU`, suggested CPU request `264m`.

Reclaims 236m per pod while still leaving ~20% above the real peak.

---

## 7. Configuration

| Env var                | Default   | Effect                                             |
|------------------------|-----------|----------------------------------------------------|
| `MONGO_URI`            | localhost | where samples are stored (set to Atlas, etc.)      |
| `SAMPLE_INTERVAL`      | `30s`     | how often usage is recorded                        |
| `RIGHTSIZE_MIN_WINDOW` | `6h`      | history required before advising (lower to demo)   |

The window directly sets the `LEARNING` gate: with a 6h window, a workload needs
roughly 5.4h of coverage and at least 3 samples before it gets a real verdict.

---

## 8. Design notes and limitations

- **p95 over peak for the downsize gate**: using p95 (not max) to decide *whether*
  to downsize means a single spike does not block reclaiming slack, while the
  **peak** still sets the *floor* of the suggestion - so the suggested size is
  never below the real peak.
- **Per-pod reasoning** makes recommendations robust to HPA/replica changes.
- **Requests, not limits**: the analysis is against CPU/memory *requests* (what
  scheduling and bin-packing use). Limits are not rightsized here.
- **Needs metrics-server**: if pod metrics are unavailable, usage reads as zero
  and workloads will not be flagged for downsize (safe default).
- **Approximate percentile**: `$percentile` uses t-digest; for typical sample
  counts (hundreds over a window) this is accurate to within a sample or two.
- **8h retention vs 6h window**: retention is intentionally larger than the
  window so the full window is always available after the TTL prune.
