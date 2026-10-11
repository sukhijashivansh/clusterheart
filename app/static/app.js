// kubepulse-ui frontend. Single-file vanilla JS — no framework on purpose.

const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => Array.from(r.querySelectorAll(s));

const state = {
  view: "pods",
  pods: [],
  filtered: [],
  jobs: [],
  jobNsSel: null,       // array of namespaces, or null = all
  jobSearch: "",
  jobFailedOnly: false,
  jobAllRuns: false,    // false = latest run per job; true = full history
  nodes: [],
  stats: null,
  namespaces: ["ALL"],
  nsSelected: null,     // array of namespaces, or null = all
  podNsMs: null,        // pods namespace multi-select controller
  jobNsMs: null,        // jobs namespace multi-select controller
  rs: null,           // /api/rightsizing payload {ready, windowMins, rows}
  rsNs: "ALL",
  rsSearch: "",
  rsIssuesOnly: false,
  rsLoading: false,
  hpas: [],
  hpaIndex: null,
  hpaNs: "ALL",
  hpaSearch: "",
  hpaLoading: false,
  search: "",
  sort: "default",
  issuesOnly: false,
  hideCron: false,
  resSort: "name",
  resIssuesOnly: false,
  resSearch: "",
  candNs: "ALL",
  selected: -1,
  costs: null,
  costServices: null,
  costsLoading: false,
  headroom: 20,
  gemini: { history: [], busy: false, pod: null, provider: "openrouter" },
  pfs: [],
  shell: null,
  contexts: [],
  currentCtx: "",
  ctxSwitching: false,
  consolidationCollapsed: false,
  capReady: false,    // capacity analysis revealed after its cool-down settles
  capTimer: null,
};

// Pods in Succeeded/Failed don't hold cluster capacity — the scheduler frees
// their requests as soon as the containers exit. They linger in the API only
// until the owning controller's TTL garbage-collects them. Excluding them
// from right-sizing math avoids false "reclaimable" credit for completed Jobs.
function isActive(p) { return p.status === "Running" || p.status === "Pending"; }

// ---------- util ----------
function fmtBytes(b) {
  if (!b || b <= 0) return "0";
  const u = ["B", "Ki", "Mi", "Gi", "Ti", "Pi"];
  let i = 0, v = b;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return v >= 100 ? `${v.toFixed(0)}${u[i]}` : `${v.toFixed(1)}${u[i]}`;
}
function fmtCpu(m) {
  if (!m) return "0m";
  if (m >= 1000) return `${(m / 1000).toFixed(2)}c`;
  return `${m}m`;
}
function pct(n, d) {
  if (!d || d <= 0) return null;
  return (n / d) * 100;
}
// fmtDuration turns a span of seconds into a compact label (e.g. 90 -> "1.5m",
// 3600 -> "1h"). Returns "—" when unset so an empty deadline reads cleanly.
function fmtDuration(s) {
  if (!s || s <= 0) return "—";
  const trim = v => (Number.isInteger(v) ? String(v) : v.toFixed(1));
  if (s < 60) return s + "s";
  if (s < 3600) return trim(s / 60) + "m";
  if (s < 86400) return trim(s / 3600) + "h";
  return trim(s / 86400) + "d";
}
// Kubernetes-reserved system namespaces (kube-system, kube-node-lease,
// kube-public, …). Their workloads are managed by the platform/add-ons and a
// live-object patch either gets reverted or risks cluster-wide breakage, so we
// don't offer to patch them from the UI.
function isSystemNamespace(ns) {
  return /^kube-/.test(ns); // kube-system, kube-node-lease, kube-public, …
}

// isInfraNamespace is a broader check than isSystemNamespace: it also excludes
// common platform / observability add-on namespaces (monitoring, ingress,
// service mesh, autoscaling, cert-manager, etc.) so views like the latency
// probe can show *application* pods only. Kept separate from isSystemNamespace
// so it doesn't affect the resources "managed" tag.
const INFRA_NAMESPACES = new Set([
  "monitoring", "observability", "logging", "prometheus", "grafana",
  "cert-manager", "ingress-nginx", "istio-system", "linkerd", "karpenter",
  "amazon-cloudwatch", "aws-observability", "external-dns", "metrics-server",
  "kubecost", "velero", "argocd", "argo", "flux-system", "gatekeeper-system",
  "kyverno", "calico-system", "calico-apiserver", "tigera-operator",
  "gpu-operator", "opentelemetry", "cattle-system", "longhorn-system", "openebs",
]);
function isInfraNamespace(ns) {
  return isSystemNamespace(ns) || INFRA_NAMESPACES.has(ns);
}

function statusClass(p) {
  if (p.status === "Succeeded") return "status-done";
  if (p.status === "Pending") return "status-pending";
  if (p.status === "Failed" || /CrashLoopBackOff|Error|Unschedulable|FailedScheduling|FailedAttachVolume|FailedMount|ImagePull|InvalidImageName/.test(p.message)) return "status-err";
  if (p.isReady && p.status === "Running") return "status-ok";
  if (!p.isReady && p.status === "Running") return "status-warn";
  return "status-warn";
}
function setMsg(t) { $("#status-msg").textContent = t; }

async function jget(url) {
  const r = await fetch(url);
  const ct = r.headers.get("content-type") || "";
  if (!r.ok) {
    if (ct.includes("json")) throw new Error((await r.json()).error || `${r.status}`);
    throw new Error(`${r.status} ${await r.text()}`);
  }
  if (ct.includes("json")) return r.json();
  return r.text();
}

// ---------- data refresh ----------
async function refresh() {
  $("#refresh-meta").textContent = "refresh: loading";
  try {
    const [pods, stats, nodes, namespaces, jobs, hpas] = await Promise.all([
      jget("/api/pods"),
      jget("/api/stats").catch(() => null),
      jget("/api/nodes").catch(() => []),
      jget("/api/namespaces").catch(() => []),
      jget("/api/jobs").catch(() => []),
      jget("/api/hpas").catch(() => []),
    ]);
    state.pods = pods || [];
    state.stats = stats;
    state.nodes = nodes || [];
    state.namespaces = ["ALL", ...(namespaces || [])];
    state.jobs = jobs || [];
    state.hpas = hpas || [];
    rebuildHpaIndex();
    renderNamespaceSelects();
    renderStats();
    renderPodTable();
    renderJobTable();
    renderNodes();
    renderRightsizing();
    if (state.view === "rightsizing") loadRightsizing();
    $("#refresh-meta").textContent = `refresh: ${new Date().toLocaleTimeString()}`;
  } catch (e) {
    $("#refresh-meta").textContent = "refresh: error";
    setMsg(`error: ${e.message}`);
  }
}

function renderStats() {
  const s = state.stats;
  $("#s-pods").textContent = state.pods.length;
  if (!s) {
    ["s-nodes", "s-cpu", "s-mem"].forEach(id => $("#" + id).textContent = "—");
    return;
  }
  $("#s-nodes").textContent = s.nodeCount;
  const cpuP = pct(s.totalCpuUsage, s.totalCpuCap);
  const memP = pct(s.totalMemUsage, s.totalMemCap);
  $("#s-cpu").innerHTML = `${fmtCpu(s.totalCpuUsage)}/${fmtCpu(s.totalCpuCap)} <span class="sep">·</span> <span class="${pctClass(cpuP)}">${cpuP !== null ? cpuP.toFixed(0) + "%" : "—"}</span>`;
  $("#s-mem").innerHTML = `${fmtBytes(s.totalMemUsage)}/${fmtBytes(s.totalMemCap)} <span class="sep">·</span> <span class="${pctClass(memP)}">${memP !== null ? memP.toFixed(0) + "%" : "—"}</span>`;
}

function pctClass(p) {
  if (p === null) return "";
  if (p > 80) return "pct-hot";
  if (p > 50) return "pct-warn";
  return "pct-ok";
}

function renderNamespaceSelects() {
  for (const id of ["#rs-ns-select"]) {
    const sel = $(id);
    if (!sel) continue;
    const cur = sel.value || "ALL";
    sel.innerHTML = "";
    for (const n of state.namespaces) {
      const o = document.createElement("option");
      o.value = n;
      o.textContent = n;
      sel.appendChild(o);
    }
    sel.value = state.namespaces.includes(cur) ? cur : "ALL";
  }
  // Feed the real namespaces (minus the "ALL" sentinel) to the multi-selects.
  const real = state.namespaces.filter(n => n !== "ALL");
  if (state.podNsMs) state.podNsMs.setOptions(real);
  if (state.jobNsMs) state.jobNsMs.setOptions(real);
}

// createMultiSelect turns an empty <div> into a checkbox dropdown for picking
// several namespaces at once. onChange gets an array of selected values, or
// null when "all" is active (nothing checked). Returns { setOptions, get }.
function createMultiSelect(root, placeholder, onChange) {
  root.className = "ms";
  root.innerHTML = `
    <button type="button" class="btn ms-btn">
      <span class="ms-label">${placeholder}</span><span class="ms-caret">▾</span>
    </button>
    <div class="ms-panel hidden">
      <input class="input ms-search" placeholder="filter namespaces" spellcheck="false" />
      <label class="ms-opt ms-all"><input type="checkbox" class="ms-all-cb" checked> all namespaces</label>
      <div class="ms-list"></div>
    </div>`;
  const btn = $(".ms-btn", root);
  const label = $(".ms-label", root);
  const panel = $(".ms-panel", root);
  const search = $(".ms-search", root);
  const allCb = $(".ms-all-cb", root);
  const list = $(".ms-list", root);
  const selected = new Set();
  let options = [];

  const value = () => (selected.size ? [...selected] : null);
  function sync() {
    label.textContent = selected.size === 0 ? placeholder
      : selected.size === 1 ? [...selected][0]
      : `${selected.size} namespaces`;
    allCb.checked = selected.size === 0;
  }
  function renderList() {
    const q = search.value.toLowerCase();
    list.innerHTML = "";
    for (const o of options) {
      if (q && !o.toLowerCase().includes(q)) continue;
      const lab = document.createElement("label");
      lab.className = "ms-opt";
      lab.innerHTML = `<input type="checkbox"> <span>${o}</span>`;
      const cb = lab.firstChild;
      cb.checked = selected.has(o);
      cb.addEventListener("change", () => {
        cb.checked ? selected.add(o) : selected.delete(o);
        sync();
        onChange(value());
      });
      list.appendChild(lab);
    }
  }
  btn.addEventListener("click", () => {
    const opening = panel.classList.contains("hidden");
    panel.classList.toggle("hidden");
    if (opening) { renderList(); search.focus(); }
  });
  search.addEventListener("input", renderList);
  allCb.addEventListener("change", () => {
    selected.clear();
    sync();
    renderList();
    onChange(value());
  });
  document.addEventListener("click", e => { if (!root.contains(e.target)) panel.classList.add("hidden"); });

  sync();
  return {
    setOptions(opts) {
      options = opts.slice();
      for (const s of [...selected]) if (!options.includes(s)) selected.delete(s);
      sync();
      if (!panel.classList.contains("hidden")) renderList();
    },
    clear() { selected.clear(); sync(); },
    get: value,
  };
}

// ---------- pod table ----------
function applyFilter() {
  let pods = state.pods.slice();
  if (state.nsSelected) pods = pods.filter(p => state.nsSelected.includes(p.namespace));
  if (state.issuesOnly) pods = pods.filter(p => !(p.isReady && p.status === "Running"));
  if (state.hideCron) pods = pods.filter(p => p.ownerKind !== "CronJob");
  if (state.search) {
    const q = state.search.toLowerCase();
    pods = pods.filter(p =>
      p.name.toLowerCase().includes(q) ||
      p.namespace.toLowerCase().includes(q) ||
      (p.nodeName || "").toLowerCase().includes(q) ||
      (p.message || "").toLowerCase().includes(q));
  }
  switch (state.sort) {
    case "cpu": pods.sort((a, b) => b.rawCpu - a.rawCpu); break;
    case "mem": pods.sort((a, b) => b.rawMem - a.rawMem); break;
    case "restarts": pods.sort((a, b) => b.restarts - a.restarts); break;
    default:
      pods.sort((a, b) => a.namespace === b.namespace
        ? a.name.localeCompare(b.name)
        : a.namespace.localeCompare(b.namespace));
  }
  state.filtered = pods;
  if (state.selected >= pods.length) state.selected = pods.length - 1;
  if (state.selected < 0 && pods.length > 0) state.selected = 0;
}

function renderPodTable() {
  applyFilter();
  $("#pod-count").textContent = `${state.filtered.length} pods`;
  const tbody = $("#pod-tbody");
  tbody.innerHTML = "";
  state.filtered.forEach((p, i) => {
    const tr = document.createElement("tr");
    if (i === state.selected) tr.classList.add("selected");
    tr.dataset.idx = i;
    tr.innerHTML = `
      <td class="c-ns" title="${p.namespace}">${p.namespace}</td>
      <td class="c-name" title="${p.name}">${p.name}</td>
      <td class="c-ready">${p.ready}</td>
      <td class="c-status"><span class="status ${statusClass(p)}">${p.status}</span></td>
      <td class="c-rest">${p.restarts}</td>
      <td class="c-cpu">${p.cpuUsage}</td>
      <td class="c-mem">${p.memUsage}</td>
      <td class="c-age">${p.age}</td>
      <td class="c-node" title="${p.nodeName}">${p.nodeName || "-"}</td>
      <td class="c-msg" title="${p.message}">${p.message}</td>
    `;
    tr.addEventListener("click", () => {
      state.selected = i;
      renderPodTable();
    });
    tr.addEventListener("dblclick", () => openLogs(p));
    tbody.appendChild(tr);
  });

  // sort button highlights
  $$(".sort").forEach(b => b.classList.toggle("active", b.dataset.sort === state.sort));
}

// ---------- jobs table ----------
function jobStatusClass(j) {
  if (j.status === "Complete") return "status-done";
  if (j.status === "Failed") return "status-err";
  if (j.status === "Running") return "status-ok";
  return "status-pending";
}

// latestRuns collapses a job list to one row per job — the most recent run.
// CronJob executions share an owner (the CronJob), so they group by owner;
// standalone jobs group by their own name. This is what tells you at a glance
// whether each job's *last* run succeeded.
function latestRuns(jobs) {
  const groups = new Map();
  for (const j of jobs) {
    const key = j.namespace + "/" + (j.ownerName || j.name);
    const cur = groups.get(key);
    if (!cur || (j.created || 0) > (cur.created || 0)) groups.set(key, j);
  }
  return [...groups.values()];
}

function renderJobTable() {
  let jobs = state.jobs.slice();
  if (state.jobNsSel) jobs = jobs.filter(j => state.jobNsSel.includes(j.namespace));
  // Default view: only each job's latest run. "all runs" shows full history.
  if (!state.jobAllRuns) jobs = latestRuns(jobs);
  if (state.jobFailedOnly) jobs = jobs.filter(j => j.status === "Failed");
  if (state.jobSearch) {
    const q = state.jobSearch.toLowerCase();
    jobs = jobs.filter(j =>
      j.name.toLowerCase().includes(q) ||
      j.namespace.toLowerCase().includes(q) ||
      (j.ownerName || "").toLowerCase().includes(q));
  }
  // Newest first — most recent runs are what you came to see.
  jobs.sort((a, b) => a.namespace === b.namespace
    ? a.name.localeCompare(b.name)
    : a.namespace.localeCompare(b.namespace));

  const mode = state.jobAllRuns ? "all runs" : "latest run per job";
  $("#job-count").textContent = `${jobs.length} job${jobs.length === 1 ? "" : "s"} · ${mode}`;
  const tbody = $("#job-tbody");
  tbody.innerHTML = "";
  jobs.forEach(j => {
    const owner = j.ownerName ? `${j.ownerKind || ""} ${j.ownerName}`.trim() : "-";
    const tr = document.createElement("tr");
    tr.innerHTML = `
      <td class="c-ns" title="${j.namespace}">${j.namespace}</td>
      <td class="c-name" title="${j.name}">${j.name}</td>
      <td class="c-ready">${j.completions}</td>
      <td class="c-status"><span class="status ${jobStatusClass(j)}">${j.status}</span></td>
      <td class="c-age">${j.duration}</td>
      <td class="c-age" title="${j.deadline ? "time budget: " + fmtDuration(j.deadline) : "no deadline set"}">${fmtDuration(j.deadline)}</td>
      <td class="c-age">${j.age}</td>
      <td class="c-node" title="${owner}">${owner}</td>
      <td class="c-msg" title="${j.message || ""}">${j.message || ""}</td>
    `;
    tbody.appendChild(tr);
  });
}

// ---------- resizable columns ----------
// Drag a header's right edge to resize a column; double-click the edge to
// auto-fit it to the widest value. Widths are driven by a <colgroup> (the
// reliable way to size table-layout:fixed columns), frozen to pixels on first
// interaction, and persisted per table — so long pod/job names can be read in
// full (the table grows and the wrapper scrolls rather than truncating).
function makeResizable(table) {
  if (!table || !table.tHead) return;
  const ths = Array.from(table.tHead.rows[0].cells);

  let cg = table.querySelector("colgroup");
  if (!cg) {
    cg = document.createElement("colgroup");
    ths.forEach(() => cg.appendChild(document.createElement("col")));
    table.insertBefore(cg, table.firstChild);
  }
  const cols = Array.from(cg.children);

  const saved = readCols(table.id);
  if (saved && saved.length === cols.length) {
    cols.forEach((c, i) => { c.style.width = saved[i] + "px"; });
    table.dataset.frozen = "1";
    setTableWidth(table, cols);
  }

  ths.forEach((th, i) => {
    const grip = document.createElement("span");
    grip.className = "col-resizer";
    grip.addEventListener("mousedown", e => startColResize(e, table, cols, i, grip));
    grip.addEventListener("dblclick", e => { e.preventDefault(); autoFitCol(table, cols, i); });
    th.appendChild(grip);
  });
}

