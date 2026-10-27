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

const appBody = `{{define "body"}}<div class="admin-shell">
<aside class="admin-nav">
  <div class="an-brand">` + logoSVG + `<span class="wordmark">Clusterheart</span></div>
  <nav class="an-list">
    <a class="nav-item active" href="/app">Clusters</a>
    <a class="nav-item" href="/account">Account</a>
  </nav>
  <div class="an-foot"><div class="an-me">{{.Email}}</div><a href="#" id="logout" class="an-logout">Log out</a></div>
</aside>
<main class="admin-main">
  <p class="kicker">Your clusters</p>
  <h1 style="margin:0 0 7px">Clusters</h1>
  <p class="sub">Live health from the read-only agent running in each cluster. Add a cluster to get its one-line install command - your credentials never leave the cluster.</p>
  <div style="margin:20px 0"><button class="act approve" id="add-cluster">+ Add cluster</button></div>
  <div id="clusters"></div>
</main>
</div>{{end}}
{{define "script"}}<script>
var _lo=document.getElementById('logout');
if(_lo)_lo.addEventListener('click',async function(e){e.preventDefault();await _post('/api/logout',{});location.href='/login';});

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
      html+='<tr><td><div class="u-name">'+esc(c.name||c.clusterId)+'</div><div class="u-email">'+esc(c.clusterId||'')+env+'</div></td>'+
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
  var pre='<section class="card2"><div class="card2-head"><p class="card2-title">Install the agent</p><p class="card2-desc">Run this against the cluster you want to watch. The agent is read-only and streams health here. Keep the token secret.</p></div><div class="card2-body">'+
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

load();
</script>{{end}}`

