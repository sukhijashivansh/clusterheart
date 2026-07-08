// kubepulse-ui frontend. Single-file vanilla JS — no framework on purpose.

const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => Array.from(r.querySelectorAll(s));

const state = {
  view: "pods",
  pods: [],
  filtered: [],
  nodes: [],
  stats: null,
  namespaces: ["ALL"],
  currentNs: "ALL",
  resNs: "ALL",
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
  costsLoading: false,
  headroom: 20,
  gemini: { history: [], busy: false, pod: null, provider: "gemini" },
  pfs: [],
  shell: null,
  contexts: [],
  currentCtx: "",
  ctxSwitching: false,
  consolidationCollapsed: false,
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
    const [pods, stats, nodes, namespaces] = await Promise.all([
      jget("/api/pods"),
      jget("/api/stats").catch(() => null),
      jget("/api/nodes").catch(() => []),
      jget("/api/namespaces").catch(() => []),
    ]);
    state.pods = pods || [];
    state.stats = stats;
    state.nodes = nodes || [];
    state.namespaces = ["ALL", ...(namespaces || [])];
    renderNamespaceSelects();
    renderStats();
    renderPodTable();
    renderNodes();
    renderResources();
    renderFinOps();
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
  for (const id of ["#ns-select", "#res-ns-select", "#cand-ns-select"]) {
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
}

// ---------- pod table ----------
function applyFilter() {
  let pods = state.pods.slice();
  if (state.currentNs !== "ALL") pods = pods.filter(p => p.namespace === state.currentNs);
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

// ---------- nodes ----------
function nodeBar(p) {
  const cls = p == null ? "" : p > 80 ? " hot" : p > 50 ? " warn" : "";
  const w = p == null ? 0 : Math.max(0, Math.min(100, p));
  return `<span class="bar${cls}"><i style="width:${w}%"></i></span>`;
}

function renderNodes() {
  const wrap = $("#node-list");
  wrap.innerHTML = "";
  let tCpuCap = 0, tCpuUse = 0, tMemCap = 0, tMemUse = 0;
  state.nodes.forEach(n => {
    tCpuCap += n.cpuCap; tCpuUse += n.cpuUsed;
    tMemCap += n.memCap; tMemUse += n.memUsed;
    const cpuP = pct(n.cpuUsed, n.cpuCap);
    const memP = pct(n.memUsed, n.memCap);
    const ephP = n.ephHasUsed ? pct(n.ephUsed, n.ephUsed + n.ephAvail) : null;
    const ready = n.ready
      ? `<span class="status-ok">Ready</span>`
      : `<span class="status-err">NotReady</span>`;
    const box = document.createElement("div");
    box.className = "node";
    box.innerHTML = `
      <div class="head">
        <div class="name" title="${n.name}">${n.name}</div>
        <div class="badges"><span>${n.role}</span><span>${ready}</span><span>${n.age}</span><span>${n.podCount}/${n.podLimit} pods</span></div>
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
  renderTopConsumers();
}

function renderConsolidation() {
  const el = $("#consolidation-card");
  if (!el) return;
  if (!state.nodes.length || !state.pods.length) { el.innerHTML = ""; return; }

  const active = state.pods.filter(isActive);
  if (!active.length) { el.innerHTML = ""; return; }

  // Collapsible header. Clicking it toggles state.consolidationCollapsed, which
  // survives the 5s node re-render so the card stays where the user left it.
  const head = (badge) => `
    <div class="card-head" role="button" tabindex="0" aria-expanded="${!state.consolidationCollapsed}">
      <span class="chev" aria-hidden="true">▾</span>
      <span class="title">consolidation analysis</span>
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
  const couldRemove = current - minNodes;

  // already at floor — nothing to say
  if (couldRemove <= 0) {
    el.innerHTML = `
      ${head(`<span class="badge-tag ok">at floor (${current} node${current === 1 ? "" : "s"} = min)</span>`)}
      <div class="card-body">
        <div class="dim" style="font-size: 12px">cluster is already running at the minimum node count its workloads can fit on. nothing to consolidate.</div>
      </div>`;
    wireCollapse();
    return;
  }

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

  const peak = Math.max(current, minNodesReq, minNodes, 1);
  const row = (label, val, klass, note) => `
    <div class="proj-row">
      <span class="lbl">${label}</span>
      <span class="track"><span class="fill ${klass}" style="width:${(val/peak*100).toFixed(1)}%"></span></span>
      <span class="val">${val}</span>
      ${note ? `<span class="note dim">${note}</span>` : ""}
    </div>`;

  // Phantom = how much capacity the scheduler is holding for these pods that
  // they're not actually using. This is what triggers new nodes to be spun up.
  const phantomCpu = Math.max(0, totalCpuReq - totalCpuUse);
  const phantomMem = Math.max(0, totalMemReq - totalMemUse);
  const phantomCpuPct = totalCpuReq > 0 ? (phantomCpu / totalCpuReq) * 100 : 0;
  const phantomMemPct = totalMemReq > 0 ? (phantomMem / totalMemReq) * 100 : 0;

  // Top offenders — rank by combined phantom load. Weight CPU and MEM equally
  // by normalising against per-node capacity, so a pod hoarding 500m phantom
  // CPU and one hoarding 2Gi phantom MEM both surface.
  const offenders = active
    .filter(p => p.cpuRequest > 0 || p.memRequest > 0)
    .map(p => {
      const pCpu = Math.max(0, p.cpuRequest - p.rawCpu);
      const pMem = Math.max(0, p.memRequest - p.rawMem);
      const score = (pCpu / nodeCpu) + (pMem / nodeMem);
      return { p, pCpu, pMem, score };
    })
    .filter(o => o.score > 0.01)
    .sort((a, b) => b.score - a.score)
    .slice(0, 8);

  const inflationRows = offenders.map((o, i) => {
    const cpuP = o.p.cpuRequest > 0 ? (o.p.rawCpu / o.p.cpuRequest) * 100 : null;
    const memP = o.p.memRequest > 0 ? (o.p.rawMem / o.p.memRequest) * 100 : null;
    return `<tr data-off="${i}">
      <td>${o.p.namespace}</td>
      <td title="${o.p.name}">${o.p.name}</td>
      <td class="num">${fmtCpu(o.p.cpuRequest)}</td>
      <td class="num">${fmtCpu(o.p.rawCpu)}</td>
      <td class="num">${cpuP !== null ? cpuP.toFixed(0) + "%" : "-"}</td>
      <td class="num">${fmtBytes(o.p.memRequest)}</td>
      <td class="num">${fmtBytes(o.p.rawMem)}</td>
      <td class="num">${memP !== null ? memP.toFixed(0) + "%" : "-"}</td>
      <td class="num save">${o.pCpu > 0 ? fmtCpu(o.pCpu) : "-"} / ${o.pMem > 0 ? fmtBytes(o.pMem) : "-"}</td>
      <td class="act"><button class="patch-btn" data-cons-off="${i}">patch</button></td>
    </tr>`;
  }).join("");

  el.innerHTML = `
    ${head(`<span class="badge-tag">${couldRemove} node${couldRemove === 1 ? "" : "s"} removable</span>`)}
    <div class="card-body">
      <div class="grid">
        <div><span class="k">active pods</span>${active.length} <span class="dim">(limit ${podLimit}/node)</span></div>
        <div><span class="k">cpu demand</span>${fmtCpu(totalCpuUse)} live · ${fmtCpu(totalCpuReq)} requested <span class="dim">(${phantomCpuPct.toFixed(0)}% phantom)</span></div>
        <div><span class="k">mem demand</span>${fmtBytes(totalMemUse)} live · ${fmtBytes(totalMemReq)} requested <span class="dim">(${phantomMemPct.toFixed(0)}% phantom)</span></div>
        <div><span class="k">node template</span>${fmtCpu(nodeCpu)} · ${fmtBytes(nodeMem)}</div>
      </div>
      <div class="proj">
        ${row("current", current, "warn", "")}
        ${row("min by requests", minNodesReq, minNodesReq <= current ? "ok" : "hot", `binding: ${reqBinding.join(" + ")}`)}
        ${row("min by live usage", minNodes, "ok", `binding: ${liveBinding.join(" + ")} · with ${Math.round((headroom-1)*100)}% headroom`)}
      </div>

      <div class="rec-banner down">
        <div class="head">scheduler is reserving ${fmtCpu(phantomCpu)} cpu and ${fmtBytes(phantomMem)} mem that nobody's using</div>
        <div class="line">that's what triggers new nodes — kube-scheduler picks based on requests, not live usage. fix the offenders below and the autoscaler will release the extra node.</div>
      </div>

      ${offenders.length ? `
        <div class="inflation-head">request inflation · top offenders by phantom load</div>
        <div class="candidates">
          <table>
            <thead><tr>
              <th>NAMESPACE</th><th>POD</th>
              <th class="num">CPU REQ</th><th class="num">CPU USE</th><th class="num">CPU%</th>
              <th class="num">MEM REQ</th><th class="num">MEM USE</th><th class="num">MEM%</th>
              <th class="num">PHANTOM (CPU/MEM)</th>
              <th class="act"></th>
            </tr></thead>
            <tbody>${inflationRows}</tbody>
          </table>
        </div>
      ` : ""}
    </div>
  `;

  // Wire patch buttons (re-use the existing patch modal)
  $$(".patch-btn[data-cons-off]", el).forEach(btn => {
    btn.addEventListener("click", () => {
      const idx = parseInt(btn.dataset.consOff, 10);
      const o = offenders[idx];
      if (o) openPatchModal(o.p);
    });
  });

  wireCollapse();
}

function consumerTable(title, byCpu) {
  const ranked = state.pods
    .filter(p => (byCpu ? p.rawCpu : p.rawMem) > 0)
    .sort((a, b) => (byCpu ? b.rawCpu - a.rawCpu : b.rawMem - a.rawMem))
    .slice(0, 10);
  if (!ranked.length) {
    return `<div class="consumer-panel"><h3>${title}</h3>
      <div class="dim">no pod metrics yet — metrics-server may be warming up</div></div>`;
  }
  const max = Math.max(...ranked.map(p => byCpu ? p.rawCpu : p.rawMem));
  const rows = ranked.map((p, i) => {
    const v = byCpu ? p.rawCpu : p.rawMem;
    const valStr = byCpu ? fmtCpu(v) : fmtBytes(v);
    const sharePct = (v / max) * 100;
    return `<tr>
      <td class="rank">${i + 1}</td>
      <td title="${p.namespace}">${p.namespace}</td>
      <td title="${p.name}">${p.name}</td>
      <td class="num">${valStr}</td>
      <td class="share">${nodeBar(sharePct)}</td>
    </tr>`;
  }).join("");
  return `<div class="consumer-panel"><h3>${title}</h3>
    <table><thead><tr><th></th><th>NAMESPACE</th><th>POD</th><th class="num">${byCpu ? "CPU" : "MEM"}</th><th>SHARE</th></tr></thead>
    <tbody>${rows}</tbody></table></div>`;
}

function renderTopConsumers() {
  const el = $("#top-consumers");
  if (!el) return;
  if (!state.pods.length) { el.innerHTML = ""; return; }
  el.innerHTML = consumerTable("top cpu consumers", true) + consumerTable("top memory consumers", false);
}

// ---------- resources ----------
function verdict(p, cpuP, memP) {
  if (!isActive(p)) return ["COMPLETED", "verdict-none"];
  if (p.cpuRequest === 0 && p.memRequest === 0) return ["NO REQ", "verdict-none"];
  if (cpuP !== null && cpuP > 90) return ["UPSIZE CPU", "verdict-up"];
  if (memP !== null && memP > 90) return ["UPSIZE MEM", "verdict-up"];
  if (p.restarts > 5) return ["UNSTABLE", "verdict-up"];
  if (cpuP !== null && cpuP < 30 && memP !== null && memP < 30) return ["DOWNSIZE", "verdict-down"];
  if (cpuP !== null && cpuP < 30) return ["DOWNSIZE CPU", "verdict-down"];
  if (memP !== null && memP < 30) return ["DOWNSIZE MEM", "verdict-down"];
  return ["OK", "verdict-ok"];
}

function renderResources() {
  let pods = state.pods.slice();
  if (state.resNs !== "ALL") pods = pods.filter(p => p.namespace === state.resNs);

  // Totals reflect what the scheduler actually reserves — i.e. ACTIVE pods
  // only. Including Succeeded/Failed pods would falsely inflate
  // request/limit totals (their containers have exited; the scheduler frees
  // their reservation) and bury the real utilization signal.
  let tCpuReq = 0, tCpuLim = 0, tCpuUse = 0, tMemReq = 0, tMemLim = 0, tMemUse = 0;
  // Track the "phantom" requests sitting in Completed pod specs so we can
  // surface them transparently instead of silently dropping them.
  let phantomCpuReq = 0, phantomMemReq = 0;
  const rows = pods.map(p => {
    if (isActive(p)) {
      tCpuReq += p.cpuRequest; tCpuLim += p.cpuLimit; tCpuUse += p.rawCpu;
      tMemReq += p.memRequest; tMemLim += p.memLimit; tMemUse += p.rawMem;
    } else {
      phantomCpuReq += p.cpuRequest;
      phantomMemReq += p.memRequest;
    }
    const cpuP = pct(p.rawCpu, p.cpuRequest);
    const memP = pct(p.rawMem, p.memRequest);
    const [v, vc] = verdict(p, cpuP, memP);
    return { p, cpuP, memP, v, vc };
  });

  const rank = r => r.v === "OK" ? 0 : r.v === "NO REQ" ? 1 : 2;
  rows.sort((a, b) => {
    switch (state.resSort) {
      case "cpu": return (b.cpuP ?? -1) - (a.cpuP ?? -1);
      case "mem": return (b.memP ?? -1) - (a.memP ?? -1);
      case "verdict": return rank(b) - rank(a) || a.p.name.localeCompare(b.p.name);
      default: return a.p.namespace.localeCompare(b.p.namespace) || a.p.name.localeCompare(b.p.name);
    }
  });
  let filtered = state.resIssuesOnly ? rows.filter(r => r.v !== "OK") : rows;
  if (state.resSearch) {
    const q = state.resSearch.toLowerCase();
    filtered = filtered.filter(r =>
      r.p.name.toLowerCase().includes(q) ||
      r.p.namespace.toLowerCase().includes(q) ||
      r.v.toLowerCase().includes(q));
  }

  const cpuPctTot = pct(tCpuUse, tCpuReq);
  const memPctTot = pct(tMemUse, tMemReq);

  // reclaimable = requested - used for ACTIVE pods (Running/Pending) below
  // 30% of their request. Succeeded/Failed pods don't reserve capacity so
  // their requests aren't really reclaimable from the cluster's perspective.
  let reclaimableCpu = 0, reclaimableMem = 0, downsizeCount = 0, completedCount = 0;
  for (const r of rows) {
    if (!isActive(r.p)) { completedCount++; continue; }
    let counted = false;
    if (r.cpuP !== null && r.cpuP < 30) {
      reclaimableCpu += Math.max(0, r.p.cpuRequest - r.p.rawCpu);
      counted = true;
    }
    if (r.memP !== null && r.memP < 30) {
      reclaimableMem += Math.max(0, r.p.memRequest - r.p.rawMem);
      counted = true;
    }
    if (counted) downsizeCount++;
  }
  const cpuReclaimPct = tCpuReq > 0 ? (reclaimableCpu / tCpuReq) * 100 : 0;
  const memReclaimPct = tMemReq > 0 ? (reclaimableMem / tMemReq) * 100 : 0;

  // banner — pick the most important message instead of stacking lines
  let bannerKind = "ok", bannerHead = "allocation is balanced", bannerLine = "", bannerAction = "";
  if (tCpuReq === 0 && tMemReq === 0) {
    bannerKind = "warn"; bannerHead = "no requests/limits set on any pod";
    bannerLine = "right-sizing analysis needs pod resource requests to compare against.";
    bannerAction = "add resources.requests to pod manifests, then return here.";
  } else if (cpuPctTot > 90 || memPctTot > 90) {
    bannerKind = "up";
    bannerHead = `resources at risk · cpu ${cpuPctTot.toFixed(0)}% of request · mem ${memPctTot.toFixed(0)}% of request`;
    bannerLine = cpuPctTot > 90 ? "cpu throttling likely under load. " : "";
    bannerLine += memPctTot > 90 ? "memory near limit — OOMKill risk." : "";
    bannerAction = "increase resources.requests on the affected pods.";
  } else if (cpuReclaimPct >= 30 || memReclaimPct >= 30) {
    bannerKind = "down";
    bannerHead = `over-allocated · using ${cpuPctTot.toFixed(0)}% cpu / ${memPctTot.toFixed(0)}% mem of requests`;
    bannerLine = `${downsizeCount} pod${downsizeCount === 1 ? "" : "s"} sitting below 30% of their request. ${fmtCpu(reclaimableCpu)} cpu and ${fmtBytes(reclaimableMem)} mem could be reclaimed by tightening requests.`;
    bannerAction = "lower resources.requests on the candidate pods (see table below).";
  } else {
    bannerHead = `allocation balanced · cpu ${cpuPctTot.toFixed(0)}% · mem ${memPctTot.toFixed(0)}% of requests`;
  }

  $("#res-summary").innerHTML = `
    <h3>namespace summary · ${state.resNs}</h3>
    <div class="hero-stats">
      <div class="item">
        <span class="k">pods</span>
        <span class="v">${pods.length}</span>
        <span class="sub">${downsizeCount} downsize · ${completedCount} completed (excluded from totals)</span>
      </div>
      <div class="item">
        <span class="k">cpu use / req (active)</span>
        <span class="v ${cpuPctTot < 30 ? "down" : cpuPctTot > 90 ? "up" : "ok"}">${fmtCpu(tCpuUse)} <span class="sub">of ${fmtCpu(tCpuReq)} (${cpuPctTot !== null ? cpuPctTot.toFixed(0) + "%" : "—"})</span></span>
        ${phantomCpuReq > 0 ? `<span class="sub">+ ${fmtCpu(phantomCpuReq)} in completed pod specs (not reserved)</span>` : ""}
      </div>
      <div class="item">
        <span class="k">mem use / req (active)</span>
        <span class="v ${memPctTot < 30 ? "down" : memPctTot > 90 ? "up" : "ok"}">${fmtBytes(tMemUse)} <span class="sub">of ${fmtBytes(tMemReq)} (${memPctTot !== null ? memPctTot.toFixed(0) + "%" : "—"})</span></span>
        ${phantomMemReq > 0 ? `<span class="sub">+ ${fmtBytes(phantomMemReq)} in completed pod specs (not reserved)</span>` : ""}
      </div>
      <div class="item">
        <span class="k">reclaimable cpu</span>
        <span class="v down">${fmtCpu(reclaimableCpu)}</span>
        <span class="sub">${cpuReclaimPct.toFixed(0)}% of requests</span>
      </div>
      <div class="item">
        <span class="k">reclaimable mem</span>
        <span class="v down">${fmtBytes(reclaimableMem)}</span>
        <span class="sub">${memReclaimPct.toFixed(0)}% of requests</span>
      </div>
    </div>
    <div class="rec-banner ${bannerKind}">
      <div class="head">${bannerHead}</div>
      ${bannerLine ? `<div class="line">${bannerLine}</div>` : ""}
      ${bannerAction ? `<div class="action">→ ${bannerAction}</div>` : ""}
    </div>
  `;

  const tbody = $("#res-tbody");
  tbody.innerHTML = "";
  filtered.forEach((r, i) => {
    const p = r.p;
    const patchable = isActive(p) && (p.cpuRequest > 0 || p.memRequest > 0);
    const actionCell = patchable
      ? `<td class="act"><button class="patch-btn" data-res="${i}">patch</button></td>`
      : `<td class="act"><span class="dim">—</span></td>`;
    const tr = document.createElement("tr");
    tr.innerHTML = `
      <td>${p.namespace}</td>
      <td title="${p.name}">${p.name}</td>
      <td class="num">${fmtCpu(p.cpuRequest)}</td>
      <td class="num">${fmtCpu(p.cpuLimit)}</td>
      <td class="num">${fmtCpu(p.rawCpu)}</td>
      <td class="num">${r.cpuP !== null ? r.cpuP.toFixed(0) + "%" : "-"}</td>
      <td class="num">${fmtBytes(p.memRequest)}</td>
      <td class="num">${fmtBytes(p.memLimit)}</td>
      <td class="num">${fmtBytes(p.rawMem)}</td>
      <td class="num">${r.memP !== null ? r.memP.toFixed(0) + "%" : "-"}</td>
      <td class="vrd"><span class="${r.vc}">${r.v}</span></td>
      ${actionCell}
    `;
    tbody.appendChild(tr);
  });
  // wire the per-row patch buttons
  $$("button.patch-btn[data-res]", tbody).forEach(btn => {
    btn.addEventListener("click", (e) => {
      e.stopPropagation();
      const idx = parseInt(btn.dataset.res, 10);
      const r = filtered[idx];
      if (r) openPatchModal(r.p);
    });
  });
}

// ---------- finops ----------
function binPackNodes(podsArr, nodeCpu, nodeMem, podLimit) {
  if (nodeCpu <= 0 || nodeMem <= 0) return 0;
  const items = podsArr.slice().sort((a, b) =>
    Math.max(b.cpu / nodeCpu, b.mem / nodeMem) - Math.max(a.cpu / nodeCpu, a.mem / nodeMem));
  const nodes = [];
  for (const it of items) {
    let placed = false;
    for (const n of nodes) {
      if (n.cpu + it.cpu <= nodeCpu && n.mem + it.mem <= nodeMem && n.count < podLimit) {
        n.cpu += it.cpu; n.mem += it.mem; n.count++;
        placed = true; break;
      }
    }
    if (!placed) nodes.push({ cpu: it.cpu, mem: it.mem, count: 1 });
  }
  return nodes.length;
}

function renderFinOps() {
  renderProjection();
  renderCandidates();
}

function renderProjection() {
  const el = $("#finops-projection");
  if (!el) return;
  if (!state.nodes.length || !state.pods.length) {
    el.innerHTML = `<div class="placeholder">waiting for node/pod data…</div>`;
    return;
  }
  let avgCpu = 0, avgMem = 0, podLimit = 110;
  let totalCpuCap = 0, totalMemCap = 0, totalCpuUse = 0, totalMemUse = 0;
  for (const n of state.nodes) {
    avgCpu += n.cpuCap; avgMem += n.memCap;
    podLimit = n.podLimit || podLimit;
    totalCpuCap += n.cpuCap; totalMemCap += n.memCap;
    totalCpuUse += n.cpuUsed; totalMemUse += n.memUsed;
  }
  avgCpu = Math.floor(avgCpu / state.nodes.length);
  avgMem = Math.floor(avgMem / state.nodes.length);
  const headroom = 1 + state.headroom / 100;

  // Only ACTIVE pods consume scheduler capacity — Completed/Failed pods don't
  // factor into bin-pack at all (their containers have exited).
  const activePods = state.pods.filter(isActive);
  const podsForPack = activePods.map(p => ({
    cpu: Math.ceil(Math.max(p.cpuRequest, p.rawCpu) * headroom),
    mem: Math.ceil(Math.max(p.memRequest, p.rawMem) * headroom),
    rightSized: {
      cpu: Math.ceil(Math.max(p.rawCpu * 1.2, 10) * headroom),
      mem: Math.ceil(Math.max(p.rawMem * 1.2, 64 * 1024 * 1024) * headroom),
    },
  }));
  const current = state.nodes.length;
  const today = binPackNodes(podsForPack, avgCpu, avgMem, podLimit);
  const rs = binPackNodes(podsForPack.map(p => p.rightSized), avgCpu, avgMem, podLimit);

  // identify binding constraint for each scenario
  const constraint = (pods, nodeCpu, nodeMem) => {
    const sumCpu = pods.reduce((s, p) => s + p.cpu, 0);
    const sumMem = pods.reduce((s, p) => s + p.mem, 0);
    const cpuLB = Math.ceil(sumCpu / nodeCpu);
    const memLB = Math.ceil(sumMem / nodeMem);
    const podLB = Math.ceil(pods.length / podLimit);
    const max = Math.max(cpuLB, memLB, podLB);
    const names = [];
    if (podLB === max) names.push("pod-count");
    if (cpuLB === max) names.push("cpu");
    if (memLB === max) names.push("memory");
    return { name: names.join(" + "), lb: max, cpuLB, memLB, podLB };
  };
  const cToday = constraint(podsForPack, avgCpu, avgMem);
  const cRs = constraint(podsForPack.map(p => p.rightSized), avgCpu, avgMem);

  const peak = Math.max(current, today, rs, 1);
  const klass = (v) => v > current ? "hot" : v < current ? "ok" : "warn";
  const row = (label, val, kc, note) => {
    const w = (val / peak) * 100;
    return `<div class="proj-row">
      <span class="lbl">${label}</span>
      <span class="track"><span class="fill ${kc}" style="width:${w.toFixed(1)}%"></span></span>
      <span class="val">${val}</span>
      ${note ? `<span class="note dim">${note}</span>` : ""}
    </div>`;
  };

  // actual cluster utilization (from metrics-server)
  const cpuPct = totalCpuCap > 0 ? (totalCpuUse / totalCpuCap) * 100 : 0;
  const memPct = totalMemCap > 0 ? (totalMemUse / totalMemCap) * 100 : 0;
  const utilCls = p => p > 80 ? "hot" : p > 50 ? "warn" : "ok";
  const utilRow = (label, p, used, cap, fmt) => `<div class="proj-row">
    <span class="lbl">${label}</span>
    <span class="track"><span class="fill ${utilCls(p)}" style="width:${Math.min(p,100).toFixed(1)}%"></span></span>
    <span class="val">${p.toFixed(0)}%</span>
    <span class="note dim">${fmt(used)} of ${fmt(cap)}</span>
  </div>`;

  // verdict — grounded in real usage AND bin-pack outcome
  let verdictHtml;
  const lowUtil = cpuPct < 30 && memPct < 60;
  if (rs < current) {
    verdictHtml = `<div class="verdict-line ok">can shrink by ${current - rs} node${current - rs === 1 ? "" : "s"} after right-sizing (≈ ${Math.round((1 - rs / current) * 100)}% reduction). binding: ${cRs.name}.</div>`;
  } else if (lowUtil && cToday.name.includes("pod-count")) {
    // cluster is idle but pod-count limit prevents consolidation
    const couldFit = Math.max(
      Math.ceil(podsForPack.reduce((s, p) => s + p.cpu, 0) / avgCpu),
      Math.ceil(podsForPack.reduce((s, p) => s + p.mem, 0) / avgMem),
      1);
    verdictHtml = `<div class="verdict-line warn">cluster under-utilized (cpu ${cpuPct.toFixed(0)}%, mem ${memPct.toFixed(0)}%) but bin-pack needs ${today} nodes because of the <b>pod-per-node limit (${podLimit})</b>. raise <code>--max-pods</code> on kubelet (or use larger nodes) and you could fit in ${couldFit} node${couldFit === 1 ? "" : "s"} by resources alone.</div>`;
  } else if (lowUtil) {
    verdictHtml = `<div class="verdict-line ok">cluster under-utilized: cpu ${cpuPct.toFixed(0)}%, mem ${memPct.toFixed(0)}%. likely room to shrink — review the right-sizing candidates below.</div>`;
  } else if (rs > current) {
    const over = rs - current;
    verdictHtml = `<div class="verdict-line hot">overcommitted: bin-pack needs ${over} more node${over === 1 ? "" : "s"} than provisioned. binding: ${cToday.name}. pods may be unschedulable under load.</div>`;
  } else {
    verdictHtml = `<div class="verdict-line warn">right-sized — no node-count change projected. binding: ${cRs.name}.</div>`;
  }

  el.innerHTML = `
    <div class="proj-section-head">actual utilization (live)</div>
    <div class="proj">
      ${utilRow("cpu", cpuPct, totalCpuUse, totalCpuCap, fmtCpu)}
      ${utilRow("memory", memPct, totalMemUse, totalMemCap, fmtBytes)}
      ${utilRow("pods", (activePods.length / (current * podLimit)) * 100, activePods.length, current * podLimit, n => n.toString())}
    </div>
    <div class="proj-section-head">bin-pack projection</div>
    <div class="proj">
      ${row("current nodes", current, "warn", "")}
      ${row("today (max req/live)", today, klass(today), `binding: ${cToday.name}`)}
      ${row("after right-size", rs, klass(rs), `binding: ${cRs.name}`)}
    </div>
    ${verdictHtml}
    <div class="proj-meta">
      <span><span class="k">node template</span>${fmtCpu(avgCpu)} cpu · ${fmtBytes(avgMem)} mem · ${podLimit} pod cap</span>
      <span><span class="k">pods</span>${activePods.length} active <span class="dim">(${state.pods.length - activePods.length} completed excluded)</span></span>
      <span><span class="k">method</span>first-fit-decreasing of max(request, live) × ${state.headroom}% headroom</span>
    </div>
  `;
}

function renderCandidates() {
  const el = $("#finops-candidates");
  if (!el) return;
  const nsFilter = state.candNs;
  const inNs = (p) => nsFilter === "ALL" || p.namespace === nsFilter;
  // count completed/failed pods so we can mention them but exclude them
  // from the reclaimable math — they don't actually hold cluster capacity.
  const completed = state.pods.filter(p => inNs(p) && !isActive(p)).length;
  // a pod is a downsize candidate if it uses < 30% of its CPU or memory request
  const rows = [];
  for (const p of state.pods) {
    if (!inNs(p)) continue;
    if (!isActive(p)) continue;
    if (p.cpuRequest === 0 && p.memRequest === 0) continue;
    const cpuP = p.cpuRequest > 0 ? (p.rawCpu / p.cpuRequest) * 100 : null;
    const memP = p.memRequest > 0 ? (p.rawMem / p.memRequest) * 100 : null;
    const cpuWaste = (cpuP !== null && cpuP < 30) ? p.cpuRequest - p.rawCpu : 0;
    const memWaste = (memP !== null && memP < 30) ? p.memRequest - p.rawMem : 0;
    if (cpuWaste === 0 && memWaste === 0) continue;
    rows.push({ p, cpuP, memP, cpuWaste, memWaste });
  }
  rows.sort((a, b) => {
    // rank by combined waste fraction (memory weighted same as cpu)
    const aw = (a.cpuWaste / 1000) + (a.memWaste / (1024 * 1024 * 1024));
    const bw = (b.cpuWaste / 1000) + (b.memWaste / (1024 * 1024 * 1024));
    return bw - aw;
  });
  const top = rows.slice(0, 15);
  const metaParts = [`${rows.length} active pod${rows.length === 1 ? "" : "s"} below 30% of request`];
  if (completed > 0) metaParts.push(`${completed} completed/failed pod${completed === 1 ? "" : "s"} excluded (no live capacity held)`);
  $("#candidates-meta").textContent = metaParts.join(" · ");
  if (!top.length) {
    el.innerHTML = `<div class="placeholder">no significant right-sizing candidates — pods are within 30% of their requests.</div>`;
    return;
  }
  let totalCpuSave = 0, totalMemSave = 0;
  for (const r of rows) { totalCpuSave += r.cpuWaste; totalMemSave += r.memWaste; }
  const body = top.map((r, i) => `<tr>
    <td>${r.p.namespace}</td>
    <td title="${r.p.name}">${r.p.name}</td>
    <td class="num">${fmtCpu(r.p.cpuRequest)}</td>
    <td class="num">${fmtCpu(r.p.rawCpu)}</td>
    <td class="num">${r.cpuP !== null ? r.cpuP.toFixed(0) + "%" : "-"}</td>
    <td class="num">${fmtBytes(r.p.memRequest)}</td>
    <td class="num">${fmtBytes(r.p.rawMem)}</td>
    <td class="num">${r.memP !== null ? r.memP.toFixed(0) + "%" : "-"}</td>
    <td class="num save">${r.cpuWaste > 0 ? fmtCpu(r.cpuWaste) : "-"} / ${r.memWaste > 0 ? fmtBytes(r.memWaste) : "-"}</td>
    <td class="act"><button class="patch-btn" data-cand="${i}">patch</button></td>
  </tr>`).join("");
  el.innerHTML = `
    <div class="candidates">
      <table>
        <thead><tr>
          <th>NAMESPACE</th><th>POD</th>
          <th class="num">CPU REQ</th><th class="num">CPU USE</th><th class="num">CPU%</th>
          <th class="num">MEM REQ</th><th class="num">MEM USE</th><th class="num">MEM%</th>
          <th class="num">RECLAIMABLE</th>
          <th class="act"></th>
        </tr></thead>
        <tbody>${body}</tbody>
      </table>
    </div>
    <div class="proj-meta">
      <span><span class="k">cluster total reclaimable</span>${fmtCpu(totalCpuSave)} cpu · ${fmtBytes(totalMemSave)} mem</span>
      <span><span class="k">threshold</span>< 30% of request</span>
    </div>
  `;
  // wire up per-row patch buttons
  $$(".patch-btn", el).forEach(btn => {
    btn.addEventListener("click", () => {
      const idx = parseInt(btn.dataset.cand, 10);
      const r = top[idx];
      if (r) openPatchModal(r.p);
    });
  });
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
  try {
    state.costs = await jget("/api/costs");
    renderCosts();
  } catch (e) {
    $("#cost-chart").innerHTML = `<div class="err">${e.message}</div>`;
    $("#costs-meta").textContent = "error";
  } finally {
    state.costsLoading = false;
  }
}

function renderCostPlaceholder() {
  if (state.costs) return;
  $("#cost-chart").innerHTML = `
    <div class="placeholder">
      no cost data loaded
      <div class="hint">click <b>load costs</b> to query AWS Cost Explorer (≈ $0.01/call, requires <code>aws</code> CLI on PATH)</div>
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
    state.currentNs = "ALL";
    state.resNs = "ALL";
    state.candNs = "ALL";
    state.selected = -1;
    state.costs = null;
    state.gemini = { history: [], busy: false, pod: null, provider: state.gemini.provider || "gemini" };
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
      else if (e.key === "3") switchView("resources");
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
      case "3": switchView("resources"); break;
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

  $("#ns-select").addEventListener("change", e => { state.currentNs = e.target.value; renderPodTable(); });
  $("#res-ns-select").addEventListener("change", e => { state.resNs = e.target.value; renderResources(); });
  $("#res-search").addEventListener("input", e => { state.resSearch = e.target.value; renderResources(); });
  $("#cand-ns-select").addEventListener("change", e => { state.candNs = e.target.value; renderCandidates(); });
  $("#search").addEventListener("input", e => { state.search = e.target.value; renderPodTable(); });
  $("#issues-only").addEventListener("change", e => { state.issuesOnly = e.target.checked; renderPodTable(); });
  $("#hide-cron").addEventListener("change", e => { state.hideCron = e.target.checked; renderPodTable(); });
  $("#res-issues-only").addEventListener("change", e => { state.resIssuesOnly = e.target.checked; renderResources(); });
  $$(".sort").forEach(b => b.addEventListener("click", () => { state.sort = b.dataset.sort; renderPodTable(); }));
  $("#res-sort-name").addEventListener("click", () => { state.resSort = "name"; renderResources(); });
  $("#res-sort-cpu").addEventListener("click", () => { state.resSort = "cpu"; renderResources(); });
  $("#res-sort-mem").addEventListener("click", () => { state.resSort = "mem"; renderResources(); });
  $("#res-sort-verdict").addEventListener("click", () => { state.resSort = "verdict"; renderResources(); });
  $("#headroom").addEventListener("change", e => {
    state.headroom = parseInt(e.target.value, 10) || 0;
    renderFinOps();
  });
  $("#costs-load").addEventListener("click", loadCosts);

  $("#gemini-send").addEventListener("click", sendGemini);
  $("#gemini-input").addEventListener("keydown", e => { if (e.key === "Enter") sendGemini(); });
  $("#gemini-clear").addEventListener("click", () => { state.gemini.history = []; state.gemini.pod = null; $("#gemini-err").textContent = ""; renderGemini(); });
  $("#gemini-provider").addEventListener("change", e => {
    state.gemini.provider = e.target.value;
    setMsg(`chat provider: ${state.gemini.provider}`);
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

  bindKeys();
  switchView("pods");
  renderCostPlaceholder();
  loadContexts();
  refresh();
  refreshPortForwards();
  setInterval(refresh, 5000);
  setInterval(refreshPortForwards, 4000);
}
init();