function freezeCols(table, cols) {
  if (table.dataset.frozen) return;
  const ths = table.tHead.rows[0].cells;
  cols.forEach((c, i) => { c.style.width = ths[i].offsetWidth + "px"; });
  table.dataset.frozen = "1";
  setTableWidth(table, cols);
}

function setTableWidth(table, cols) {
  let total = 0;
  cols.forEach(c => { total += parseFloat(c.style.width) || 0; });
  if (total) table.style.width = total + "px";
}

function startColResize(e, table, cols, i, grip) {
  e.preventDefault();
  freezeCols(table, cols);
  const startX = e.clientX;
  const startW = parseFloat(cols[i].style.width) || table.tHead.rows[0].cells[i].offsetWidth;
  document.body.classList.add("col-resizing");
  grip.classList.add("dragging");
  const move = ev => {
    cols[i].style.width = Math.max(48, startW + (ev.clientX - startX)) + "px";
    setTableWidth(table, cols);
  };
  const up = () => {
    document.removeEventListener("mousemove", move);
    document.removeEventListener("mouseup", up);
    document.body.classList.remove("col-resizing");
    grip.classList.remove("dragging");
    saveCols(table, cols);
  };
  document.addEventListener("mousemove", move);
  document.addEventListener("mouseup", up);
}

// Measure a column's widest content with canvas text metrics — robust against
// the fixed-layout clipping that makes scrollWidth unreliable here.
function cssFont(el) {
  const cs = getComputedStyle(el);
  // The `font` shorthand comes back empty when set via longhands, so assemble
  // a valid canvas font string ourselves.
  return `${cs.fontStyle} ${cs.fontWeight} ${cs.fontSize} ${cs.fontFamily}`;
}

function measureCol(table, i) {
  const th = table.tHead.rows[0].cells[i];
  const ctx = measureCol._ctx || (measureCol._ctx = document.createElement("canvas").getContext("2d"));
  let max = 0;
  ctx.font = cssFont(th);
  max = Math.max(max, ctx.measureText(th.textContent.trim()).width);
  const rows = table.tBodies[0] ? table.tBodies[0].rows : [];
  if (rows.length) ctx.font = cssFont(rows[0].cells[i] || th);
  for (const r of rows) {
    const cell = r.cells[i];
    if (cell) max = Math.max(max, ctx.measureText(cell.textContent.trim()).width);
  }
  return Math.ceil(max) + 22; // cell padding (20) + small buffer
}

function autoFitCol(table, cols, i) {
  freezeCols(table, cols);
  cols[i].style.width = measureCol(table, i) + "px";
  setTableWidth(table, cols);
  saveCols(table, cols);
}

function readCols(id) {
  try { return JSON.parse(localStorage.getItem("clusterheart:cols:" + id) || "null"); }
  catch (_) { return null; }
}
function saveCols(table, cols) {
  const ws = cols.map(c => Math.round(parseFloat(c.style.width) || 0));
  try { localStorage.setItem("clusterheart:cols:" + table.id, JSON.stringify(ws)); } catch (_) {}
}

// ---------- nodes ----------
function nodeBar(p) {
  const cls = p == null ? "" : p > 80 ? " hot" : p > 50 ? " warn" : "";
  const w = p == null ? 0 : Math.max(0, Math.min(100, p));
  return `<span class="bar${cls}"><i style="width:${w}%"></i></span>`;
}

// Control-plane / master nodes carry a node-role.kubernetes.io/{master,
// control-plane} label, surfaced by the backend as the node's role. Managed
// control planes (EKS/GKE) hide these, so there may be none.
function isControlPlane(n) { return /master|control-plane/i.test(n.role || ""); }

function renderNodes() {
  const wrap = $("#node-list");
  wrap.innerHTML = "";

  // Cluster totals for the summary row.
  let tCpuCap = 0, tCpuUse = 0, tMemCap = 0, tMemUse = 0, tPods = 0, tPodLimit = 0;
  let tEphCap = 0, tEphUse = 0, ephKnown = false, readyCount = 0, cpCount = 0;
  for (const n of state.nodes) {
    tCpuCap += n.cpuCap; tCpuUse += n.cpuUsed;
    tMemCap += n.memCap; tMemUse += n.memUsed;
    tPods += n.podCount; tPodLimit += n.podLimit;
    tEphCap += n.ephCap;
    if (n.ephHasUsed) { tEphUse += n.ephUsed; ephKnown = true; }
    if (n.ready) readyCount++;
    if (isControlPlane(n)) cpCount++;
  }
  renderNodeStats({ tCpuCap, tCpuUse, tMemCap, tMemUse, tPods, tPodLimit, tEphCap, tEphUse, ephKnown, readyCount, cpCount });

  // Control-plane nodes first, then by name — so the master is easy to spot.
  const ordered = state.nodes.slice().sort((a, b) =>
    (isControlPlane(b) - isControlPlane(a)) || a.name.localeCompare(b.name));

  ordered.forEach(n => {
    const cpuP = pct(n.cpuUsed, n.cpuCap);
    const memP = pct(n.memUsed, n.memCap);
    const ephP = n.ephHasUsed ? pct(n.ephUsed, n.ephUsed + n.ephAvail) : null;
    const ready = n.ready
      ? `<span class="status-ok">Ready</span>`
      : `<span class="status-err">NotReady</span>`;
    const cp = isControlPlane(n);
    const roleBadge = cp
      ? `<span class="role-cp" title="control-plane node">◆ ${n.role}</span>`
      : `<span>${n.role}</span>`;
    const box = document.createElement("div");
    box.className = cp ? "node node-cp" : "node";
    box.innerHTML = `
      <div class="head">
        <div class="name" title="${n.name}">${n.name}</div>
        <div class="badges">${roleBadge}<span>${ready}</span><span>${n.age}</span><span>${n.podCount}/${n.podLimit} pods</span></div>
      </div>
      <div class="row">
        <span class="lbl">CPU</span>
        ${nodeBar(cpuP)}
        <span class="pct">${cpuP !== null ? cpuP.toFixed(0) + "%" : "n/a"}</span>
        <span class="used">${fmtCpu(n.cpuUsed)} / ${fmtCpu(n.cpuCap)}</span>
      </div>
      <div class="row">
        <span class="lbl">MEM</span>
        ${nodeBar(memP)}
        <span class="pct">${memP !== null ? memP.toFixed(0) + "%" : "n/a"}</span>
        <span class="used">${fmtBytes(n.memUsed)} / ${fmtBytes(n.memCap)}</span>
      </div>
      <div class="row">
        <span class="lbl">EPH</span>
        ${ephP !== null ? nodeBar(ephP) : `<span class="bar-na">live usage n/a</span>`}
        <span class="pct">${ephP !== null ? ephP.toFixed(0) + "%" : "—"}</span>
        <span class="used">${ephP !== null ? `${fmtBytes(n.ephUsed)} / ${fmtBytes(n.ephCap)}` : fmtBytes(n.ephCap) + " cap"}</span>
      </div>
    `;
    wrap.appendChild(box);
  });
  $("#node-summary").textContent = state.nodes.length
    ? `${state.nodes.length} nodes · cpu ${fmtCpu(tCpuUse)}/${fmtCpu(tCpuCap)} · mem ${fmtBytes(tMemUse)}/${fmtBytes(tMemCap)}`
    : "no nodes";

  renderConsolidation();
}

// usageClass maps a utilisation percent to a severity color for the stat tiles.
function usageClass(p) {
  if (p === null) return "";
  if (p > 85) return "hot";
  if (p > 65) return "warn";
  return "ok";
}

function renderNodeStats(t) {
  const el = $("#node-stats");
  if (!el) return;
  if (!state.nodes.length) { el.innerHTML = ""; return; }
  const cpuP = pct(t.tCpuUse, t.tCpuCap);
  const memP = pct(t.tMemUse, t.tMemCap);
  const ephP = t.ephKnown ? pct(t.tEphUse, t.tEphCap) : null;
  const nodeSub = t.cpCount
    ? `${t.readyCount}/${state.nodes.length} ready · ${t.cpCount} control-plane`
    : `${t.readyCount}/${state.nodes.length} ready`;
  const tile = (val, unit, label, sub, cls = "") => `
    <div class="nstat">
      <div class="nstat-v ${cls}">${val}${unit ? `<span class="u">${unit}</span>` : ""}</div>
      <div class="nstat-l">${label}</div>
      <div class="nstat-sub">${sub}</div>
    </div>`;
  el.innerHTML =
    tile(state.nodes.length, "", "nodes", nodeSub) +
    tile(cpuP !== null ? cpuP.toFixed(0) : "—", "%", "cpu used",
      `${fmtCpu(t.tCpuUse)} / ${fmtCpu(t.tCpuCap)}`, usageClass(cpuP)) +
    tile(memP !== null ? memP.toFixed(0) : "—", "%", "memory used",
      `${fmtBytes(t.tMemUse)} / ${fmtBytes(t.tMemCap)}`, usageClass(memP)) +
    tile(t.tPods, "", "pods", `${t.tPods} / ${t.tPodLimit} slots`) +
    (t.ephKnown
      ? tile(ephP !== null ? ephP.toFixed(0) : "—", "%", "storage used",
          `${fmtBytes(t.tEphUse)} / ${fmtBytes(t.tEphCap)}`, usageClass(ephP))
      : tile(fmtBytes(t.tEphCap), "", "storage", "capacity"));
}

// Capacity analysis settles for a short cool-down before it reveals, so it
// eases in rather than snapping up the instant the nodes view opens. Re-armed
// on every entry to the nodes view (see switchView).
const CAP_COOLDOWN_MS = 2500;
function armCapacityCooldown() {
  state.capReady = false;
  clearTimeout(state.capTimer);
  renderConsolidation(); // show the "analyzing…" placeholder now
  state.capTimer = setTimeout(() => {
    state.capReady = true;
    renderConsolidation(); // reveal the real analysis
  }, CAP_COOLDOWN_MS);
}

function renderConsolidation() {
  const el = $("#consolidation-card");
  if (!el) return;
  if (!state.nodes.length || !state.pods.length) { el.innerHTML = ""; return; }

  const active = state.pods.filter(isActive);
  if (!active.length) { el.innerHTML = ""; return; }

  // During the cool-down, hold an unobtrusive placeholder instead of the card.
  if (!state.capReady) {
    el.innerHTML = `
      <div class="card-head">
        <span class="title">capacity analysis</span>
        <span class="grow"></span>
        <span class="dim">analyzing…</span>
      </div>
      <div class="card-body">
        <div class="cap-wait"><span class="lt-spin"></span> analyzing capacity across ${state.nodes.length} node${state.nodes.length === 1 ? "" : "s"} and ${active.length} pods…</div>
      </div>`;
    return;
  }

  // Collapsible header. Clicking it toggles state.consolidationCollapsed, which
  // survives the 5s node re-render so the card stays where the user left it.
  const head = (badge) => `
    <div class="card-head" role="button" tabindex="0" aria-expanded="${!state.consolidationCollapsed}">
      <span class="chev" aria-hidden="true">▾</span>
      <span class="title">capacity analysis</span>
      <span class="grow"></span>
      ${badge}
    </div>`;
  const wireCollapse = () => {
    el.classList.toggle("collapsed", state.consolidationCollapsed);
    const h = $(".card-head", el);
    if (!h) return;
    const toggle = () => {
      state.consolidationCollapsed = !state.consolidationCollapsed;
      el.classList.toggle("collapsed", state.consolidationCollapsed);
      h.setAttribute("aria-expanded", String(!state.consolidationCollapsed));
    };
    h.addEventListener("click", toggle);
    h.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggle(); }
    });
  };

  let totalCpuReq = 0, totalMemReq = 0, totalCpuUse = 0, totalMemUse = 0;
  for (const p of active) {
    totalCpuReq += p.cpuRequest;
    totalMemReq += p.memRequest;
    totalCpuUse += p.rawCpu;
    totalMemUse += p.rawMem;
  }
  // node template = average across current nodes (rough but matches what the
  // ASG/MNG would re-launch as)
  let nodeCpu = 0, nodeMem = 0, podLimit = 0;
  for (const n of state.nodes) {
    nodeCpu += n.cpuCap; nodeMem += n.memCap;
    podLimit = Math.max(podLimit, n.podLimit);
  }
  nodeCpu = Math.floor(nodeCpu / state.nodes.length);
  nodeMem = Math.floor(nodeMem / state.nodes.length);
  if (podLimit === 0) podLimit = 110;

  // Min by live usage (with 30% safety headroom)
  const headroom = 1.3;
  const minCpuUse  = Math.max(1, Math.ceil((totalCpuUse * headroom) / nodeCpu));
  const minMemUse  = Math.max(1, Math.ceil((totalMemUse * headroom) / nodeMem));
  const minPods    = Math.max(1, Math.ceil(active.length / podLimit));
  const minNodes   = Math.max(minCpuUse, minMemUse, minPods);
  // Min by requests (more conservative; reflects what scheduler would honour)
  const minCpuReq  = Math.max(1, Math.ceil(totalCpuReq / nodeCpu));
  const minMemReq  = Math.max(1, Math.ceil(totalMemReq / nodeMem));
  const minNodesReq = Math.max(minCpuReq, minMemReq, minPods);

  const current = state.nodes.length;
  const removableNow = current - minNodesReq;    // drainable given current requests
  const removableIdeal = current - minNodes;     // reachable after right-sizing

  // What's the binding dimension for the live-usage minimum?
  const liveBinding = [];
  if (minCpuUse === minNodes) liveBinding.push("cpu");
  if (minMemUse === minNodes) liveBinding.push("mem");
  if (minPods   === minNodes) liveBinding.push("pod-count");

  // What's the binding dimension for the request minimum?
  const reqBinding = [];
  if (minCpuReq === minNodesReq) reqBinding.push("cpu");
  if (minMemReq === minNodesReq) reqBinding.push("mem");
  if (minPods   === minNodesReq) reqBinding.push("pod-count");

  // Phantom = capacity the scheduler reserves for these pods that they aren't
  // actually using. Because kube-scheduler places by requests (not live usage),
  // this is what pins extra nodes in place.
  const phantomCpu = Math.max(0, totalCpuReq - totalCpuUse);
  const phantomMem = Math.max(0, totalMemReq - totalMemUse);

  const peak = Math.max(current, minNodesReq, minNodes, 1);
  const bar = (label, val, klass, note) => `
    <div class="proj-row">
      <span class="lbl">${label}</span>
      <span class="track"><span class="fill ${klass}" style="width:${(val / peak * 100).toFixed(1)}%"></span></span>
      <span class="val">${val}</span>
      ${note ? `<span class="note dim">${note}</span>` : ""}
    </div>`;

  // Already at the floor on both requests and live usage — nothing to do.
  if (removableNow <= 0 && removableIdeal <= 0) {
    el.innerHTML = `
      ${head(`<span class="badge-tag ok">at floor</span>`)}
      <div class="card-body">
        <div class="proj">
          ${bar("current", current, "ok")}
          ${bar("needed", minNodesReq, "ok", `binding: ${reqBinding.join(" + ")}`)}
        </div>
        <div class="cap-note">Running at the minimum node count these workloads fit on — nothing to consolidate.</div>
      </div>`;
    wireCollapse();
    return;
  }

  const badge = removableNow > 0
    ? `<span class="badge-tag">${removableNow} node${removableNow === 1 ? "" : "s"} removable</span>`
    : `<span class="badge-tag ok">right-size to shrink</span>`;

  const verdict = removableNow > 0
    ? `${current} nodes running, ${minNodesReq} needed for current reservations — <b>${removableNow} node${removableNow === 1 ? "" : "s"} can be drained now</b>.`
    : `All ${current} nodes are pinned by <b>requests</b>, but live usage fits on ${minNodes}. Right-size over-requested pods to free <b>${removableIdeal} node${removableIdeal === 1 ? "" : "s"}</b>.`;

  el.innerHTML = `
    ${head(badge)}
    <div class="card-body">
      <div class="proj">
        ${bar("current", current, "warn")}
        ${bar("needed now", minNodesReq, removableNow > 0 ? "ok" : "hot", `by requests · binding: ${reqBinding.join(" + ")}`)}
        ${minNodes < minNodesReq ? bar("after right-sizing", minNodes, "ok", `by live usage +${Math.round((headroom - 1) * 100)}% · ${liveBinding.join(" + ")}`) : ""}
      </div>
      <div class="verdict-line ${removableNow > 0 ? "ok" : "warn"}">${verdict}</div>
      <div class="cap-note">Scheduler is holding <b>${fmtCpu(phantomCpu)}</b> cpu and <b>${fmtBytes(phantomMem)}</b> mem that pods reserved but aren't using — that's what keeps the extra capacity alive. Right-size the worst offenders from the <b>Resources</b> tab.</div>
    </div>
  `;

  wireCollapse();
}

