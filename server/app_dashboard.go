package main

// Browser dashboard (BFF). Logged-in users manage and view their own clusters
// from any device. The browser only talks to this account server (same origin,
// session cookie); this server calls the SRE control plane with the shared
// admin token, scoped to the user's tenant. No kubeconfig ever touches us - a
// read-only agent runs inside the user's cluster and streams health upward.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// tenantFor derives a stable, per-account tenant id from the email. Deterministic
// so no schema migration is needed; the control plane isolates all cluster data
// by this id.
func tenantFor(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return "ten_" + hex.EncodeToString(sum[:])[:16]
}

// cpDo calls the control plane with the admin bearer token. path includes the
// leading /api/v1/... The caller is responsible for tenant scoping.
func (a *app) cpDo(method, path string, body any) (int, []byte, error) {
	base := strings.TrimRight(os.Getenv("SRE_CONTROLPLANE_URL"), "/")
	if base == "" {
		return 0, nil, fmt.Errorf("SRE_CONTROLPLANE_URL not set")
	}
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("SRE_ADMIN_TOKEN"))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 12 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

// handleAppClusters dispatches GET (list my clusters) and POST (register one).
func (a *app) handleAppClusters(w http.ResponseWriter, r *http.Request) {
	u := a.sessionUser(r)
	if u == nil || !u.Approved {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}
	tenant := tenantFor(u.Email)

	switch r.Method {
	case http.MethodGet:
		code, data, err := a.cpDo("GET", "/api/v1/clusters?tenantId="+url.QueryEscape(tenant), nil)
		if err != nil {
			writeJSON(w, 502, map[string]any{"error": "control plane unreachable"})
			return
		}
		if code != 200 {
			writeJSON(w, 502, map[string]any{"error": "could not list clusters"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(data)

	case http.MethodPost:
		var in struct {
			Name        string `json:"name"`
			Environment string `json:"environment"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in)
		name := strings.TrimSpace(in.Name)
		if name == "" {
			name = "my-cluster"
		}
		env := strings.TrimSpace(in.Environment)
		if env == "" {
			env = "production"
		}
		code, data, err := a.cpDo("POST", "/api/v1/clusters", map[string]any{
			"tenantId":    tenant,
			"clusterName": name,
			"environment": env,
		})
		if err != nil {
			writeJSON(w, 502, map[string]any{"error": "control plane unreachable"})
			return
		}
		if code < 200 || code >= 300 {
			writeJSON(w, 502, map[string]any{"error": "could not register cluster"})
			return
		}
		var reg struct {
			ClusterID  string `json:"clusterId"`
			AgentToken string `json:"agentToken"`
		}
		if err := json.Unmarshal(data, &reg); err != nil || reg.ClusterID == "" {
			writeJSON(w, 502, map[string]any{"error": "bad control plane response"})
			return
		}
		writeJSON(w, 200, map[string]any{
			"clusterId":  reg.ClusterID,
			"agentToken": reg.AgentToken,
			"install":    agentInstallSnippet(reg.ClusterID, reg.AgentToken),
		})

	default:
		writeJSON(w, 405, map[string]any{"error": "method not allowed"})
	}
}

// handleAppClusterConnect re-issues a FRESH agent token for one of the user's
// clusters and returns the install snippet. This is how you recover a token you
// never saved (it is shown only once). Scoped to the user's tenant.
func (a *app) handleAppClusterConnect(w http.ResponseWriter, r *http.Request) {
	u := a.sessionUser(r)
	if u == nil || !u.Approved {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, 400, map[string]any{"error": "missing cluster id"})
		return
	}
	tenant := tenantFor(u.Email)
	code, data, err := a.cpDo("POST", "/api/v1/clusters/"+url.PathEscape(id)+"/agent-token?tenantId="+url.QueryEscape(tenant), nil)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "control plane unreachable"})
		return
	}
	if code == 404 {
		writeJSON(w, 404, map[string]any{"error": "cluster not found"})
		return
	}
	if code < 200 || code >= 300 {
		writeJSON(w, 502, map[string]any{"error": "could not issue token"})
		return
	}
	var reg struct {
		ClusterID  string `json:"clusterId"`
		AgentToken string `json:"agentToken"`
	}
	if err := json.Unmarshal(data, &reg); err != nil || reg.AgentToken == "" {
		writeJSON(w, 502, map[string]any{"error": "bad control plane response"})
		return
	}
	writeJSON(w, 200, map[string]any{
		"clusterId":  reg.ClusterID,
		"agentToken": reg.AgentToken,
		"install":    agentInstallSnippet(reg.ClusterID, reg.AgentToken),
	})
}

// agentInstallSnippet builds the copy-paste kubectl commands that enroll the
// read-only agent in the user's cluster. The per-cluster secrets go into a k8s
// Secret; the manifest itself is generic and hosted publicly.
func agentInstallSnippet(clusterID, token string) string {
	cpURL := envOr("AGENT_CONTROLPLANE_URL", "https://clusterheart-server.clusterheart.com/cp")
	manifest := envOr("AGENT_MANIFEST_URL", "https://clusterheart.com/agent.yaml")
	return "kubectl create namespace clusterheart-sre\n" +
		"kubectl -n clusterheart-sre create secret generic clusterheart-agent \\\n" +
		"  --from-literal=CONTROLPLANE_URL=" + cpURL + " \\\n" +
		"  --from-literal=CLUSTER_ID=" + clusterID + " \\\n" +
		"  --from-literal=AGENT_TOKEN=" + token + "\n" +
		"kubectl apply -f " + manifest
}

// pageApp renders the browser dashboard shell; the list is filled client-side.
func (a *app) pageApp(w http.ResponseWriter, r *http.Request) {
	u := a.sessionUser(r)
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !u.Approved {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	render(w, appTmpl, map[string]any{"Name": u.Name, "Email": u.Email})
}

var appTmpl = page(appBody)

const appBody = `{{define "body"}}<div class="admin-shell" id="app-shell">
<aside class="admin-nav">
  <div class="an-brand"><button class="nav-burger" data-nav-toggle aria-label="Hide menu" title="Hide menu">&#9776;</button>` + logoSVG + `<span class="wordmark">Clusterheart</span></div>
  <nav class="an-list">
    <a class="nav-item active" href="/app">Clusters</a>
    <a class="nav-item" href="/account">Account</a>
  </nav>
  <div class="an-foot"><div class="an-me">{{.Email}}</div><a href="#" id="logout" class="an-logout">Log out</a></div>
</aside>
<main class="admin-main">
  <button id="nav-reopen" class="nav-burger" data-nav-toggle aria-label="Show menu" title="Show menu">&#9776;</button>
  <div id="list-head">
    <p class="kicker">Your clusters</p>
    <h1 style="margin:0 0 7px">Clusters</h1>
    <p class="sub">Live health from the read-only agent running in each cluster. Add a cluster to get its one-line install command - your credentials never leave the cluster.</p>
    <div style="margin:20px 0;display:flex;align-items:center;gap:16px;flex-wrap:wrap"><button class="act approve" id="add-cluster">+ Add cluster</button><a href="/docs/connect-a-cluster.md" target="_blank" rel="noopener" style="color:var(--accent);font-size:13.5px;font-weight:600">How to connect a cluster &rarr;</a></div>
  </div>
  <div id="clusters"></div>
</main>
</div>{{end}}
{{define "script"}}<script>
var _lo=document.getElementById('logout');
if(_lo)_lo.addEventListener('click',async function(e){e.preventDefault();await _post('/api/logout',{});location.href='/login';});

var _sh=document.getElementById('app-shell');
if(_sh&&localStorage.getItem('ch-nav')==='hidden')_sh.classList.add('nav-hidden');
document.querySelectorAll('[data-nav-toggle]').forEach(function(b){b.addEventListener('click',function(){var h=_sh.classList.toggle('nav-hidden');localStorage.setItem('ch-nav',h?'hidden':'shown');});});

function esc(s){return (s||'').replace(/[&<>"]/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];});}

async function load(){
  var box=document.getElementById('clusters');
  box.innerHTML='<p class="sub">Loading…</p>';
  try{
    var r=await fetch('/api/app/clusters',{headers:{'Accept':'application/json'}});
    if(!r.ok){box.innerHTML='<p class="sub">Could not load clusters.</p>';return;}
    var payload=await r.json();
    var list=(payload&&payload.clusters)||[];
    if(list.length===0){
      box.innerHTML='<section class="card2"><div class="card2-body"><p class="sub" style="margin:0">No clusters yet. Click <b>Add cluster</b> to connect your first one.</p></div></section>';
      return;
    }
    var html='<section class="card2"><table class="users"><thead><tr><th>Cluster</th><th>Status</th><th>Nodes</th><th>Pods</th><th>Last seen</th></tr></thead><tbody>';
    list.forEach(function(c){
      var connected=c.connected===true;
      var n=c.latest&&c.latest.nodes, p=c.latest&&c.latest.pods;
      var nodes=n?(n.ready+' / '+n.total):'-';
      var pods=p?(p.running+' / '+p.total):'-';
      var seen=c.lastHeartbeat?new Date(c.lastHeartbeat).toLocaleString():'never';
      var env=c.environment?' · '+esc(c.environment):'';
      var link='/app?cluster='+encodeURIComponent(c.clusterId||'');
      html+='<tr style="cursor:pointer" onclick="location.href=\''+link+'\'"><td><div class="u-name"><a href="'+link+'" style="color:var(--accent)">'+esc(c.name||c.clusterId)+'</a></div><div class="u-email">'+esc(c.clusterId||'')+env+'</div></td>'+
        '<td><span class="pill '+(connected?'on':'off')+'"><span class="dot"></span>'+(connected?'Connected':'Offline')+'</span></td>'+
        '<td class="u-time">'+nodes+'</td>'+
        '<td class="u-time">'+pods+'</td>'+
        '<td class="u-time">'+seen+'</td></tr>';
    });
    html+='</tbody></table></section>';
    box.innerHTML=html;
  }catch(e){box.innerHTML='<p class="sub">Could not load clusters.</p>';}
}

function showInstall(d){
  var box=document.getElementById('clusters');
  var pre='<section class="card2"><div class="card2-head"><p class="card2-title">Install the agent</p><p class="card2-desc">Run this against the cluster you want to watch. The agent is read-only. <b>Save the AGENT_TOKEN now. It is shown only once.</b> <a href="/docs/connect-a-cluster.md" target="_blank" rel="noopener" style="color:var(--accent)">Full guide &rarr;</a></p></div><div class="card2-body">'+
    '<textarea class="nb" rows="6" readonly onclick="this.select()">'+esc(d.install)+'</textarea>'+
    '<div class="nb-actions"><button class="act approve" id="copy-install">Copy</button><span class="nb-msg">Cluster ID: '+esc(d.clusterId)+'</span></div>'+
    '</div></section>';
  box.insertAdjacentHTML('afterbegin',pre);
  var cp=document.getElementById('copy-install');
  cp.addEventListener('click',function(){navigator.clipboard.writeText(d.install);cp.textContent='Copied';});
}

document.getElementById('add-cluster').addEventListener('click',async function(){
  var name=prompt('Name this cluster (e.g. prod-us-east):','my-cluster');
  if(name===null)return;
  var btn=this;btn.disabled=true;btn.textContent='Creating…';
  var r=await _post('/api/app/clusters',{name:name});
  btn.disabled=false;btn.textContent='+ Add cluster';
  if(!r.ok){alert((r.data&&r.data.error)||'Failed to create cluster');return;}
  showInstall(r.data);load();
});

function stat(label,val,sub){return '<div style="flex:1 1 230px;min-width:230px;padding:16px 18px;background:var(--surface2);border:1px solid var(--border)"><div style="font-size:12px;color:var(--faint)">'+label+'</div><div style="font-size:24px;font-weight:700;color:var(--text);margin-top:4px">'+val+'</div>'+(sub?'<div class="u-email" style="margin-top:3px;white-space:nowrap">'+sub+'</div>':'')+'</div>';}

async function loadDetail(id){
  var box=document.getElementById('clusters');
  box.innerHTML='<p class="sub">Loading…</p>';
  try{
    var r=await fetch('/api/app/clusters',{headers:{'Accept':'application/json'}});
    var payload=await r.json();
    var list=(payload&&payload.clusters)||[];
    var c=null;list.forEach(function(x){if(x.clusterId===id)c=x;});
    if(!c){box.innerHTML='<a href="/app" style="color:var(--muted)">&larr; All clusters</a><p class="sub" style="margin-top:14px">Cluster not found.</p>';return;}
    var connected=c.connected===true;
    var env=c.environment?esc(c.environment):'';
    var html='<a href="/app" style="color:var(--muted);text-decoration:none">&larr; All clusters</a>';
    html+='<h1 style="margin:14px 0 6px">'+esc(c.name||c.clusterId)+'</h1>';
    html+='<p class="sub">'+esc(c.clusterId)+(env?' · '+env:'')+' · <span class="pill '+(connected?'on':'off')+'"><span class="dot"></span>'+(connected?'Connected':'Offline')+'</span>'+(c.k8sVersion?' · k8s '+esc(c.k8sVersion):'')+(c.agentVersion?' · agent '+esc(c.agentVersion):'')+'</p>';
    html+='<div style="margin:14px 0 4px"><button class="act approve" id="connect-btn" data-id="'+esc(c.clusterId)+'">Get install command</button> <a href="/docs/connect-a-cluster.md" target="_blank" rel="noopener" style="color:var(--accent);font-size:13px;margin-left:8px">guide &rarr;</a></div>';
    var L=c.latest;
    if(!L){html+='<section class="card2"><div class="card2-body"><p class="sub" style="margin:0">No data yet. Once the agent reports (every ~30s), health shows up here. Make sure the agent pod is Running in <b>clusterheart-sre</b>.</p></div></section>';box.innerHTML=html;return;}
    var n=L.nodes||{},p=L.pods||{},wl=L.workloads||{};
    html+='<div style="display:flex;gap:12px;flex-wrap:wrap;margin:18px 0">'+
      stat('Nodes ready',(n.ready||0)+' / '+(n.total||0),(n.notReady?n.notReady+' not ready':'all ready'))+
      stat('Pods running',(p.running||0)+' / '+(p.total||0),(p.pending||0)+' pending · '+(p.failed||0)+' failed · '+(p.succeeded||0)+' done')+
      stat('Workloads',(wl.deployments||0)+' deploy',(wl.daemonSets||0)+' ds · '+(wl.statefulSets||0)+' sts · '+(wl.jobs||0)+' jobs')+
      stat('Restart pressure',(p.highRestart||0),'pods restarting')+
    '</div>';
    var all=L.podList||[];
    var hasMetrics=all.some(function(x){return x.cpuMilli||x.memMi;});
    function isJob(x){return (x.owner||'').indexOf('Job/')===0;}
    function byCpu(a,b){return (b.cpuMilli||0)-(a.cpuMilli||0);}
    var svc=all.filter(function(x){return !isJob(x);}).sort(byCpu);
    var jobs=all.filter(isJob).sort(byCpu);
    function rowK(x,kind){
      var ok=(x.phase==='Running'||x.phase==='Succeeded');
      return '<tr data-kind="'+kind+'" data-ns="'+esc(x.namespace)+'" data-name="'+esc((x.name||'').toLowerCase())+'"><td><div class="u-name" style="font-size:13.5px">'+esc(x.name)+'</div><div class="u-email">'+esc(x.namespace)+'</div></td>'+
        '<td><span class="pill '+(ok?'on':'off')+'"><span class="dot"></span>'+esc(x.phase||'')+'</span></td>'+
        '<td class="u-time">'+esc(x.ready||'')+'</td><td class="u-time">'+(x.restarts||0)+'</td>'+
        '<td class="u-time">'+(x.cpuMilli?x.cpuMilli+'m':'-')+'</td><td class="u-time">'+(x.memMi?x.memMi+'Mi':'-')+'</td>'+
        '<td class="u-email">'+esc(x.node||'')+'</td></tr>';
    }
    if(svc.length||jobs.length){
      var nss=Array.from(new Set(all.map(function(x){return x.namespace;}))).sort();
      html+='<section class="card2">'+
        '<div class="card2-head">'+
          '<div style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">'+
            '<div class="seg" id="pj-seg"><button class="seg-btn active" data-kind="pod">Pods ('+svc.length+')</button><button class="seg-btn" data-kind="job">Jobs ('+jobs.length+')</button></div>'+
            '<input id="pod-search" class="toolbar-input" placeholder="Search…" style="flex:1;min-width:130px">'+
            '<select id="pod-ns" class="toolbar-input"><option value="">All namespaces</option>'+nss.map(function(n){return '<option>'+esc(n)+'</option>';}).join('')+'</select>'+
          '</div>'+
          '<p class="card2-desc" style="margin-top:8px">'+(hasMetrics?'Live CPU/memory from metrics-server.':'Install metrics-server for CPU/memory.')+'</p>'+
        '</div>'+
        '<div class="tscroll"><table class="users"><thead><tr><th>Name</th><th>Status</th><th>Ready</th><th>Restarts</th><th>CPU</th><th>Mem</th><th>Node</th></tr></thead><tbody id="pods-body">'+
        svc.map(function(x){return rowK(x,'pod');}).join('')+jobs.map(function(x){return rowK(x,'job');}).join('')+
        '</tbody></table></div>'+
        (L.podListTruncated?'<div class="card2-body"><p class="sub" style="margin:0">+'+L.podListTruncated+' more pods (capped).</p></div>':'')+
      '</section>';
    }
    var probs=[];
    (L.podProblems||[]).forEach(function(x){probs.push({type:x.type,where:x.namespace+'/'+x.pod+(x.container?' ['+x.container+']':''),msg:x.message});});
    (L.workloadProblems||[]).forEach(function(x){probs.push({type:x.type,where:x.namespace+'/'+x.kind+' '+x.name,msg:x.message});});
    if(probs.length){
      html+='<section class="card2"><div class="card2-head"><p class="card2-title">Problems ('+probs.length+')</p><p class="card2-desc">Measured fault signals from the latest snapshot.</p></div><table class="users"><thead><tr><th>Type</th><th>Object</th><th>Detail</th></tr></thead><tbody>';
      probs.forEach(function(x){html+='<tr><td><span class="pill off"><span class="dot"></span>'+esc(x.type)+'</span></td><td class="u-email">'+esc(x.where)+'</td><td class="u-time">'+esc(x.msg||'')+'</td></tr>';});
      html+='</tbody></table></section>';
    }
    var w=L.warnings||[];
    if(w.length){
      html+='<section class="card2" style="margin-top:16px"><div class="card2-head"><p class="card2-title">Recent warnings ('+w.length+')</p></div><table class="users"><thead><tr><th>Reason</th><th>Object</th><th>Count</th><th>Last seen</th></tr></thead><tbody>';
      w.forEach(function(e){html+='<tr><td class="u-name" style="font-size:13.5px">'+esc(e.reason)+'</td><td class="u-email">'+esc(e.namespace)+'/'+esc(e.object)+'</td><td class="u-time">'+(e.count||0)+'</td><td class="u-time">'+(e.lastSeen?new Date(e.lastSeen).toLocaleString():'')+'</td></tr>';});
      html+='</tbody></table></section>';
    }
    if(!probs.length&&!w.length){html+='<section class="card2"><div class="card2-body"><p class="sub" style="margin:0">No problems or warnings in the latest snapshot. Looks healthy.</p></div></section>';}
    box.innerHTML=html;
    var ps=document.getElementById('pod-search'),pn=document.getElementById('pod-ns'),seg=document.getElementById('pj-seg'),_kind='pod';
    function applyPodFilter(){
      var q=((ps&&ps.value)||'').toLowerCase(),ns=(pn&&pn.value)||'';
      document.querySelectorAll('#pods-body tr').forEach(function(tr){
        var okk=tr.dataset.kind===_kind, okn=!ns||tr.dataset.ns===ns, okq=!q||(tr.dataset.name||'').indexOf(q)>=0;
        tr.style.display=(okk&&okn&&okq)?'':'none';
      });
    }
    if(ps)ps.addEventListener('input',applyPodFilter);
    if(pn)pn.addEventListener('change',applyPodFilter);
    if(seg)seg.querySelectorAll('.seg-btn').forEach(function(b){b.addEventListener('click',function(){seg.querySelectorAll('.seg-btn').forEach(function(x){x.classList.remove('active');});b.classList.add('active');_kind=b.dataset.kind;applyPodFilter();});});
    applyPodFilter();
  }catch(e){box.innerHTML='<a href="/app" style="color:var(--muted)">&larr; All clusters</a><p class="sub" style="margin-top:14px">Could not load cluster.</p>';}
}

var _cbox=document.getElementById('clusters');
if(_cbox)_cbox.addEventListener('click',async function(e){
  var cb=e.target.closest&&e.target.closest('#connect-btn');
  if(!cb)return;
  if(!confirm('This issues a NEW agent token and invalidates any previous token for this cluster. Continue?'))return;
  cb.disabled=true;cb.textContent='…';
  var rr=await _post('/api/app/clusters/'+encodeURIComponent(cb.dataset.id)+'/connect',{});
  cb.disabled=false;cb.textContent='Get install command';
  if(!rr.ok){alert((rr.data&&rr.data.error)||'Failed to issue token');return;}
  showInstall(rr.data);window.scrollTo(0,0);
});

function route(){
  var id=new URLSearchParams(location.search).get('cluster');
  var head=document.getElementById('list-head');
  if(id){head.style.display='none';loadDetail(id);}
  else{head.style.display='';load();}
}
route();
</script>{{end}}`