// ---------- resources ----------
// workloadName is the label a group of replicas collapses to. For Deployments
// the pod's controller is a ReplicaSet named <deployment>-<hash>, so strip the
// trailing hash; other owners (DaemonSet/StatefulSet/CronJob) use their name.
function workloadName(p) {
  if (!p.ownerKind || !p.ownerName) return p.name;
  if (p.ownerKind === "ReplicaSet") return p.ownerName.replace(/-[^-]+$/, "");
  return p.ownerName;
}

// ---------- rightsizing (history-backed) ----------
// Reads /api/rightsizing, which aggregates the last N hours of MongoDB usage
// samples per workload and returns a p95/peak-based DOWNSIZE/OK/UPSIZE verdict.
async function loadRightsizing() {
  state.rsLoading = true;
  renderRightsizing();
  try {
    const ns = state.rsNs && state.rsNs !== "ALL" ? `?ns=${encodeURIComponent(state.rsNs)}` : "";
    state.rs = (await jget("/api/rightsizing" + ns)) || { ready: false, rows: [] };
  } catch (e) {
    state.rs = { ready: false, reason: e.message, rows: [] };
  } finally {
    state.rsLoading = false;
    renderRightsizing();
  }
}

// find a live pod for a workload so the patch modal can resolve its controller
function podForWorkload(ns, workload) {
  return state.pods.find(p => isActive(p) && p.namespace === ns && workloadName(p) === workload);
}

// tone for a p95 cell: red near/over request, green when there's lots of headroom
function pctToneCls(used, req) {
  const p = pct(used, req);
  if (p === null) return "";
  if (p > 90) return "pct-hot";
  if (p < 40) return "pct-cool";
  return "";
}

function fmtWindow(mins) {
  if (!mins) return "6h";
  if (mins % 60 === 0) return (mins / 60) + "h";
  if (mins < 60) return mins + "m";
  return (mins / 60).toFixed(1) + "h";
}

function renderRsSummary(rs, c) {
  const win = fmtWindow(rs.windowMins);
  const learningNote = c.learning
    ? `<div class="line">${c.learning} workload${c.learning === 1 ? "" : "s"} still <b>learning</b> — need ${win} of history before advising.</div>` : "";
  let head, kind;
  if (c.up > 0) { kind = "up"; head = `${c.up} workload${c.up === 1 ? "" : "s"} under-provisioned — p95 near the request`; }
  else if (c.down > 0) { kind = "down"; head = `${c.down} workload${c.down === 1 ? "" : "s"} over-provisioned — safe to downsize`; }
  else { kind = "ok"; head = `allocation looks right-sized over the ${win} window`; }
  return `<div class="rec-banner ${kind}">
    <div class="head">${head}</div>
    <div class="line">Verdicts use the <b>95th percentile</b> and <b>peak</b> per-pod usage over the window, so a spiky workload isn't told to shrink on a quiet moment.</div>
    ${learningNote}
  </div>`;
}

function renderRightsizing() {
  const tbody = $("#rs-tbody");
  if (!tbody) return;
  const rs = state.rs || { ready: false, rows: [] };
  $("#rs-window").textContent = `${fmtWindow(rs.windowMins)} history · p95 + peak per pod`;

  if (state.rsLoading && (!rs.rows || rs.rows.length === 0)) {
    tbody.innerHTML = `<tr><td colspan="10" class="dim" style="padding:14px">loading…</td></tr>`;
    $("#rs-summary").innerHTML = "";
    return;
  }
  if (rs.ready === false && (!rs.rows || rs.rows.length === 0)) {
    tbody.innerHTML = `<tr><td colspan="10" class="dim" style="padding:14px">${rs.reason || "history unavailable — start MongoDB (docker compose up -d) and relaunch."}</td></tr>`;
    $("#rs-summary").innerHTML = "";
    return;
  }

  const all = rs.rows || [];
  const learning = all.filter(r => r.verdict === "LEARNING").length;
  const down = all.filter(r => r.verdict.startsWith("DOWNSIZE")).length;
  const up = all.filter(r => r.verdict.startsWith("UPSIZE")).length;
  $("#rs-summary").innerHTML = renderRsSummary(rs, { learning, down, up });

  let rows = all.slice();
  if (state.rsNs !== "ALL") rows = rows.filter(r => r.namespace === state.rsNs);
  if (state.rsSearch) {
    const q = state.rsSearch.toLowerCase();
    rows = rows.filter(r => r.workload.toLowerCase().includes(q) ||
      r.namespace.toLowerCase().includes(q) || r.verdict.toLowerCase().includes(q));
  }
  if (state.rsIssuesOnly) rows = rows.filter(r => r.verdict !== "OK" && r.verdict !== "LEARNING");
  rows.sort((a, b) => a.namespace.localeCompare(b.namespace) || a.workload.localeCompare(b.workload));

  if (rows.length === 0) {
    tbody.innerHTML = `<tr><td colspan="10" class="dim" style="padding:14px">no workloads${state.rsNs !== "ALL" ? " in " + state.rsNs : ""}.</td></tr>`;
    return;
  }

  tbody.innerHTML = "";
  rows.forEach((r, i) => {
    const rep = podForWorkload(r.namespace, r.workload);
    const managed = isSystemNamespace(r.namespace);
    const actionable = r.verdict.startsWith("DOWNSIZE") || r.verdict.startsWith("UPSIZE");
    let actionCell;
    if (managed) actionCell = `<td class="act"><span class="managed-tag" title="Platform-managed namespace — patch via the EKS add-on/operator, not here.">managed</span></td>`;
    else if (rep && actionable) actionCell = `<td class="act"><button class="patch-btn" data-rs="${i}">patch</button></td>`;
    else actionCell = `<td class="act"><span class="dim">—</span></td>`;

    const suggest = (r.suggestCpu || r.suggestMem)
      ? `<div class="rs-suggest">→ ${r.suggestCpu ? fmtCpu(r.suggestCpu) : ""}${r.suggestCpu && r.suggestMem ? " · " : ""}${r.suggestMem ? fmtBytes(r.suggestMem) : ""}</div>` : "";

    const tr = document.createElement("tr");
    tr.innerHTML = `
      <td>${r.namespace}</td>
      <td title="${r.ownerKind}/${r.workload} · ${r.replicas} replica${r.replicas === 1 ? "" : "s"} · ${r.samples} samples over ${fmtWindow(r.windowMins)}">${r.workload} <span class="dim">×${r.replicas}</span></td>
      <td class="num">${fmtCpu(r.cpuRequest)}</td>
      <td class="num ${pctToneCls(r.cpuP95, r.cpuRequest)}">${fmtCpu(r.cpuP95)}</td>
      <td class="num dim">${fmtCpu(r.cpuPeak)}</td>
      <td class="num">${fmtBytes(r.memRequest)}</td>
      <td class="num ${pctToneCls(r.memP95, r.memRequest)}">${fmtBytes(r.memP95)}</td>
      <td class="num dim">${fmtBytes(r.memPeak)}</td>
      <td class="vrd"><span class="${r.verdictCls}">${r.verdict}</span>${suggest}</td>
      ${actionCell}
    `;
    tbody.appendChild(tr);
  });

  $$("button.patch-btn[data-rs]", tbody).forEach(btn => {
    btn.addEventListener("click", (e) => {
      e.stopPropagation();
      const r = rows[parseInt(btn.dataset.rs, 10)];
      const rep = r && podForWorkload(r.namespace, r.workload);
      if (rep) openPatchModal(rep);
    });
  });

}

// ---------- autoscalers (HPA) ----------
// state.hpas holds every HPA in the cluster (fetched once per refresh); both
// the autoscalers view and the resources table read from it. hpaIndex maps a
// workload to its HPA so the resources rows can show scaling inline.
function rebuildHpaIndex() {
  const idx = new Map();
  for (const h of state.hpas || []) {
    // Key on the scale target the workload row can produce: namespace + the
    // controller name (Deployment/StatefulSet/DaemonSet name).
    idx.set(`${h.namespace}|${h.refName}`, h);
  }
  state.hpaIndex = idx;
}
function hpaFor(namespace, name) {
  return state.hpaIndex ? state.hpaIndex.get(`${namespace}|${name}`) : undefined;
}

async function loadHPAs() {
  state.hpaLoading = true;
  renderHPAs();
  try {
    state.hpas = (await jget("/api/hpas")) || [];
    rebuildHpaIndex();
  } catch (e) {
    state.hpas = [];
    setMsg("hpa load failed: " + e.message);
  } finally {
    state.hpaLoading = false;
    renderHPAs();
    renderRightsizing();
  }
}

// A metric pill reads "cpu 31% / 80%" with a mini fill bar of current-vs-target.
// >=90% of target = hot (about to scale up), <37.5% = cool (headroom / scaling
// down), otherwise neutral. currentPct === -1 means no reading yet (a
// freshly-scaled pod that hasn't reported metrics).
function hpaMetricCell(m) {
  const target = m.target || "—";
  const cur = m.current || "—";
  let tone = "hpa-mid", fill = 0, hasPct = m.currentPct >= 0 && m.targetPct > 0;
  if (hasPct) {
    const ratio = m.currentPct / m.targetPct;
    tone = ratio >= 0.9 ? "hpa-hot" : ratio < 0.375 ? "hpa-cool" : "hpa-mid";
    fill = Math.min(100, (m.currentPct / m.targetPct) * 100);
  }
  const bar = hasPct
    ? `<span class="hpa-metric-track"><span class="hpa-metric-fill ${tone}" style="width:${fill}%"></span></span>`
    : "";
  return `<span class="hpa-metric ${tone}">`
    + `<span class="hpa-metric-name">${m.name}</span>`
    + `<span class="hpa-metric-val">${cur}<span class="dim"> / ${target}</span></span>`
    + bar
    + `</span>`;
}

// hpaBadge renders a compact autoscaler cell for the resources table: the
// replica band (min–max), the live count, and the hottest metric's headroom.
// Tooltip carries the target names and the current scaling reason.
function hpaBadge(h) {
  if (!h) return `<span class="dim">—</span>`;
  const atMax = h.currentReplicas >= h.maxReplicas;
  const atMin = h.currentReplicas <= h.minReplicas;
  const cls = atMax ? "hpa-pill-max" : atMin ? "hpa-pill-min" : "hpa-pill-mid";

  // hottest metric = closest to (or over) its target — that's what drives the
  // next scale-up, so it's the one worth surfacing next to the count.
  let hot = null;
  for (const m of h.metrics || []) {
    if (m.currentPct < 0 || m.targetPct <= 0) continue;
    const ratio = m.currentPct / m.targetPct;
    if (!hot || ratio > hot.ratio) hot = { m, ratio };
  }
  const metricStr = hot
    ? `<span class="hpa-pill-metric">${hot.m.name} ${hot.m.current}</span>` : "";

  const conds = h.conditions || [];
  const limited = conds.find(c => c.type === "ScalingLimited" && c.status === "True");
  const tip = [
    `${h.refKind}/${h.refName}`,
    `replicas ${h.minReplicas}–${h.maxReplicas}, now ${h.currentReplicas}` + (atMax ? " (at MAX)" : atMin ? " (at min)" : ""),
    ...(h.metrics || []).map(m => `${m.name}: ${m.current} / ${m.target}`),
    limited ? `limited: ${limited.reason}` : "",
    h.lastScale ? `last scale: ${h.lastScale} ago` : "",
  ].filter(Boolean).join("\n");

  return `<span class="hpa-badge" title="${escapeAttr(tip)}">`
    + `<span class="hpa-pill ${cls}">${h.currentReplicas}<span class="hpa-pill-den">/${h.maxReplicas}</span></span>`
    + metricStr + `</span>`;
}

function renderHPAs() {
  const tbody = $("#hpa-tbody");
  if (!tbody) return;
  const q = state.hpaSearch.trim().toLowerCase();
  let rows = state.hpas.slice();
  if (state.hpaNs !== "ALL") rows = rows.filter(h => h.namespace === state.hpaNs);
  if (q) {
    rows = rows.filter(h =>
      h.name.toLowerCase().includes(q) ||
      (h.refName || "").toLowerCase().includes(q) ||
      (h.namespace || "").toLowerCase().includes(q));
  }

  if (state.hpaLoading && rows.length === 0) {
    tbody.innerHTML = `<tr><td colspan="6" class="dim" style="padding:14px">loading…</td></tr>`;
    $("#hpa-summary").textContent = "";
    return;
  }
  if (rows.length === 0) {
    tbody.innerHTML = `<tr><td colspan="6" class="dim" style="padding:14px">no autoscalers found${state.hpaNs !== "ALL" ? " in " + state.hpaNs : ""}.</td></tr>`;
    $("#hpa-summary").textContent = "";
    return;
  }

  tbody.innerHTML = rows.map(h => {
    const metrics = (h.metrics || []).map(hpaMetricCell).join("") || "<span class=\"dim\">—</span>";
    return `<tr>
      <td>${h.namespace}</td>
      <td class="hpa-name">${h.name}</td>
      <td class="dim hpa-target" title="${escapeAttr((h.refKind || "") + "/" + (h.refName || ""))}">${h.refKind || ""}/${h.refName || ""}</td>
      <td class="hpa-replicas-col">${hpaReplicasCell(h)}</td>
      <td class="hpa-metrics-col">${metrics}</td>
      <td class="num dim">${h.lastScale ? h.lastScale + " ago" : "—"}</td>
    </tr>`;
  }).join("");

  const maxed = rows.filter(h => h.currentReplicas >= h.maxReplicas).length;
  $("#hpa-summary").textContent =
    `${rows.length} autoscaler${rows.length === 1 ? "" : "s"}` + (maxed ? ` · ${maxed} at max replicas` : "");
}

// hpaReplicasCell draws the min→max band with a filled track up to the current
// count, plus a "cur / max" readout. Green when parked at min (headroom), red
// when pinned at max (out of room), neutral in between.
function hpaReplicasCell(h) {
  const min = h.minReplicas, max = h.maxReplicas, cur = h.currentReplicas;
  const atMax = cur >= max, atMin = cur <= min;
  const span = Math.max(1, max - min);
  const fill = Math.min(100, Math.max(0, ((cur - min) / span) * 100));
  const cntCls = atMax ? "hpa-repl-max" : atMin ? "hpa-repl-min" : "hpa-repl-mid";
  const barCls = atMax ? "hpa-hot" : atMin ? "hpa-cool" : "hpa-mid-bar";
  return `<div class="hpa-repl" title="min ${min} · current ${cur} · max ${max}">
    <span class="hpa-repl-num ${cntCls}">${cur}</span>
    <span class="hpa-repl-track"><span class="hpa-repl-fill ${barCls}" style="width:${fill}%"></span></span>
    <span class="hpa-repl-band dim">${min}–${max}</span>
  </div>`;
}

function escapeAttr(s) {
  return String(s || "").replace(/"/g, "&quot;").replace(/</g, "&lt;");
}

// ---------- patch modal ----------
function suggestedCpu(p) {
  if (p.rawCpu <= 0) return Math.max(10, Math.floor(p.cpuRequest * 0.2));
  return Math.max(10, Math.ceil(p.rawCpu * 1.3));
}
function suggestedMem(p) {
  const floor = 64 * 1024 * 1024;
  if (p.rawMem <= 0) return Math.max(floor, Math.floor(p.memRequest * 0.2));
  return Math.max(floor, Math.ceil(p.rawMem * 1.3));
}
// k8s-friendly memory string: round up to nearest Mi
function memToK8s(b) {
  if (b <= 0) return "";
  const mi = Math.ceil(b / (1024 * 1024));
  return `${mi}Mi`;
}
function cpuToK8s(milli) {
  if (milli <= 0) return "";
  return `${milli}m`;
}

let patchCtx = null;

async function openPatchModal(p) {
  // Backstop: never patch platform-managed namespaces even if invoked directly.
  if (isSystemNamespace(p.namespace)) {
    setMsg(`${p.namespace} is platform-managed — change it via the EKS add-on / operator, not a live patch`);
    return;
  }
  patchCtx = { pod: p, owner: null };
  $("#patch-title").textContent = `patch · ${p.namespace}/${p.name}`;
  $("#patch-meta").textContent = "resolving controller…";
  $("#patch-grid").innerHTML = "";
  $("#patch-cmd").textContent = "—";
  $("#patch-err").textContent = "";
  $("#patch-result").textContent = "";
  $("#patch-result").classList.remove("err");
  $("#patch-modal").classList.remove("hidden");

  try {
    const owner = await jget(`/api/pod/${encodeURIComponent(p.namespace)}/${encodeURIComponent(p.name)}/owner`);
    patchCtx.owner = owner;
    renderPatchForm();
  } catch (e) {
    $("#patch-meta").innerHTML = `<span class="status-err">cannot resolve controller: ${e.message}</span>`;
    $("#patch-grid").innerHTML = `<div style="grid-column: 1 / -1; color: var(--muted); font-size: 11.5px">
      this pod is not owned by a Deployment/StatefulSet/DaemonSet/Job — there's nothing to patch from here.<br>
      edit the pod or its owner directly with <code>kubectl edit</code>.
    </div>`;
  }
}

function renderPatchForm() {
  const { pod, owner } = patchCtx;
  $("#patch-meta").innerHTML = `controller: <b>${owner.kind}/${owner.name}</b> in <b>${owner.namespace}</b> · ${owner.containers.length} container${owner.containers.length === 1 ? "" : "s"}`;

  const containerSelect = owner.containers.length > 1
    ? `<select id="patch-container">${owner.containers.map((c, i) => `<option value="${i}">${c.name}</option>`).join("")}</select>`
    : `<span class="dim">${owner.containers[0]?.name || "—"}</span>`;

  const initCpu = cpuToK8s(suggestedCpu(pod));
  const initMem = memToK8s(suggestedMem(pod));
  const initLimCpu = pod.cpuLimit > 0 ? cpuToK8s(pod.cpuLimit) : "";
  const initLimMem = pod.memLimit > 0 ? memToK8s(pod.memLimit) : "";

  $("#patch-grid").innerHTML = `
    <div></div>
    <div class="h">current</div>
    <div class="h">new (suggested)</div>

    <div class="lbl">cpu req</div>
    <div class="current">${pod.cpuRequest > 0 ? cpuToK8s(pod.cpuRequest) : "—"} <span class="dim">(use ${cpuToK8s(pod.rawCpu)})</span></div>
    <div><input id="patch-cpu-req" value="${initCpu}" /></div>

    <div class="lbl">mem req</div>
    <div class="current">${pod.memRequest > 0 ? memToK8s(pod.memRequest) : "—"} <span class="dim">(use ${memToK8s(pod.rawMem)})</span></div>
    <div><input id="patch-mem-req" value="${initMem}" /></div>

    <div class="lbl">cpu lim</div>
    <div class="current">${pod.cpuLimit > 0 ? cpuToK8s(pod.cpuLimit) : "—"}</div>
    <div><input id="patch-cpu-lim" value="${initLimCpu}" placeholder="(leave blank to skip)" /></div>

    <div class="lbl">mem lim</div>
    <div class="current">${pod.memLimit > 0 ? memToK8s(pod.memLimit) : "—"}</div>
    <div><input id="patch-mem-lim" value="${initLimMem}" placeholder="(leave blank to skip)" /></div>

    <div class="container-row">
      <span class="lbl">container</span>
      ${containerSelect}
      <span class="dim">applies to the selected container only</span>
    </div>
  `;
  $$("#patch-grid input, #patch-container").forEach(el => el.addEventListener("input", updatePatchCmd));
  if ($("#patch-container")) $("#patch-container").addEventListener("change", updatePatchCmd);
  updatePatchCmd();
}

function readPatchBody() {
  const { owner } = patchCtx;
  const idx = $("#patch-container") ? parseInt($("#patch-container").value, 10) : 0;
  const container = owner.containers[idx]?.name || "";
  return {
    container,
    requests: { cpu: $("#patch-cpu-req").value.trim(), memory: $("#patch-mem-req").value.trim() },
    limits:   { cpu: $("#patch-cpu-lim").value.trim(), memory: $("#patch-mem-lim").value.trim() },
  };
}

async function updatePatchCmd() {
  const { owner } = patchCtx;
  const body = readPatchBody();
  try {
    const r = await fetch(`/api/owner/${encodeURIComponent(owner.namespace)}/${encodeURIComponent(owner.kind)}/${encodeURIComponent(owner.name)}/patch`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ...body, dryRun: true }),
    });
    const j = await r.json();
    $("#patch-cmd").textContent = j.command || j.error || "—";
    $("#patch-err").textContent = r.ok ? "" : (j.error || "");
  } catch (e) {
    $("#patch-cmd").textContent = "—";
    $("#patch-err").textContent = e.message;
  }
}

async function applyPatch() {
  const { owner, pod } = patchCtx;
  if (!confirmSync(`Apply new requests/limits to ${owner.kind}/${owner.name} in ${owner.namespace}?\n\nThis will trigger a rolling update on the controller.`)) return;
  const body = readPatchBody();
  $("#patch-apply").disabled = true;
  $("#patch-result").textContent = "applying…";
  $("#patch-result").classList.remove("err");
  try {
    const r = await fetch(`/api/owner/${encodeURIComponent(owner.namespace)}/${encodeURIComponent(owner.kind)}/${encodeURIComponent(owner.name)}/patch`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ...body, dryRun: false }),
    });
    const j = await r.json();
    if (!r.ok) throw new Error(j.error || `${r.status}`);
    $("#patch-result").textContent = j.output || "applied";
    setMsg(`patched ${owner.kind}/${owner.name}`);
    setTimeout(refresh, 2000);
  } catch (e) {
    $("#patch-result").textContent = "error: " + e.message;
    $("#patch-result").classList.add("err");
  } finally {
    $("#patch-apply").disabled = false;
  }
}

function confirmSync(msg) { return window.confirm(msg); }
function closePatchModal() { $("#patch-modal").classList.add("hidden"); patchCtx = null; }

async function loadCosts() {
  if (state.costsLoading) return;
  state.costsLoading = true;
  $("#costs-meta").textContent = "loading…";
  $("#cost-services-meta").textContent = "loading…";
  // Daily total and per-service breakdown are independent CE queries; load both
  // but let each fail on its own so one error doesn't blank the other.
  const daily = jget("/api/costs")
    .then(d => { state.costs = d; renderCosts(); })
    .catch(e => { $("#cost-chart").innerHTML = `<div class="err">${e.message}</div>`; $("#costs-meta").textContent = "error"; });
  try { await Promise.all([daily, loadCostServices()]); }
  finally { state.costsLoading = false; }
}

// AWS Cost Explorer reports full service names; shorten the common ones so the
// list scans quickly. Unknowns just drop the "Amazon "/"AWS " prefix.
const SERVICE_SHORT = {
  "Amazon Elastic Compute Cloud - Compute": "EC2",
  "EC2 - Other": "EC2 · Other",
  "Amazon Elastic Kubernetes Service": "EKS",
  "Amazon Elastic Container Service for Kubernetes": "EKS",
  "Amazon Elastic Container Service": "ECS",
  "Amazon Elastic Container Registry (ECR)": "ECR",
  "Amazon EC2 Container Registry (ECR)": "ECR",
  "Amazon Elastic Block Store": "EBS",
  "Amazon Simple Storage Service": "S3",
  "Amazon DynamoDB": "DynamoDB",
  "Amazon Relational Database Service": "RDS",
  "Amazon Virtual Private Cloud": "VPC",
  "Amazon Elastic Load Balancing": "ELB",
  "Amazon CloudWatch": "CloudWatch",
  "AmazonCloudWatch": "CloudWatch",
  "AWS Key Management Service": "KMS",
  "AWS Lambda": "Lambda",
  "Amazon Route 53": "Route 53",
  "Amazon Simple Notification Service": "SNS",
  "Amazon Simple Queue Service": "SQS",
  "AWS Cost Explorer": "Cost Explorer",
  "AWS Secrets Manager": "Secrets Manager",
  "Amazon Simple Email Service": "SES",
  "AWS Data Transfer": "Data Transfer",
  "AWS Security Hub": "Security Hub",
  "Tax": "Tax",
};
function shortenService(name) {
  return SERVICE_SHORT[name] || name.replace(/^Amazon\s+/, "").replace(/^AWS\s+/, "");
}

// Fill the month dropdown with the current month and the previous few, newest
// first. Values are "YYYY-MM"; labels read like "Sep 2026".
function populateCostMonths() {
  const sel = $("#cost-month");
  if (!sel || sel.options.length) return;
  const now = new Date();
  for (let i = 0; i < 6; i++) {
    const d = new Date(now.getFullYear(), now.getMonth() - i, 1);
    const val = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}`;
    const o = document.createElement("option");
    o.value = val;
    o.textContent = d.toLocaleDateString(undefined, { month: "short", year: "numeric" }) + (i === 0 ? " (MTD)" : "");
    sel.appendChild(o);
  }
}

async function loadCostServices() {
  const month = $("#cost-month") ? $("#cost-month").value : "";
  $("#cost-services-meta").textContent = "loading…";
  try {
    state.costServices = await jget("/api/costs/services" + (month ? `?month=${month}` : ""));
    renderCostServices();
  } catch (e) {
    $("#cost-services").innerHTML = `<div class="err">${e.message}</div>`;
    $("#cost-services-meta").textContent = "error";
  }
}

function renderCostPlaceholder() {
  if (state.costs) return;
  $("#cost-chart").innerHTML = `
    <div class="placeholder">
      no cost data loaded
      <div class="hint">click <b>load costs</b> to query AWS Cost Explorer (≈ $0.01/call, requires <code>aws</code> CLI on PATH)</div>
    </div>`;
  $("#cost-services").innerHTML = `
    <div class="placeholder">per-service breakdown loads with costs</div>`;
}

// renderCostServices draws a ranked breakdown of spend by AWS service — where
// the money actually goes (EKS, EC2, EBS, DynamoDB, …) — as a share bar list.
function renderCostServices() {
  const el = $("#cost-services");
  const svc = state.costServices;
  if (!svc || !svc.length) {
    el.innerHTML = `<div class="placeholder">no per-service cost data for this period</div>`;
    $("#cost-services-meta").textContent = "";
    return;
  }
  const total = svc.reduce((s, x) => s + x.usd, 0);
  $("#cost-services-meta").textContent = `${svc.length} services · $${total.toFixed(2)} total`;

  // Curated, harmonious hues (cool → warm) rather than a raw rainbow — reads as
  // one palette instead of crayons. hue()/color() also feed the legend swatch
  // and the faint per-row share bar so a service is the same colour everywhere.
  const hues = [210, 190, 168, 145, 96, 52, 32, 14, 340, 315, 268, 232];
  const hue = i => hues[i % hues.length];
  const color = i => `hsl(${hue(i)} 52% 62%)`;
  const soft = i => `hsl(${hue(i)} 52% 62% / 0.16)`;

  // Segmented donut: thin ring on a faint track, with a small gap between arcs.
  const r = 82, sw = 18, C = 2 * Math.PI * r, gap = 3.5;
  let acc = 0;
  const segs = svc.map((x, i) => {
    const len = total > 0 ? (x.usd / total) * C : 0;
    const pct = total > 0 ? (x.usd / total) * 100 : 0;
    const draw = Math.max(0.01, len - gap);
    const seg = `<circle cx="100" cy="100" r="${r}" fill="none" stroke="${color(i)}" stroke-width="${sw}"
        stroke-dasharray="${draw.toFixed(2)} ${(C - draw).toFixed(2)}" stroke-dashoffset="${(-acc).toFixed(2)}">
        <title>${shortenService(x.service)}: $${x.usd.toFixed(2)} (${pct.toFixed(1)}%)</title></circle>`;
    acc += len;
    return seg;
  }).join("");

  const legend = svc.map((x, i) => {
    const pct = total > 0 ? (x.usd / total) * 100 : 0;
    const bar = `linear-gradient(90deg, ${soft(i)} 0, ${soft(i)} ${pct.toFixed(1)}%, transparent ${pct.toFixed(1)}%)`;
    return `<div class="svc-leg-row" style="background:${bar}">
      <i class="svc-sw" style="background:${color(i)}"></i>
      <span class="svc-leg-name" title="${x.service}">${shortenService(x.service)}</span>
      <span class="svc-leg-amt">$${x.usd.toFixed(2)}</span>
      <span class="svc-leg-pct">${pct.toFixed(1)}%</span>
    </div>`;
  }).join("");

  el.innerHTML = `
    <div class="svc-chart">
      <div class="svc-donut">
        <svg viewBox="0 0 200 200" role="img" aria-label="cost share by service">
          <circle class="svc-track" cx="100" cy="100" r="${r}" fill="none" stroke-width="${sw}" />
          <g transform="rotate(-90 100 100)">${segs}</g>
        </svg>
        <div class="svc-donut-center">
          <div class="svc-donut-total">$${Math.round(total).toLocaleString()}</div>
          <div class="svc-donut-sub">total</div>
        </div>
      </div>
      <div class="svc-legend">${legend}</div>
    </div>`;
}

function renderCosts() {
  const c = state.costs;
  if (!c || !c.length) { renderCostPlaceholder(); return; }
  const data = c.slice().sort((a, b) => a.date < b.date ? -1 : 1);

  const max = Math.max(...data.map(p => p.usd), 0.01);
  const total = data.reduce((s, p) => s + p.usd, 0);
  const avg = total / data.length;

  // Smart Y-axis cap: when a few outlier days (savings plan applications,
  // EDP charges, etc.) blow out the range, clip the chart to a sane ceiling
  // and mark the clipped bars so the day-to-day signal stays readable.
  const sortedUsd = data.map(p => p.usd).filter(v => v > 0).sort((a, b) => a - b);
  const p95 = sortedUsd.length ? sortedUsd[Math.floor(sortedUsd.length * 0.95)] || sortedUsd[sortedUsd.length - 1] : max;
  let yMax = max;
  if (max > p95 * 2 && max > avg * 4) {
    yMax = Math.max(p95 * 1.5, avg * 3);
  }

  // group by month for the summary cards
  const monthTotals = {};
  for (const p of data) {
    const m = p.date.slice(0, 7);
    monthTotals[m] = (monthTotals[m] || 0) + p.usd;
  }
  const months = Object.keys(monthTotals).sort();
  const now = new Date();
  const todayMonth = now.toISOString().slice(0, 7);
  const currentMonth = monthTotals[todayMonth] || 0;
  const prevMonths = months.filter(m => m < todayMonth);
  const lastMonth = prevMonths.length ? prevMonths[prevMonths.length - 1] : null;
  const lastMonthTotal = lastMonth ? monthTotals[lastMonth] : 0;

  // project current month spend linearly from elapsed UTC days
  const day = now.getUTCDate();
  const daysInMonth = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() + 1, 0)).getUTCDate();
  const projected = day > 0 ? (currentMonth / day) * daysInMonth : currentMonth;

  // delta vs last month, signed
  let delta = null;
  if (lastMonthTotal > 0) delta = ((projected - lastMonthTotal) / lastMonthTotal) * 100;

  // build forecast (ghost) entries for days between last real data point and
  // end of current month, so "today" actually shows up on the chart even when
  // AWS Cost Explorer lags by a day or two.
  const todayStr = now.toISOString().slice(0, 10);
  const endOfMonth = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() + 1, 0));
  const dailyRate = day > 0 ? currentMonth / day : avg;
  const lastDate = new Date(data[data.length - 1].date + "T00:00:00Z");
  const augmented = data.map(p => ({ ...p, forecast: false }));
  const cursor = new Date(lastDate);
  cursor.setUTCDate(cursor.getUTCDate() + 1);
  while (cursor <= endOfMonth) {
    augmented.push({
      date: cursor.toISOString().slice(0, 10),
      usd: dailyRate,
      forecast: true,
    });
    cursor.setUTCDate(cursor.getUTCDate() + 1);
  }

  // find today's index (after augmentation) so we can draw a "today" line
  let todayIdx = augmented.findIndex(p => p.date === todayStr);

  // bars — color by amount vs average, forecast bars use outline only
  let prevMonthKey = augmented[0].date.slice(0, 7);
  const bars = augmented.map(p => {
    const m = p.date.slice(0, 7);
    const sep = m !== prevMonthKey ? " month-sep" : "";
    prevMonthKey = m;
    const isToday = p.date === todayStr ? " today" : "";
    const fc = p.forecast ? " forecast" : "";
    const dataAttrs = `data-date="${p.date}" data-usd="${p.usd.toFixed(2)}" data-forecast="${p.forecast ? 1 : 0}"`;
    if (!p.forecast && p.usd <= 0) {
      return `<div class="cbar zero${sep}${isToday}" ${dataAttrs}></div>`;
    }
    let level = "";
    if (avg > 0) {
      if (p.usd >= avg * 1.5) level = " high";
      else if (p.usd > avg) level = " hi";
    }
    const clipped = !p.forecast && p.usd > yMax;
    let h = clipped ? 100 : (p.usd / yMax) * 100;
    if (h < 1.5) h = 1.5;
    const clipAttr = clipped ? ` clipped" data-real="${p.usd.toFixed(0)}` : "";
    return `<div class="cbar${level}${fc}${sep}${isToday}${clipAttr}" style="height:${h.toFixed(2)}%" ${dataAttrs}></div>`;
  }).join("");

  const avgY = (1 - avg / yMax) * 100;

  // today-line overlay (centered on the today bar within the chart area)
  let todayLine = "";
  if (todayIdx >= 0 && augmented.length > 0) {
    const left = ((todayIdx + 0.5) / augmented.length) * 100;
    todayLine = `<div class="today-line" style="left:${left.toFixed(2)}%"><span class="lbl">today</span></div>`;
  }

  // projected-month-end horizontal reference (only meaningful if projection > 0
  // and within the visible Y range of the chart)
  let projLine = "";
  if (projected > 0 && dailyRate > 0 && yMax > 0) {
    const y = (1 - dailyRate / yMax) * 100;
    if (y >= 0 && y <= 100) {
      projLine = `<div class="proj-line" style="top:${y.toFixed(2)}%"><span class="lbl">proj $${dailyRate.toFixed(2)}/day → $${projected.toFixed(0)} for ${todayMonth}</span></div>`;
    }
  }

  const fmt$ = n => `$${n.toFixed(2)}`;
  const deltaHTML = delta === null ? "" :
    `<span class="sub ${delta > 0 ? "up" : "down"}">${delta > 0 ? "▲" : "▼"} ${Math.abs(delta).toFixed(0)}% vs ${lastMonth}</span>`;

  $("#cost-chart").innerHTML = `
    <div class="cost-summary">
      <div class="item"><span class="k">total</span><span class="v">${fmt$(total)}</span></div>
      <div class="item"><span class="k">current month</span><span class="v">${fmt$(currentMonth)} <span class="sub">proj ${fmt$(projected)}</span></span></div>
      <div class="item"><span class="k">last month${lastMonth ? " (" + lastMonth + ")" : ""}</span><span class="v">${fmt$(lastMonthTotal)} ${deltaHTML}</span></div>
      <div class="item"><span class="k">avg / day</span><span class="v">${fmt$(avg)}</span></div>
      <div class="item"><span class="k">max day</span><span class="v">${fmt$(max)}</span></div>
    </div>
    <div class="cost-plot">
      <div class="y-axis">
        <div style="top: 0">${fmt$(yMax)}${yMax < max ? " <span class='dim'>+</span>" : ""}</div>
        <div style="top: 25%">${fmt$(yMax * 0.75)}</div>
        <div style="top: 50%">${fmt$(yMax * 0.5)}</div>
        <div style="top: 75%">${fmt$(yMax * 0.25)}</div>
        <div style="top: 100%; transform: translateY(-100%)">$0</div>
      </div>
      <div class="chart-area">
        <div class="grid-line" style="top: 0"></div>
        <div class="grid-line" style="top: 25%"></div>
        <div class="grid-line" style="top: 50%"></div>
        <div class="grid-line" style="top: 75%"></div>
        <div class="grid-line avg" style="top: ${avgY.toFixed(2)}%" title="avg ${fmt$(avg)}"></div>
        ${projLine}
        ${todayLine}
        <div class="bars">${bars}</div>
      </div>
    </div>
    <div class="x-axis">
      <span>${augmented[0].date}</span>
      <span class="center">solid = actual · dashed = projected · green ≤ avg · yellow > avg · red ≥ 1.5× avg${yMax < max ? " · ▲ = outlier clipped at $" + yMax.toFixed(0) : ""}</span>
      <span>${augmented[augmented.length - 1].date}</span>
    </div>
  `;
  $("#costs-meta").textContent = `${data.length} days · ${fmt$(total)} total`;

  attachCostTooltip(avg);
}

function attachCostTooltip(avg) {
  const plot = $("#cost-chart").querySelector(".chart-area");
  if (!plot) return;
  let tip = plot.querySelector(".cost-tip");
  if (!tip) {
    tip = document.createElement("div");
    tip.className = "cost-tip";
    plot.appendChild(tip);
  }
  const fmt$ = n => `$${n.toFixed(2)}`;
  const onMove = (e) => {
    const bar = e.target.closest(".cbar");
    if (!bar || !plot.contains(bar)) { tip.classList.remove("show"); return; }
    const date = bar.dataset.date;
    const usd = parseFloat(bar.dataset.usd);
    const forecast = bar.dataset.forecast === "1";
    const real = bar.dataset.real ? parseFloat(bar.dataset.real) : null;
    let delta = "";
    if (!forecast && avg > 0) {
      const d = ((usd - avg) / avg) * 100;
      const cls = d >= 0 ? "up" : "down";
      delta = `<span class="delta ${cls}">${d >= 0 ? "+" : ""}${d.toFixed(0)}% vs avg</span>`;
    }
    const tag = forecast ? `<span class="tag">projected</span>` :
                real !== null ? `<span class="tag">clipped · real ${fmt$(real)}</span>` : "";
    tip.innerHTML = `<span class="d">${date}</span><span class="v">${forecast ? "≈ " : ""}${fmt$(usd)}</span>${delta}${tag}`;

    const rect = plot.getBoundingClientRect();
    const tipW = tip.offsetWidth || 200;
    const tipH = tip.offsetHeight || 28;
    let x = e.clientX - rect.left + 12;
    let y = e.clientY - rect.top - tipH - 8;
    if (x + tipW > rect.width - 4) x = rect.width - tipW - 4;
    if (y < 4) y = e.clientY - rect.top + 16;
    tip.style.left = x + "px";
    tip.style.top = y + "px";
    tip.classList.add("show");
  };
  plot.onmousemove = onMove;
  plot.onmouseleave = () => tip.classList.remove("show");
}

// ---------- load test (JMeter) ----------
let ltBusy = false;
let ltLast = null;

async function runLoadTest() {
  if (ltBusy) return;
  const raw = $("#pb-pod").value;
  if (!raw) { $("#tf-status").textContent = "pick a Deployment pod first"; return; }
  const [namespace, pod] = raw.split("|");
  const port = parseInt($("#pb-port").value, 10);
  if (!port || port <= 0) { $("#tf-status").textContent = "enter the pod's container port"; $("#pb-port").focus(); return; }
  const method = $("#lt-method").value;
  const payload = {
    namespace, pod, port,
    scheme: $("#pb-scheme").value,
    path: ($("#pb-path").value || "/").trim(),
    method,
    threads: parseInt($("#lt-threads").value, 10) || 1,
    rampSeconds: parseInt($("#lt-ramp").value, 10) || 0,
    durationSeconds: parseInt($("#lt-duration").value, 10) || 1,
  };
  const rawBody = ($("#lt-body").value || "").trim();
  if (rawBody && ["POST", "PUT", "PATCH"].includes(method)) {
    payload.body = rawBody;
    payload.contentType = ($("#lt-content-type").value || "application/json").trim();
  }
  ltBusy = true;
  $("#lt-run").disabled = true;
  const dur = payload.durationSeconds;
  $("#tf-status").innerHTML = `<span class="lt-spin"></span> port-forwarding to ${pod} and running ${payload.threads} users for ${dur}s (${payload.method}) — watch the cluster metrics react above…`;
  const started = Date.now();
  const tick = setInterval(() => {
    const left = Math.max(0, dur - Math.round((Date.now() - started) / 1000));
    $("#tf-status").innerHTML = `<span class="lt-spin"></span> running… ~${left}s left (plus JMeter startup). Watch pods/CPU update above.`;
  }, 1000);
  try {
    const r = await fetch("/api/loadtest", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    const j = await r.json();
    if (!r.ok) throw new Error(j.error || `${r.status}`);
    ltLast = j;
    renderLoadResult(j);
    $("#tf-status").textContent = `done · ${j.samples} requests in ${j.durationS}s`;
  } catch (e) {
    $("#lt-results").innerHTML = `<div class="lt-err">${e.message}</div>`;
    $("#tf-status").textContent = "failed";
  } finally {
    clearInterval(tick);
    ltBusy = false;
    $("#lt-run").disabled = false;
  }
}

function renderLoadResult(j) {
  const stat = (label, val, cls, sub) =>
    `<div class="lt-stat"><div class="lt-stat-v ${cls || ""}">${val}</div><div class="lt-stat-l">${label}</div>${sub ? `<div class="lt-stat-sub">${sub}</div>` : ""}</div>`;
  const errCls = j.errorPct > 5 ? "hot" : j.errorPct > 0 ? "warn" : "ok";
  const latCls = ms => ms < 200 ? "ok" : ms < 500 ? "warn" : "hot";

  const overviewCards = [
    stat("requests", (j.samples || 0).toLocaleString()),
    stat("throughput", `${j.throughput}<span class="u">/s</span>`, "", `peak ${j.peakRps || 0}/s`),
    stat("duration", `${j.durationS}<span class="u">s</span>`),
    stat("errors", (j.errors || 0).toLocaleString(), j.errors > 0 ? errCls : "ok"),
    stat("error rate", `${j.errorPct}<span class="u">%</span>`, errCls),
  ].join("");

  const latencyCards = [
    stat("min", `${j.minMs}<span class="u">ms</span>`, latCls(j.minMs)),
    stat("median p50", `${j.p50Ms}<span class="u">ms</span>`, latCls(j.p50Ms)),
    stat("avg", `${Math.round(j.avgMs)}<span class="u">ms</span>`, latCls(j.avgMs)),
    stat("std dev", `${Math.round(j.stdDev || 0)}<span class="u">ms</span>`),
    stat("p90", `${j.p90Ms}<span class="u">ms</span>`, latCls(j.p90Ms)),
    stat("p95", `${j.p95Ms}<span class="u">ms</span>`, latCls(j.p95Ms)),
    stat("p99", `${j.p99Ms}<span class="u">ms</span>`, latCls(j.p99Ms)),
    stat("max", `${j.maxMs}<span class="u">ms</span>`, latCls(j.maxMs)),
  ].join("");

  $("#lt-results").innerHTML = `
    <div class="lt-target">${j.target} &middot; <b>${j.threads}</b> concurrent users</div>
    <div class="lt-section-hdr">Overview</div>
    <div class="lt-stats lt-stats-5">${overviewCards}</div>
    <div class="lt-section-hdr">Latency breakdown</div>
    <div class="lt-stats lt-stats-8">${latencyCards}</div>
    <div class="lt-charts-row">
      <div class="lt-chart-card">
        <div class="lt-chart-head">
          <span>Throughput over time</span>
          <span class="lt-legend">
            <span class="lg"><i class="sw ok"></i>ok</span>
            <span class="lg"><i class="sw err"></i>errors</span>
          </span>
        </div>
        <div class="lt-chart" id="lt-chart-tp"></div>
      </div>
      <div class="lt-chart-card">
        <div class="lt-chart-head">
          <span>Latency over time</span>
          <span class="lt-legend">
            <span class="lg"><i class="sw lat"></i>avg ms</span>
            <span class="lg" style="font-size:10px;color:var(--dim)">-- p90 &nbsp; <span style="color:var(--red);opacity:.7">-- p99</span></span>
          </span>
        </div>
        <div class="lt-chart" id="lt-chart-lat"></div>
      </div>
    </div>
    <div class="lt-charts-row" style="margin-top:10px">
      <div class="lt-chart-card">
        <div class="lt-chart-head"><span>Response time distribution</span></div>
        <div class="lt-chart" id="lt-chart-hist"></div>
      </div>
      <div class="lt-chart-card">
        <div class="lt-chart-head"><span>Latency percentiles</span></div>
        <div class="lt-chart" id="lt-chart-pct"></div>
      </div>
    </div>`;

  const series = j.series || [];
  drawThroughputChart($("#lt-chart-tp"), series);
  drawLatencyTimeline($("#lt-chart-lat"), series, j);
  drawHistogram($("#lt-chart-hist"), j.histogram || []);
  drawPercentileChart($("#lt-chart-pct"), j);
}

// ---------- connection timing (probe) ----------
let pbBusy = false;

let tfMode = "latency";

// Application pods eligible for the traffic view: Deployment-managed (a
// Deployment's pods are controlled by a ReplicaSet named <deployment>-<hash>),
// excluding platform / observability namespaces. This drops DaemonSets,
// StatefulSets, Jobs/CronJobs, bare pods, and system workloads.
function appDeploymentPods() {
  return (state.pods || []).filter(p => p.ownerKind === "ReplicaSet" && !isInfraNamespace(p.namespace));
}

// Populate namespace → pod → endpoint. Called whenever the traffic view opens.
function populateTraffic() {
  populateTrafficNamespaces();
}

function populateTrafficNamespaces() {
  const sel = $("#pb-ns");
  if (!sel) return;
  const prev = sel.value;
  const namespaces = [...new Set(appDeploymentPods().map(p => p.namespace))].sort((a, b) => a.localeCompare(b));
  if (!namespaces.length) {
    sel.innerHTML = `<option value="">— no application namespaces —</option>`;
    populateTrafficPods();
    return;
  }
  sel.innerHTML = namespaces.map(n => `<option value="${n}">${n}</option>`).join("");
  const selectedNs = state.selected >= 0 && state.filtered[state.selected] ? state.filtered[state.selected].namespace : "";
  if (prev && namespaces.includes(prev)) sel.value = prev;
  else if (namespaces.includes(selectedNs)) sel.value = selectedNs;
  populateTrafficPods();
}

function populateTrafficPods() {
  const sel = $("#pb-pod");
  if (!sel) return;
  const ns = $("#pb-ns").value;
  const prev = sel.value;
  const pods = appDeploymentPods()
    .filter(p => p.namespace === ns)
    .sort((a, b) => a.name.localeCompare(b.name));
  if (!pods.length) {
    sel.innerHTML = `<option value="">— no Deployment pods —</option>`;
    loadProbeEndpoints();
    return;
  }
  sel.innerHTML = pods.map(p => `<option value="${p.namespace}|${p.name}">${p.name}</option>`).join("");
  if (prev && pods.some(p => `${p.namespace}|${p.name}` === prev)) sel.value = prev;
  loadProbeEndpoints();
}

// Switch between the latency probe and the load test. Both share the same
// namespace → pod → endpoint target; only the params and results differ.
function setTrafficMode(m) {
  tfMode = m === "load" ? "load" : "latency";
  const isLoad = tfMode === "load";
  $$(".tf-mode .btn").forEach(b => {
    const on = b.dataset.mode === tfMode;
    b.classList.toggle("active", on);
    b.setAttribute("aria-selected", String(on));
  });
  $(".tf-latency").hidden = isLoad;
  $(".tf-load").hidden = !isLoad;
  $("#pb-results").hidden = isLoad;
  $("#lt-results").hidden = !isLoad;
  $("#tf-status").textContent = "";
  updateTfTargetHint();
}

// One-line echo of what will actually be hit, so both modes make the
// port-forward target explicit.
function updateTfTargetHint() {
  const hint = $("#tf-target-hint");
  if (!hint) return;
  const raw = $("#pb-pod").value || "";
  if (!raw) { hint.textContent = ""; return; }
  const pod = raw.split("|")[1] || "";
  const scheme = $("#pb-scheme").value;
  const port = $("#pb-port").value || "?";
  const path = ($("#pb-path").value || "/").trim() || "/";
  hint.textContent = `port-forward → ${scheme}://${pod}:${port}${path}`;
}

// Ask the backend which endpoints the selected pod plausibly serves (from its
// probes, ports, prometheus annotations, and Services) and fill the dropdown.
// Picking one pre-fills scheme/port/path; the fields stay editable.
async function loadProbeEndpoints() {
  const sel = $("#pb-endpoint");
  if (!sel) return;
  const raw = $("#pb-pod").value;
  if (!raw) { sel.innerHTML = ""; return; }
  const [ns, pod] = raw.split("|");
  sel.innerHTML = `<option value="">discovering…</option>`;
  try {
    const j = await jget(`/api/pod/${encodeURIComponent(ns)}/${encodeURIComponent(pod)}/endpoints`);
    const eps = j.endpoints || [];
    if (!eps.length) {
      sel.innerHTML = `<option value="">— none found, enter port/path manually —</option>`;
      return;
    }
    sel.innerHTML = eps.map((e, i) =>
      `<option value="${i}" data-scheme="${e.scheme}" data-port="${e.port}" data-path="${e.path}">${e.scheme}://:${e.port}${e.path} · ${e.label} (${e.source})</option>`).join("");
    applyProbeEndpoint();
  } catch (e) {
    sel.innerHTML = `<option value="">discovery failed — enter manually</option>`;
  }
}

function applyProbeEndpoint() {
  const sel = $("#pb-endpoint");
  const opt = sel.options[sel.selectedIndex];
  if (!opt || !opt.dataset || !opt.dataset.port) return;
  $("#pb-scheme").value = opt.dataset.scheme || "http";
  $("#pb-port").value = opt.dataset.port;
  $("#pb-path").value = opt.dataset.path || "/";
  updateTfTargetHint();
}

async function runProbe() {
  if (pbBusy) return;
  const raw = $("#pb-pod").value;
  if (!raw) { $("#tf-status").textContent = "pick a Deployment pod first"; return; }
  const [namespace, pod] = raw.split("|");
  const port = parseInt($("#pb-port").value, 10);
  if (!port || port <= 0) { $("#tf-status").textContent = "enter the pod's container port"; $("#pb-port").focus(); return; }
  const payload = {
    namespace, pod, port,
    scheme: $("#pb-scheme").value,
    path: ($("#pb-path").value || "/").trim(),
    samples: parseInt($("#pb-samples").value, 10) || 5,
  };
  pbBusy = true;
  $("#pb-run").disabled = true;
  $("#tf-status").innerHTML = `<span class="lt-spin"></span> port-forwarding to ${pod} and timing ${payload.samples} request(s)…`;
  try {
    const r = await fetch("/api/probe", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    const j = await r.json();
    if (!r.ok) throw new Error(j.error || `${r.status}`);
    renderProbeResult(j);
    $("#tf-status").textContent = `done · ${j.okCount}/${j.count} ok${j.proto ? " · " + j.proto : ""}`;
  } catch (e) {
    $("#pb-results").innerHTML = `<div class="lt-err">${e.message}</div>`;
    $("#tf-status").textContent = "failed";
  } finally {
    pbBusy = false;
    $("#pb-run").disabled = false;
  }
}

function renderProbeResult(j) {
  const latCls = ms => ms < 50 ? "ok" : ms < 200 ? "warn" : "hot";
  const stat = (label, s, cls, note) =>
    `<div class="lt-stat"><div class="lt-stat-v ${cls || ""}">${s.avg}<span class="u">ms</span></div><div class="lt-stat-l">${label}</div><div class="lt-stat-sub">${note || `${s.min}–${s.max} ms`}</div></div>`;

  const cards = [
    stat("DNS", j.dns, latCls(j.dns.avg), "local · port-forward"),
    stat("TCP connect", j.connect, latCls(j.connect.avg), "local · port-forward"),
    stat("TLS handshake", j.tls, latCls(j.tls.avg), "local · port-forward"),
    stat("server wait (TTFB)", j.wait, latCls(j.wait.avg)),
    stat("transfer", j.transfer, latCls(j.transfer.avg)),
    stat("total", j.total, latCls(j.total.avg)),
  ].join("");

  const rows = (j.samples || []).map((s, i) => s.err
    ? `<tr class="pb-fail"><td>${i + 1}</td><td colspan="7" class="pb-errcell">${s.err}</td></tr>`
    : `<tr><td>${i + 1}</td><td>${s.dnsMs}</td><td>${s.connectMs}</td><td>${s.tlsMs}</td><td>${s.waitMs}</td><td>${s.transferMs}</td><td><b>${s.totalMs}</b></td><td class="pb-code s${Math.floor(s.status / 100)}">${s.status}</td></tr>`).join("");

  $("#pb-results").innerHTML = `
    <div class="lt-target">GET ${j.target}${j.proto ? ` &middot; ${j.proto}` : ""} &middot; <b>${j.okCount}/${j.count}</b> ok</div>
    <div class="lt-section-hdr">Latency breakdown (avg)</div>
    <div class="lt-stats lt-stats-6">${cards}</div>
    <div class="lt-section-hdr">Per-sample (ms)</div>
    <table class="pb-table">
      <thead><tr><th>#</th><th>DNS</th><th>TCP</th><th>TLS</th><th>wait</th><th>transfer</th><th>total</th><th>code</th></tr></thead>
      <tbody>${rows}</tbody>
    </table>`;
}

// drawThroughputChart — vertical bar chart of req/s (ok=accent, errors=red).
function drawThroughputChart(host, s) {
  if (!s.length) { host.innerHTML = '<div style="padding:20px;font-size:12px;color:var(--muted)">no timeline</div>'; return; }
  const W = Math.max(360, host.clientWidth || 500);
  const H = 190;
  const m = { t: 20, r: 16, b: 26, l: 44 };
  const iw = W - m.l - m.r, ih = H - m.t - m.b;
  const n = s.length;
  const maxCount = Math.max(1, ...s.map(b => b.count));
  const band = iw / n;
  const bw = Math.min(band * 0.75, 28);
  const yBot = m.t + ih;
  const parts = [];
  for (let k = 0; k <= 4; k++) {
    const y = m.t + (ih * k) / 4;
    parts.push(`<line x1="${m.l}" y1="${y.toFixed(1)}" x2="${m.l + iw}" y2="${y.toFixed(1)}" stroke="var(--border)" stroke-width="1"/>`);
    parts.push(`<text x="${m.l - 6}" y="${(y + 3.5).toFixed(1)}" text-anchor="end" class="lt-axis">${Math.round(maxCount * (1 - k / 4))}</text>`);
  }
  s.forEach((b, i) => {
    const x = m.l + band * i + (band - bw) / 2;
    const okC = Math.max(0, b.count - b.errors);
    const okH = (okC / maxCount) * ih;
    const erH = (b.errors / maxCount) * ih;
    const tip = `t=${b.t}s · ${b.count} req${b.errors ? ` · ${b.errors} err` : ""}`;
    if (okH > 0) parts.push(`<rect x="${x.toFixed(1)}" y="${(yBot - okH).toFixed(1)}" width="${bw.toFixed(1)}" height="${okH.toFixed(1)}" fill="var(--accent)" opacity="0.85" rx="1"><title>${tip}</title></rect>`);
    if (erH > 0) parts.push(`<rect x="${x.toFixed(1)}" y="${(yBot - okH - erH).toFixed(1)}" width="${bw.toFixed(1)}" height="${erH.toFixed(1)}" fill="var(--red)" rx="1"><title>${tip}</title></rect>`);
  });
  const ticks = n <= 1 ? [0] : [...new Set([0, Math.floor((n - 1) / 2), n - 1])];
  ticks.forEach(i => {
    const cx = (m.l + band * i + band / 2).toFixed(1);
    parts.push(`<text x="${cx}" y="${H - 6}" text-anchor="middle" class="lt-axis">${s[i].t}s</text>`);
  });
  parts.push(`<text x="${m.l - 6}" y="12" text-anchor="end" class="lt-axis dimlbl">req/s</text>`);
  host.innerHTML = `<svg width="${W}" height="${H}" viewBox="0 0 ${W} ${H}">${parts.join("")}</svg>`;
}

// drawLatencyTimeline — line chart of per-second avg latency with p90/p99 reference lines.
function drawLatencyTimeline(host, s, j) {
  if (!s.length) { host.innerHTML = '<div style="padding:20px;font-size:12px;color:var(--muted)">no timeline</div>'; return; }
  const W = Math.max(360, host.clientWidth || 500);
  const H = 190;
  const m = { t: 20, r: 46, b: 26, l: 44 };
  const iw = W - m.l - m.r, ih = H - m.t - m.b;
  const n = s.length;
  const maxLat = Math.max(1, j.maxMs || 0, ...s.map(b => b.avgMs));
  const band = iw / n;
  const yBot = m.t + ih;
  const parts = [];
  for (let k = 0; k <= 4; k++) {
    const y = m.t + (ih * k) / 4;
    parts.push(`<line x1="${m.l}" y1="${y.toFixed(1)}" x2="${m.l + iw}" y2="${y.toFixed(1)}" stroke="var(--border)" stroke-width="1"/>`);
    parts.push(`<text x="${m.l - 6}" y="${(y + 3.5).toFixed(1)}" text-anchor="end" class="lt-axis">${Math.round(maxLat * (1 - k / 4))}</text>`);
  }
  // p99 dashed reference line
  const p99Y = (m.t + ih - (j.p99Ms / maxLat) * ih).toFixed(1);
  parts.push(`<line x1="${m.l}" y1="${p99Y}" x2="${m.l + iw}" y2="${p99Y}" stroke="var(--red)" stroke-width="1" stroke-dasharray="4,3" opacity="0.55"/>`);
  parts.push(`<text x="${m.l + iw + 4}" y="${(+p99Y + 3.5).toFixed(1)}" class="lt-axis" style="fill:var(--red);opacity:.7;font-size:9px">p99</text>`);
  // p90 dashed reference line
  const p90Y = (m.t + ih - (j.p90Ms / maxLat) * ih).toFixed(1);
  parts.push(`<line x1="${m.l}" y1="${p90Y}" x2="${m.l + iw}" y2="${p90Y}" stroke="var(--dim)" stroke-width="1" stroke-dasharray="4,3" opacity="0.45"/>`);
  parts.push(`<text x="${m.l + iw + 4}" y="${(+p90Y + 3.5).toFixed(1)}" class="lt-axis" style="font-size:9px">p90</text>`);
  // filled area + line
  const pts = s.map((b, i) => [(m.l + band * i + band / 2), (m.t + ih - (b.avgMs / maxLat) * ih)]);
  const area = `M${pts[0][0].toFixed(1)},${yBot} ` + pts.map(p => `L${p[0].toFixed(1)},${p[1].toFixed(1)}`).join(" ") + ` L${pts[pts.length - 1][0].toFixed(1)},${yBot} Z`;
  parts.push(`<path d="${area}" fill="var(--yellow)" opacity="0.07"/>`);
  parts.push(`<polyline points="${pts.map(p => `${p[0].toFixed(1)},${p[1].toFixed(1)}`).join(" ")}" fill="none" stroke="var(--yellow)" stroke-width="2"/>`);
  pts.forEach((p, i) => parts.push(`<circle cx="${p[0].toFixed(1)}" cy="${p[1].toFixed(1)}" r="2.5" fill="var(--yellow)"><title>t=${s[i].t}s: ${s[i].avgMs}ms avg</title></circle>`));
  // x-axis ticks
  const ticks = n <= 1 ? [0] : [...new Set([0, Math.floor((n - 1) / 2), n - 1])];
  ticks.forEach(i => {
    const cx = (m.l + band * i + band / 2).toFixed(1);
    parts.push(`<text x="${cx}" y="${H - 6}" text-anchor="middle" class="lt-axis">${s[i].t}s</text>`);
  });
  parts.push(`<text x="${m.l - 6}" y="12" text-anchor="end" class="lt-axis dimlbl">ms</text>`);
  host.innerHTML = `<svg width="${W}" height="${H}" viewBox="0 0 ${W} ${H}">${parts.join("")}</svg>`;
}

// drawHistogram — vertical bars for each response-time bucket.
function drawHistogram(host, hist) {
  if (!hist || !hist.length) { host.innerHTML = '<div style="padding:20px;font-size:12px;color:var(--muted)">no data</div>'; return; }
  const W = Math.max(280, host.clientWidth || 400);
  const H = 190;
  const m = { t: 20, r: 16, b: 44, l: 40 };
  const iw = W - m.l - m.r, ih = H - m.t - m.b;
  const n = hist.length;
  const maxPct = Math.max(1, ...hist.map(b => b.pct));
  const band = iw / n;
  const bw = band * 0.68;
  const yBot = m.t + ih;
  const parts = [];
  for (let k = 0; k <= 4; k++) {
    const y = m.t + (ih * k) / 4;
    parts.push(`<line x1="${m.l}" y1="${y.toFixed(1)}" x2="${m.l + iw}" y2="${y.toFixed(1)}" stroke="var(--border)" stroke-width="1"/>`);
    if (k < 4) parts.push(`<text x="${m.l - 4}" y="${(y + 3.5).toFixed(1)}" text-anchor="end" class="lt-axis">${Math.round(maxPct * (1 - k / 4))}%</text>`);
  }
  hist.forEach((b, i) => {
    const x = m.l + band * i + (band - bw) / 2;
    const bH = (b.pct / maxPct) * ih;
    const colors = ["var(--green)", "var(--accent)", "var(--yellow)", "var(--yellow)", "var(--red)", "var(--red)"];
    const color = colors[i] || "var(--muted)";
    if (bH > 0.5) {
      parts.push(`<rect x="${x.toFixed(1)}" y="${(yBot - bH).toFixed(1)}" width="${bw.toFixed(1)}" height="${bH.toFixed(1)}" fill="${color}" opacity="0.8" rx="2"><title>${b.label}: ${b.count} (${b.pct}%)</title></rect>`);
      parts.push(`<text x="${(x + bw / 2).toFixed(1)}" y="${(yBot - bH - 4).toFixed(1)}" text-anchor="middle" class="lt-axis" style="font-size:9px">${b.pct}%</text>`);
    }
    // x-axis label (two lines: wrap on dash)
    const lbl = b.label.replace("–", "\n");
    const lines = lbl.split("\n");
    lines.forEach((ln, li) => {
      parts.push(`<text x="${(x + bw / 2).toFixed(1)}" y="${(H - 28 + li * 12).toFixed(1)}" text-anchor="middle" class="lt-axis" style="font-size:9px">${ln}</text>`);
    });
  });
  host.innerHTML = `<svg width="${W}" height="${H}" viewBox="0 0 ${W} ${H}">${parts.join("")}</svg>`;
}

// drawPercentileChart — horizontal waterfall bars for p50/p90/p95/p99/max.
function drawPercentileChart(host, j) {
  const rows = [
    { label: "P50", val: j.p50Ms, color: "var(--green)" },
    { label: "P90", val: j.p90Ms, color: "var(--accent)" },
    { label: "P95", val: j.p95Ms, color: "var(--yellow)" },
    { label: "P99", val: j.p99Ms, color: j.p99Ms > 500 ? "var(--red)" : "var(--yellow)" },
    { label: "Max", val: j.maxMs, color: j.maxMs > 1000 ? "var(--red)" : "var(--yellow)" },
  ];
  const W = Math.max(280, host.clientWidth || 400);
  const rowH = 36;
  const H = rows.length * rowH + 24;
  const m = { t: 12, r: 70, b: 12, l: 42 };
  const iw = W - m.l - m.r;
  const maxVal = Math.max(1, j.maxMs || 0);
  const parts = [];
  rows.forEach((r, i) => {
    const y = m.t + i * rowH;
    const barH = 18;
    const barY = y + (rowH - barH) / 2;
    const barW = Math.max(4, (r.val / maxVal) * iw);
    parts.push(`<text x="${m.l - 6}" y="${(barY + barH / 2 + 4).toFixed(1)}" text-anchor="end" class="lt-axis" style="font-size:11px;font-weight:600">${r.label}</text>`);
    parts.push(`<rect x="${m.l}" y="${barY.toFixed(1)}" width="${iw}" height="${barH}" fill="var(--border)" rx="4"/>`);
    parts.push(`<rect x="${m.l}" y="${barY.toFixed(1)}" width="${barW.toFixed(1)}" height="${barH}" fill="${r.color}" opacity="0.82" rx="4"><title>${r.label}: ${r.val}ms</title></rect>`);
    parts.push(`<text x="${(m.l + barW + 8).toFixed(1)}" y="${(barY + barH / 2 + 4).toFixed(1)}" class="lt-axis" style="font-size:11px;fill:${r.color};font-weight:600">${r.val}ms</text>`);
  });
  host.innerHTML = `<svg width="${W}" height="${H}" viewBox="0 0 ${W} ${H}">${parts.join("")}</svg>`;
}

// ---------- gemini ----------
// renderTurnText parses model responses for sentiment-marked bullets and
// returns a DOM fragment so the chat view can color each line. The model is
// instructed to start every bullet with [+] (good/green), [!] (bad/red), or
// [-] (neutral). User turns are rendered as-is.
function renderTurnText(role, raw) {
  const frag = document.createDocumentFragment();
  const lines = (raw || "").split(/\r?\n/);
  // User text or any prose without markers — keep one block, default color.
  if (role !== "model" || !lines.some(l => /^\s*\[[+!\-]\]/.test(l))) {
    const span = document.createElement("span");
    span.textContent = raw;
    frag.appendChild(span);
    return frag;
  }
  for (const line of lines) {
    if (line.trim() === "") continue;
    const m = line.match(/^\s*\[([+!\-])\]\s?(.*)$/);
    const div = document.createElement("div");
    if (m) {
      const sigil = m[1];
      div.className =
        sigil === "+" ? "bullet bullet-good" :
        sigil === "!" ? "bullet bullet-bad" :
                        "bullet bullet-info";
      div.textContent = "• " + m[2];
    } else {
      // Stray non-bullet line from the model — render neutral, no marker.
      div.className = "bullet bullet-info";
      div.textContent = line;
    }
    frag.appendChild(div);
  }
  return frag;
}

function renderGemini() {
  const wrap = $("#gemini-history");
  wrap.innerHTML = "";
  state.gemini.history.forEach(t => {
    const div = document.createElement("div");
    div.className = "turn";
    div.innerHTML = `<span class="role ${t.role}">${t.role}</span><span class="text"></span>`;
    div.querySelector(".text").appendChild(renderTurnText(t.role, t.text));
    wrap.appendChild(div);
  });
  if (state.gemini.busy) {
    const d = document.createElement("div");
    d.className = "turn busy";
    d.textContent = "thinking…";
    wrap.appendChild(d);
  }
  wrap.scrollTop = wrap.scrollHeight;

  const ctx = state.gemini.pod
    ? `context: cluster + pod ${state.gemini.pod.namespace}/${state.gemini.pod.name}`
    : "context: cluster";
  $("#gemini-ctx").textContent = ctx;
  const provSel = $("#gemini-provider");
  if (provSel && provSel.value !== state.gemini.provider) provSel.value = state.gemini.provider;
  syncApiKeyField();
}

// API keys are stored client-side (localStorage), one per provider, and sent
// with each chat request. They never persist on the server.
function apiKeyStoreKey(provider) { return "clusterheart:apiKey:" + provider; }
function loadApiKey(provider) {
  try { return localStorage.getItem(apiKeyStoreKey(provider)) || ""; } catch (_) { return ""; }
}
function saveApiKey(provider, val) {
  try {
    if (val) localStorage.setItem(apiKeyStoreKey(provider), val);
    else localStorage.removeItem(apiKeyStoreKey(provider));
  } catch (_) {}
}
// Reflect the saved key for the active provider into the input + status label.
function syncApiKeyField() {
  const input = $("#gemini-key");
  const status = $("#gemini-key-status");
  if (!input) return;
  const saved = loadApiKey(state.gemini.provider);
  input.value = saved;
  input.placeholder = `${state.gemini.provider} key — stored locally`;
  if (status) status.textContent = saved ? "saved ✓" : "not set";
  const mt = $("#gemini-maxtokens");
  if (mt) { try { mt.value = localStorage.getItem("clusterheart:maxTokens") || ""; } catch (_) {} }
  const md = $("#gemini-model");
  if (md) { try { md.value = localStorage.getItem("clusterheart:orModel") || ""; } catch (_) {} }
}
// Optional reply-length cap (OpenRouter). Empty = server default (1500).
function loadMaxTokens() {
  const mt = $("#gemini-maxtokens");
  const n = mt ? parseInt(mt.value, 10) : NaN;
  return Number.isFinite(n) && n > 0 ? n : 0;
}
// Optional OpenRouter model id. Empty = server's free default.
function loadModel() {
  const md = $("#gemini-model");
  return md ? md.value.trim() : "";
}

async function sendGemini() {
  const text = $("#gemini-input").value.trim();
  if (!text || state.gemini.busy) return;
  state.gemini.history.push({ role: "user", text });
  state.gemini.busy = true;
  state.gemini.err = "";
  $("#gemini-err").textContent = "";
  $("#gemini-input").value = "";
  renderGemini();

  try {
    const r = await fetch("/api/gemini", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        provider: state.gemini.provider,
        apiKey: loadApiKey(state.gemini.provider),
        model: loadModel(),
        maxTokens: loadMaxTokens(),
        namespace: state.gemini.pod ? state.gemini.pod.namespace : "",
        pod: state.gemini.pod ? state.gemini.pod.name : "",
        history: state.gemini.history,
      }),
    });
    const j = await r.json();
    if (!r.ok) throw new Error(j.error || `${r.status}`);
    state.gemini.history.push({ role: "model", text: j.text });
  } catch (e) {
    $("#gemini-err").textContent = e.message;
  } finally {
    state.gemini.busy = false;
    renderGemini();
  }
}

// ---------- modal ----------
function openModal(title, body) {
  $("#modal-title").textContent = title;
  $("#modal-body").textContent = body;
  $("#modal").classList.remove("hidden");
}
function closeModal() { $("#modal").classList.add("hidden"); }

function confirm(text, onYes) {
  $("#confirm-text").textContent = text;
  $("#confirm").classList.remove("hidden");
  const yes = $("#confirm-yes"), no = $("#confirm-no");
  const cleanup = () => {
    $("#confirm").classList.add("hidden");
    yes.onclick = null; no.onclick = null;
  };
  yes.onclick = () => { cleanup(); onYes(); };
  no.onclick = cleanup;
}

// ---------- pod actions ----------
function selectedPod() {
  if (state.selected < 0) return null;
  return state.filtered[state.selected];
}
function openLogs(p) {
  if (!p) return;
  pickContainer(p, (container) => openLogsWith(p, container));
}
async function openLogsWith(p, container) {
  const label = container ? `${p.name}/${container}` : p.name;
  setMsg(`fetching logs for ${label}…`);
  try {
    const q = new URLSearchParams({ tail: "200" });
    if (container) q.set("container", container);
    const t = await jget(`/api/pod/${encodeURIComponent(p.namespace)}/${encodeURIComponent(p.name)}/logs?${q}`);
    const title = container ? `logs · ${p.namespace}/${p.name} · ${container}` : `logs · ${p.namespace}/${p.name}`;
    openModal(title, t || "(empty)");
    setMsg(`logs · ${label}`);
  } catch (e) { setMsg(`logs error: ${e.message}`); }
}
async function openYaml(p) {
  if (!p) return;
  setMsg(`fetching yaml for ${p.name}…`);
  try {
    const t = await jget(`/api/pod/${encodeURIComponent(p.namespace)}/${encodeURIComponent(p.name)}/yaml`);
    openModal(`yaml · ${p.namespace}/${p.name}`, t || "(empty)");
    setMsg(`yaml · ${p.name}`);
  } catch (e) { setMsg(`yaml error: ${e.message}`); }
}
async function openDiagnose(p) {
  if (!p) return;
  setMsg(`diagnosing ${p.name}…`);
  try {
    const t = await jget(`/api/pod/${encodeURIComponent(p.namespace)}/${encodeURIComponent(p.name)}/diagnose`);
    openModal(`diagnose · ${p.namespace}/${p.name}`, t);
    setMsg(`diagnose · ${p.name}`);
  } catch (e) { setMsg(`diagnose error: ${e.message}`); }
}
async function deletePod(p) {
  if (!p) return;
  confirm(`Delete pod ${p.namespace}/${p.name}?`, async () => {
    try {
      const r = await fetch(`/api/pod/${encodeURIComponent(p.namespace)}/${encodeURIComponent(p.name)}`, { method: "DELETE" });
      const j = await r.json();
      if (!r.ok) throw new Error(j.error);
      setMsg(`deleted ${p.name}`);
      refresh();
    } catch (e) { setMsg(`delete error: ${e.message}`); }
  });
}
// ---------- container picker ----------
function pickContainer(p, onPick) {
  if (!p) return;
  if (!p.containers || p.containers.length <= 1) {
    onPick(p.containers && p.containers[0]);
    return;
  }
  const m = $("#container-modal");
  const list = $("#container-list");
  list.innerHTML = "";
  p.containers.forEach(c => {
    const el = document.createElement("div");
    el.className = "ci";
    el.textContent = c;
    el.addEventListener("click", () => { m.classList.add("hidden"); onPick(c); });
    list.appendChild(el);
  });
  $("#container-title").textContent = `select container · ${p.namespace}/${p.name}`;
  m.classList.remove("hidden");
}

// ---------- shell ----------
function openShell(p) {
  if (!p) return;
  pickContainer(p, (container) => openShellWith(p, container));
}

function openShellWith(p, container) {
  closeShell();
  const modal = $("#shell-modal");
  $("#shell-title").textContent = `shell · ${p.namespace}/${p.name}${container ? " · " + container : ""}`;
  $("#shell-status").textContent = "connecting";
  modal.classList.remove("hidden");

  const termEl = $("#shell-term");
  termEl.innerHTML = "";

  if (typeof Terminal === "undefined") {
    $("#shell-status").textContent = "xterm not loaded (check network)";
    return;
  }

  const term = new Terminal({
    cursorBlink: true,
    fontFamily: '"SF Mono", Menlo, Consolas, monospace',
    fontSize: 13,
    theme: { background: "#000000", foreground: "#d8dde2", cursor: "#d8dde2" },
    convertEol: true,
  });
  let fit = null;
  if (typeof FitAddon !== "undefined" && FitAddon.FitAddon) {
    fit = new FitAddon.FitAddon();
    term.loadAddon(fit);
  }
  term.open(termEl);
  if (fit) fit.fit();

  const scheme = location.protocol === "https:" ? "wss" : "ws";
  const q = new URLSearchParams({ ns: p.namespace, pod: p.name });
  if (container) q.set("container", container);
  const ws = new WebSocket(`${scheme}://${location.host}/api/shell?${q}`);
  ws.binaryType = "arraybuffer";

  const sendResize = () => {
    if (ws.readyState !== WebSocket.OPEN) return;
    ws.send("1:" + JSON.stringify({ cols: term.cols, rows: term.rows }));
  };

  ws.onopen = () => {
    $("#shell-status").textContent = "connected";
    sendResize();
    term.focus();
  };
  ws.onmessage = (ev) => {
    const data = typeof ev.data === "string" ? ev.data : new TextDecoder().decode(new Uint8Array(ev.data));
    if (data.startsWith("__exit__:")) {
      term.write("\r\n\x1b[33m[" + data.slice(9).trim() + "]\x1b[0m\r\n");
      $("#shell-status").textContent = "closed";
      return;
    }
    term.write(data);
  };
  ws.onclose = () => { $("#shell-status").textContent = "closed"; };
  ws.onerror = () => { $("#shell-status").textContent = "error"; };

  term.onData(d => { if (ws.readyState === WebSocket.OPEN) ws.send("0:" + d); });

  const onResize = () => { if (fit) fit.fit(); sendResize(); };
  window.addEventListener("resize", onResize);

  state.shell = { ws, term, onResize };
}

function closeShell() {
  if (!state.shell) { $("#shell-modal").classList.add("hidden"); return; }
  try { state.shell.ws.close(); } catch (e) {}
  try { state.shell.term.dispose(); } catch (e) {}
  window.removeEventListener("resize", state.shell.onResize);
  state.shell = null;
  $("#shell-term").innerHTML = "";
  $("#shell-modal").classList.add("hidden");
}

// ---------- port-forward ----------
async function refreshPortForwards() {
  try {
    state.pfs = await jget("/api/portforward");
  } catch (e) {
    state.pfs = [];
  }
  renderPortForwards();
}

function renderPortForwards() {
  const bar = $("#pf-bar");
  if (!state.pfs.length) { bar.classList.add("hidden"); bar.innerHTML = ""; return; }
  bar.classList.remove("hidden");
  bar.innerHTML = `<span class="pf-label">port-forwards</span>` + state.pfs.map(e => `
    <span class="pf-chip" data-id="${e.id}">
      <a href="http://127.0.0.1:${e.localPort}" target="_blank">:${e.localPort}</a>
      <span class="pf-meta">→ ${e.namespace}/${e.pod}:${e.podPort}</span>
      <span class="x" title="stop">×</span>
    </span>`).join("");
  $$(".pf-chip .x", bar).forEach((el, i) => {
    el.addEventListener("click", () => stopPortForward(state.pfs[i].id));
  });
}

function openPortForwardModal(p) {
  if (!p) return;
  const m = $("#pf-modal");
  $("#pf-title").textContent = `port-forward · ${p.namespace}/${p.name}`;
  $("#pf-pod-port").value = "";
  $("#pf-local-port").value = "";
  $("#pf-err").textContent = "";
  const sel = $("#pf-container");
  sel.innerHTML = "";
  (p.containers || []).forEach(c => {
    const o = document.createElement("option"); o.value = c; o.textContent = c; sel.appendChild(o);
  });
  m.classList.remove("hidden");
  $("#pf-pod-port").focus();

  const start = async () => {
    const podPort = parseInt($("#pf-pod-port").value, 10);
    const localPort = parseInt($("#pf-local-port").value, 10) || 0;
    if (!podPort || podPort <= 0) { $("#pf-err").textContent = "pod port required"; return; }
    try {
      const r = await fetch("/api/portforward", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ namespace: p.namespace, pod: p.name, podPort, localPort }),
      });
      const j = await r.json();
      if (!r.ok) throw new Error(j.error || `${r.status}`);
      m.classList.add("hidden");
      setMsg(`port-forward ${j.localPort} → ${p.name}:${podPort}`);
      refreshPortForwards();
    } catch (e) { $("#pf-err").textContent = e.message; }
  };
  $("#pf-start").onclick = start;
  $("#pf-cancel").onclick = () => m.classList.add("hidden");
}

async function stopPortForward(id) {
  try {
    await fetch("/api/portforward/" + encodeURIComponent(id), { method: "DELETE" });
    setMsg("port-forward stopped");
    refreshPortForwards();
  } catch (e) { setMsg("stop error: " + e.message); }
}

function geminiForPod(p) {
  state.gemini.pod = p;
  switchView("gemini");
  renderGemini();
  $("#gemini-input").focus();
}

// ---------- kubeconfig context switching ----------
async function loadContexts() {
  try {
    const r = await jget("/api/contexts");
    state.contexts = r.contexts || [];
    state.currentCtx = r.current || "";
    renderContexts();
  } catch (e) {
    $("#ctx-status").textContent = "ctx err";
    setMsg("contexts: " + e.message);
  }
}

function renderContexts() {
  const sel = $("#ctx-select");
  if (!sel) return;
  sel.innerHTML = "";
  if (state.contexts.length === 0) {
    const o = document.createElement("option");
    o.value = "";
    o.textContent = "(none)";
    sel.appendChild(o);
    sel.disabled = true;
    return;
  }
  sel.disabled = state.ctxSwitching;
  for (const name of state.contexts) {
    const o = document.createElement("option");
    o.value = name;
    o.textContent = name;
    sel.appendChild(o);
  }
  if (state.currentCtx && state.contexts.includes(state.currentCtx)) {
    sel.value = state.currentCtx;
  }
  $("#ctx-status").textContent = state.ctxSwitching ? "switching..." : "";
}

async function switchContext(name) {
  if (!name || name === state.currentCtx) return;
  state.ctxSwitching = true;
  renderContexts();
  setMsg(`switching to ${name}...`);
  try {
    const r = await fetch("/api/contexts/switch", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ context: name }),
    });
    const data = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(data.error || `${r.status}`);
    state.currentCtx = data.current || name;
    if (data.warning) setMsg("warning: " + data.warning);
    else setMsg(`context: ${state.currentCtx}`);
    // Reset cluster-scoped state so the new context starts clean.
    state.pods = [];
    state.nodes = [];
    state.stats = null;
    state.namespaces = ["ALL"];
    state.nsSelected = null;
    state.jobNsSel = null;
    if (state.podNsMs) state.podNsMs.clear();
    if (state.jobNsMs) state.jobNsMs.clear();
    state.rsNs = "ALL";
    state.rs = null;
    state.candNs = "ALL";
    state.selected = -1;
    state.costs = null;
    state.gemini = { history: [], busy: false, pod: null, provider: state.gemini.provider || "openrouter" };
    await refresh();
    refreshPortForwards();
  } catch (e) {
    setMsg("context switch failed: " + e.message);
    // Re-sync the select with the actual current context.
    await loadContexts();
  } finally {
    state.ctxSwitching = false;
    renderContexts();
  }
}

// ---------- view switching ----------
function switchView(v) {
  state.view = v;
  $$(".view").forEach(s => s.classList.toggle("active", s.id === "view-" + v));
  $$(".nav").forEach(n => n.classList.toggle("active", n.dataset.view === v));
  if (v === "load") populateTraffic();
  if (v === "nodes") armCapacityCooldown();
  if (v === "rightsizing") loadRightsizing();
  if (v === "incidents") { wireSRE(); loadIncidents(); }
  if (v === "remediation") { wireSRE(); loadRemediation(); }
}

// ---------- key bindings ----------
function bindKeys() {
  document.addEventListener("keydown", (e) => {
    if (!$("#shell-modal").classList.contains("hidden")) {
      // shell owns the keyboard; only Esc when not focused on terminal
      return;
    }
    if (!$("#patch-modal").classList.contains("hidden")) {
      if (e.key === "Escape") closePatchModal();
      return;
    }
    if (!$("#pf-modal").classList.contains("hidden")) {
      if (e.key === "Escape") $("#pf-modal").classList.add("hidden");
      if (e.key === "Enter") $("#pf-start").click();
      return;
    }
    if (!$("#container-modal").classList.contains("hidden")) {
      if (e.key === "Escape") $("#container-modal").classList.add("hidden");
      return;
    }
    if (!$("#modal").classList.contains("hidden")) {
      if (e.key === "Escape") closeModal();
      return;
    }
    if (!$("#confirm").classList.contains("hidden")) {
      if (e.key === "Escape") $("#confirm-no").click();
      if (e.key === "Enter") $("#confirm-yes").click();
      return;
    }
    const ae = document.activeElement;
    if (ae && (ae.tagName === "INPUT" || ae.tagName === "SELECT" || ae.tagName === "TEXTAREA")) {
      if (e.key === "Escape") ae.blur();
      return;
    }

    if (state.view !== "pods") {
      // global hotkeys when not on pods
      if (e.key === "1") switchView("pods");
      else if (e.key === "2") switchView("nodes");
      else if (e.key === "3") switchView("rightsizing");
      else if (e.key === "4") switchView("finops");
      else if (e.key === "5") switchView("gemini");
      return;
    }

    switch (e.key) {
      case "j": case "ArrowDown":
        if (state.selected < state.filtered.length - 1) { state.selected++; renderPodTable(); }
        e.preventDefault(); break;
      case "k": case "ArrowUp":
        if (state.selected > 0) { state.selected--; renderPodTable(); }
        e.preventDefault(); break;
      case "/":
        $("#search").focus(); e.preventDefault(); break;
      case "l": openLogs(selectedPod()); break;
      case "y": openYaml(selectedPod()); break;
      case "d": openDiagnose(selectedPod()); break;
      case "x": deletePod(selectedPod()); break;
      case "s": openShell(selectedPod()); break;
      case "f": openPortForwardModal(selectedPod()); break;
      case "g": geminiForPod(selectedPod()); break;
      case "r": refresh(); break;
      case "1": switchView("pods"); break;
      case "2": switchView("nodes"); break;
      case "3": switchView("rightsizing"); break;
      case "4": switchView("finops"); break;
      case "5": switchView("gemini"); break;
    }
  });
}

// ---------- wire up ----------
function init() {
  $$(".nav").forEach(n => n.addEventListener("click", e => {
    e.preventDefault();
    switchView(n.dataset.view);
  }));
  $("#refresh-btn").addEventListener("click", refresh);
  $("#ctx-select").addEventListener("change", e => switchContext(e.target.value));

  state.podNsMs = createMultiSelect($("#ns-ms"), "ALL", sel => { state.nsSelected = sel; renderPodTable(); });
  state.jobNsMs = createMultiSelect($("#job-ns-ms"), "ALL", sel => { state.jobNsSel = sel; renderJobTable(); });
  $("#job-search").addEventListener("input", e => { state.jobSearch = e.target.value; renderJobTable(); });
  $("#job-all-runs").addEventListener("change", e => { state.jobAllRuns = e.target.checked; renderJobTable(); });
  $("#job-failed-only").addEventListener("change", e => { state.jobFailedOnly = e.target.checked; renderJobTable(); });
  $("#rs-ns-select").addEventListener("change", e => { state.rsNs = e.target.value; loadRightsizing(); });
  $("#rs-search").addEventListener("input", e => { state.rsSearch = e.target.value; renderRightsizing(); });
  $("#rs-issues-only").addEventListener("change", e => { state.rsIssuesOnly = e.target.checked; renderRightsizing(); });
  $("#search").addEventListener("input", e => { state.search = e.target.value; renderPodTable(); });
  $("#issues-only").addEventListener("change", e => { state.issuesOnly = e.target.checked; renderPodTable(); });
  $("#hide-cron").addEventListener("change", e => { state.hideCron = e.target.checked; renderPodTable(); });
  $$(".sort").forEach(b => b.addEventListener("click", () => { state.sort = b.dataset.sort; renderPodTable(); }));
  $("#costs-load").addEventListener("click", loadCosts);
  $("#cost-month").addEventListener("change", () => { if (state.costServices || state.costs) loadCostServices(); });
  {
    const card = $("#cost-services-card"), toggle = $(".card-collapse", card);
    const flip = () => {
      const collapsed = card.classList.toggle("collapsed");
      toggle.setAttribute("aria-expanded", String(!collapsed));
    };
    toggle.addEventListener("click", flip);
    toggle.addEventListener("keydown", e => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); flip(); } });
  }

  // Traffic view: mode switch + shared namespace → pod → endpoint target.
  $$(".tf-mode .btn").forEach(b => b.addEventListener("click", () => setTrafficMode(b.dataset.mode)));
  $("#pb-ns").addEventListener("change", populateTrafficPods);
  $("#pb-pod").addEventListener("change", () => { loadProbeEndpoints(); updateTfTargetHint(); });
  $("#pb-endpoint").addEventListener("change", applyProbeEndpoint);
  $("#pb-scheme").addEventListener("change", updateTfTargetHint);
  $("#pb-port").addEventListener("input", updateTfTargetHint);
  $("#pb-path").addEventListener("input", updateTfTargetHint);

  const runCurrent = () => (tfMode === "load" ? runLoadTest() : runProbe());
  $("#pb-run").addEventListener("click", runProbe);
  $("#pb-port").addEventListener("keydown", e => { if (e.key === "Enter") runCurrent(); });
  $("#pb-path").addEventListener("keydown", e => { if (e.key === "Enter") runCurrent(); });

  $("#lt-run").addEventListener("click", runLoadTest);
  $("#lt-method").addEventListener("change", e => {
    const needsBody = ["POST", "PUT", "PATCH"].includes(e.target.value);
    $("#lt-body-row").style.display = needsBody ? "" : "none";
  });

  $("#gemini-send").addEventListener("click", sendGemini);
  $("#gemini-input").addEventListener("keydown", e => { if (e.key === "Enter") sendGemini(); });
  $("#gemini-clear").addEventListener("click", () => { state.gemini.history = []; state.gemini.pod = null; $("#gemini-err").textContent = ""; renderGemini(); });
  $("#gemini-provider").addEventListener("change", e => {
    state.gemini.provider = e.target.value;
    setMsg(`chat provider: ${state.gemini.provider}`);
    syncApiKeyField();
  });
  $("#gemini-key").addEventListener("input", e => {
    saveApiKey(state.gemini.provider, e.target.value.trim());
    const status = $("#gemini-key-status");
    if (status) status.textContent = e.target.value.trim() ? "saved ✓" : "not set";
  });
  $("#gemini-maxtokens").addEventListener("input", e => {
    try {
      const v = e.target.value.trim();
      if (v) localStorage.setItem("clusterheart:maxTokens", v);
      else localStorage.removeItem("clusterheart:maxTokens");
    } catch (_) {}
  });
  $("#gemini-model").addEventListener("input", e => {
    try {
      const v = e.target.value.trim();
      if (v) localStorage.setItem("clusterheart:orModel", v);
      else localStorage.removeItem("clusterheart:orModel");
    } catch (_) {}
  });

  $("#modal-close").addEventListener("click", closeModal);
  $("#modal-copy").addEventListener("click", () => {
    navigator.clipboard.writeText($("#modal-body").textContent).then(() => setMsg("copied"));
  });
  $("#modal").addEventListener("click", e => { if (e.target.id === "modal") closeModal(); });
  $("#patch-close").addEventListener("click", closePatchModal);
  $("#patch-modal").addEventListener("click", e => { if (e.target.id === "patch-modal") closePatchModal(); });
  $("#patch-copy").addEventListener("click", () => {
    const cmd = $("#patch-cmd").textContent;
    navigator.clipboard.writeText(cmd).then(() => {
      $("#patch-result").textContent = "command copied";
      $("#patch-result").classList.remove("err");
    });
  });
  $("#patch-apply").addEventListener("click", applyPatch);
  $("#shell-close").addEventListener("click", closeShell);
  $("#shell-modal").addEventListener("click", e => { if (e.target.id === "shell-modal") closeShell(); });
  $("#container-cancel").addEventListener("click", () => $("#container-modal").classList.add("hidden"));

  ["pod-table", "job-table", "rs-table"].forEach(id => makeResizable(document.getElementById(id)));

  bindKeys();
  switchView("pods");
  populateCostMonths();
  renderCostPlaceholder();
  loadContexts();
  refresh();
  refreshPortForwards();
  setInterval(refresh, 5000);
  setInterval(refreshPortForwards, 4000);
}
init();

// ======================================================================
// Autonomous SRE: incidents + remediation approval queue.
// All data comes from the dashboard's server-side proxy (/api/sre/*), which
// injects the control-plane admin token and pins the tenant. The renderer
// never holds the token. Nothing here is fabricated: an unconfigured or
// unreachable control plane shows an explicit notice, not fake rows.
// ======================================================================
const sreState = { configured: null, tenantId: "", paused: false };
let sreWired = false;

function sreEsc(s) {
  return String(s == null ? "" : s)
    .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}
function sreTime(t) {
  if (!t) return "—";
  const d = new Date(t);
  if (isNaN(d)) return "—";
  return d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}
function sevBadge(sev) {
  const s = String(sev || "").toLowerCase();
  return `<span class="sre-badge sev-${sreEsc(s) || "unknown"}">${sreEsc(s || "—")}</span>`;
}
function statusBadge(st) {
  const s = String(st || "").toLowerCase();
  return `<span class="sre-badge st-${sreEsc(s)}">${sreEsc(s || "—")}</span>`;
}

function showNotice(el, kind, msg) {
  el.className = "panel sre-notice " + (kind || "");
  el.textContent = msg;
  el.classList.remove("hidden");
}
function hideNotice(el) { el.classList.add("hidden"); }

// Resolve configured-ness once (cached). Returns the config object.
async function sreConfig() {
  if (sreState.configured !== null) return sreState;
  try {
    const c = await jget("/api/sre/config");
    sreState.configured = !!c.configured;
    sreState.tenantId = c.tenantId || "";
  } catch (_) {
    sreState.configured = false;
  }
  return sreState;
}

function sreUnconfiguredMsg() {
  return "SRE control plane is not configured. Launch the dashboard with " +
    "CONTROLPLANE_URL, CONTROLPLANE_ADMIN_TOKEN and SRE_TENANT_ID set to connect " +
    "to your Autonomous SRE control plane.";
}

// ---------- incidents ----------
async function loadIncidents() {
  const notice = $("#sre-inc-notice"), tbody = $("#sre-inc-tbody"), meta = $("#sre-inc-meta");
  const cfg = await sreConfig();
  if (!cfg.configured) { tbody.innerHTML = ""; meta.textContent = ""; showNotice(notice, "warn", sreUnconfiguredMsg()); return; }
  hideNotice(notice);
  meta.textContent = "loading…";
  const status = $("#sre-inc-status").value;
  try {
    const data = await jget("/api/sre/incidents" + (status ? `?status=${encodeURIComponent(status)}` : ""));
    sreState._incidents = (data && data.incidents) || [];
    renderIncidents();
    meta.textContent = `tenant ${cfg.tenantId}`;
  } catch (e) {
    tbody.innerHTML = ""; meta.textContent = "";
    showNotice(notice, "err", "could not load incidents: " + e.message);
  }
}
function renderIncidents() {
  const tbody = $("#sre-inc-tbody");
  const q = ($("#sre-inc-search").value || "").toLowerCase();
  const rows = (sreState._incidents || []).filter(i =>
    !q || [i.type, i.workload, i.namespace, i.title].some(x => String(x || "").toLowerCase().includes(q)));
  if (!rows.length) { tbody.innerHTML = `<tr><td colspan="9" class="dim" style="padding:14px">no incidents</td></tr>`; return; }
  tbody.innerHTML = rows.map(i => `
    <tr class="sre-row" data-id="${sreEsc(i.id)}">
      <td>${sevBadge(i.severity)}</td>
      <td>${sreEsc(i.type)}</td>
      <td>${sreEsc(i.workload || "—")}</td>
      <td class="c-ns">${sreEsc(i.namespace || "—")}</td>
      <td class="num">${i.occurrences || 0}</td>
      <td class="dim">${sreTime(i.firstSeen)}</td>
      <td class="dim">${sreTime(i.lastSeen)}</td>
      <td>${statusBadge(i.status)}</td>
      <td class="act">
        <button class="btn sre-diagnose" data-id="${sreEsc(i.id)}">diagnose</button>
        <button class="btn sre-propose" data-id="${sreEsc(i.id)}" title="turn the AI recommendation into a policy-checked proposal">propose fix</button>
      </td>
    </tr>`).join("");
  $$("#sre-inc-tbody .sre-row").forEach(tr => tr.addEventListener("click", e => {
    if (e.target.closest("button")) return;
    const inc = (sreState._incidents || []).find(x => x.id === tr.dataset.id);
    if (inc) openModal(`incident · ${inc.type}`, JSON.stringify(inc, null, 2));
  }));
  $$("#sre-inc-tbody .sre-diagnose").forEach(b => b.addEventListener("click", () => diagnoseIncident(b.dataset.id, b)));
  $$("#sre-inc-tbody .sre-propose").forEach(b => b.addEventListener("click", () => proposeFix(b.dataset.id, b)));
}

// proposeFix turns an incident's AI recommendation into a policy-checked
// remediation proposal (server-side). On success it jumps to the remediation
// approval queue; if nothing is auto-recommended it says so (never fabricates).
async function proposeFix(id, btn) {
  if (btn) { btn.disabled = true; btn.textContent = "proposing…"; }
  try {
    const r = await fetch(`/api/sre/incidents/${encodeURIComponent(id)}/propose`, { method: "POST" });
    const data = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(data.error || `${r.status}`);
    setMsg(`proposal created (${data.status}) — see remediation`);
    switchView("remediation");
  } catch (e) {
    setMsg("propose failed: " + e.message);
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = "propose fix"; }
  }
}

// diagnoseIncident runs AI root-cause analysis for an incident and renders the
// evidence-backed diagnosis. Advisory only — it does not create a remediation.
async function diagnoseIncident(id, btn) {
  if (btn) { btn.disabled = true; btn.textContent = "diagnosing…"; }
  try {
    const r = await fetch(`/api/sre/incidents/${encodeURIComponent(id)}/diagnose`, { method: "POST" });
    if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || `${r.status}`);
    openModal("root-cause analysis", formatDiagnosis(await r.json()));
  } catch (e) {
    setMsg("diagnose failed: " + e.message);
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = "diagnose"; }
  }
}

// formatDiagnosis renders a Diagnosis as readable text for the <pre> modal,
// keeping measured facts and inferred hypotheses clearly separated.
function formatDiagnosis(d) {
  const L = [];
  const src = d.source === "llm" ? `AI (${d.model || "llm"}, ${d.promptVersion || ""})` : "deterministic rules";
  L.push(`SUMMARY`, `  ${d.summary || "—"}`, "");
  L.push(`SOURCE: ${src}   CONFIDENCE: ${(d.confidence || "—").toUpperCase()}` + (d.latencyMs ? `   (${d.latencyMs} ms)` : ""), "");
  L.push("OBSERVED FACTS (measured)");
  (d.observedFacts || []).forEach(f => L.push(`  • ${f}`));
  L.push("", "HYPOTHESES (inferred, most likely first)");
  (d.hypotheses || []).forEach((h, i) => {
    L.push(`  ${i + 1}. [${(h.confidence || "?").toUpperCase()}] ${h.cause}`);
    (h.supporting || []).forEach(s => L.push(`        + ${s}`));
    (h.contradicting || []).forEach(s => L.push(`        - ${s}`));
  });
  L.push("", "RECOMMENDED");
  if (d.recommendedActionable) {
    const t = d.recommendedTarget || {};
    L.push(`  action: ${d.recommendedAction}  →  ${t.kind}/${t.namespace}/${t.name}`);
    L.push(`  (advisory — still requires a policy-checked proposal + approval; it is NOT auto-applied)`);
  } else {
    L.push(`  ${d.recommendedAction || "investigate"} (no automatic action)`);
  }
  L.push("", "RISKS", `  ${d.risks || "—"}`);
  L.push("", "VERIFICATION");
  (d.verificationCriteria || []).forEach(v => L.push(`  • ${v}`));
  L.push("", "ROLLBACK", `  ${d.rollback || "—"}`);
  return L.join("\n");
}

// ---------- remediation approval queue ----------
async function loadRemediation() {
  const notice = $("#sre-prop-notice"), tbody = $("#sre-prop-tbody");
  const cfg = await sreConfig();
  if (!cfg.configured) { tbody.innerHTML = ""; $("#sre-policy-meta").textContent = ""; showNotice(notice, "warn", sreUnconfiguredMsg()); return; }
  hideNotice(notice);
  await loadPolicyMeta();
  const status = $("#sre-prop-status").value;
  try {
    const data = await jget("/api/sre/proposals" + (status ? `?status=${encodeURIComponent(status)}` : ""));
    sreState._proposals = (data && data.proposals) || [];
    renderProposals();
  } catch (e) {
    tbody.innerHTML = "";
    showNotice(notice, "err", "could not load proposals: " + e.message);
  }
}
async function loadPolicyMeta() {
  try {
    const pol = await jget("/api/sre/policy");
    sreState.paused = !!pol.paused;
    const meta = $("#sre-policy-meta");
    meta.textContent = `mode ${pol.mode}${pol.paused ? " · PAUSED" : ""}`;
    const btn = $("#sre-pause-btn");
    btn.textContent = pol.paused ? "resume automation" : "pause automation";
    btn.classList.toggle("danger", !pol.paused);
  } catch (_) { /* leave meta blank on failure */ }
}
function renderProposals() {
  const tbody = $("#sre-prop-tbody");
  const rows = sreState._proposals || [];
  if (!rows.length) { tbody.innerHTML = `<tr><td colspan="7" class="dim" style="padding:14px">no proposals</td></tr>`; return; }
  tbody.innerHTML = rows.map(p => {
    const t = p.target || {};
    const target = `${sreEsc(t.kind || "")}/${sreEsc(t.namespace || "")}/${sreEsc(t.name || "")}`;
    let result = "—";
    if (p.execution) {
      const e = p.execution;
      const verdict = e.success ? "ok" : "failed";
      const dry = e.dryRun ? " (dry-run)" : "";
      result = `<span class="sre-exec ${e.success ? "ok" : "bad"}">${verdict}${dry}</span>` +
        (e.error ? `<div class="dim sre-exec-err">${sreEsc(e.error)}</div>` : "");
    } else if (p.status === "blocked" && p.decision && p.decision.reasons) {
      result = `<span class="dim">${sreEsc(p.decision.reasons.join("; "))}</span>`;
    }
    const actions = p.status === "proposed"
      ? `<button class="btn primary sre-approve" data-id="${sreEsc(p.id)}">approve</button>
         <button class="btn sre-reject" data-id="${sreEsc(p.id)}">reject</button>`
      : "";
    return `
      <tr class="sre-row" data-id="${sreEsc(p.id)}">
        <td>${statusBadge(p.status)}</td>
        <td>${sreEsc(p.action)}</td>
        <td>${target}</td>
        <td>${sevBadge(p.riskClass)}</td>
        <td class="sre-rationale">${sreEsc(p.rationale || "—")}</td>
        <td>${result}</td>
        <td class="act">${actions}</td>
      </tr>`;
  }).join("");
  $$("#sre-prop-tbody .sre-row").forEach(tr => tr.addEventListener("click", e => {
    if (e.target.closest("button")) return; // let action buttons handle their own click
    const p = (sreState._proposals || []).find(x => x.id === tr.dataset.id);
    if (p) openModal(`proposal · ${p.action}`, JSON.stringify(p, null, 2));
  }));
  $$("#sre-prop-tbody .sre-approve").forEach(b => b.addEventListener("click", () => decideProposal(b.dataset.id, "approve")));
  $$("#sre-prop-tbody .sre-reject").forEach(b => b.addEventListener("click", () => decideProposal(b.dataset.id, "reject")));
}
async function decideProposal(id, action) {
  const body = action === "reject" ? { reason: "rejected from dashboard" } : {};
  try {
    const r = await fetch(`/api/sre/proposals/${encodeURIComponent(id)}/${action}`, {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
    });
    if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || `${r.status}`);
    setMsg(`proposal ${action}d`);
    loadRemediation();
  } catch (e) { setMsg(`could not ${action}: ${e.message}`); }
}
async function togglePause() {
  const path = sreState.paused ? "/api/sre/policy/resume" : "/api/sre/policy/pause";
  try {
    const r = await fetch(path, { method: "POST" });
    if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || `${r.status}`);
    setMsg(sreState.paused ? "automation resumed" : "automation paused");
    loadRemediation();
  } catch (e) { setMsg("could not change pause state: " + e.message); }
}

function wireSRE() {
  if (sreWired) return;
  sreWired = true;
  $("#sre-inc-refresh").addEventListener("click", loadIncidents);
  $("#sre-inc-status").addEventListener("change", loadIncidents);
  $("#sre-inc-search").addEventListener("input", renderIncidents);
  $("#sre-prop-refresh").addEventListener("click", loadRemediation);
  $("#sre-prop-status").addEventListener("change", loadRemediation);
  $("#sre-pause-btn").addEventListener("click", togglePause);
}
