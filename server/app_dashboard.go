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

// handleAppClusterRename changes a cluster's display name (tenant-scoped).
func (a *app) handleAppClusterRename(w http.ResponseWriter, r *http.Request) {
	u := a.sessionUser(r)
	if u == nil || !u.Approved {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	var in struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in)
	name := strings.TrimSpace(in.Name)
	if id == "" || name == "" {
		writeJSON(w, 400, map[string]any{"error": "id and name are required"})
		return
	}
	tenant := tenantFor(u.Email)
	code, _, err := a.cpDo("POST", "/api/v1/clusters/"+url.PathEscape(id)+"/rename?tenantId="+url.QueryEscape(tenant), map[string]any{"name": name})
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "control plane unreachable"})
		return
	}
	if code == 404 {
		writeJSON(w, 404, map[string]any{"error": "cluster not found"})
		return
	}
	if code < 200 || code >= 300 {
		writeJSON(w, 502, map[string]any{"error": "could not rename cluster"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "name": name})
}

// handleAppClusterSetEnv changes a cluster's environment label (tenant-scoped).
func (a *app) handleAppClusterSetEnv(w http.ResponseWriter, r *http.Request) {
	u := a.sessionUser(r)
	if u == nil || !u.Approved {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	var in struct {
		Environment string `json:"environment"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in)
	env := strings.TrimSpace(in.Environment)
	if id == "" || env == "" {
		writeJSON(w, 400, map[string]any{"error": "id and environment are required"})
		return
	}
	tenant := tenantFor(u.Email)
	code, _, err := a.cpDo("POST", "/api/v1/clusters/"+url.PathEscape(id)+"/environment?tenantId="+url.QueryEscape(tenant), map[string]any{"environment": env})
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "control plane unreachable"})
		return
	}
	if code == 404 {
		writeJSON(w, 404, map[string]any{"error": "cluster not found"})
		return
	}
	if code < 200 || code >= 300 {
		writeJSON(w, 502, map[string]any{"error": "could not update environment"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "environment": env})
}

// handleAppClusterDelete removes one of the user's clusters (tenant-scoped).
func (a *app) handleAppClusterDelete(w http.ResponseWriter, r *http.Request) {
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
	code, _, err := a.cpDo("DELETE", "/api/v1/clusters/"+url.PathEscape(id)+"?tenantId="+url.QueryEscape(tenant), nil)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "control plane unreachable"})
		return
	}
	if code == 404 {
		writeJSON(w, 404, map[string]any{"error": "cluster not found"})
		return
	}
	if code < 200 || code >= 300 {
		writeJSON(w, 502, map[string]any{"error": "could not delete cluster"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleAppClusterUsage returns per-workload peak usage over the last 6h from
// the control plane's stored snapshots (for Rightsizing):
// GET /api/app/clusters/{id}/usage.
func (a *app) handleAppClusterUsage(w http.ResponseWriter, r *http.Request) {
	_, tenant, ok := a.appSREUser(w, r)
	if !ok {
		return
	}
	code, data, err := a.cpDo("GET", "/api/v1/clusters/"+url.PathEscape(r.PathValue("id"))+"/usage?windowMins=360&tenantId="+url.QueryEscape(tenant), nil)
	cpRelay(w, code, data, err)
}

// appSREUser gates the SRE endpoints the same way as the cluster ones and
// returns the caller's tenant.
func (a *app) appSREUser(w http.ResponseWriter, r *http.Request) (*User, string, bool) {
	u := a.sessionUser(r)
	if u == nil || !u.Approved {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return nil, "", false
	}
	return u, tenantFor(u.Email), true
}

// cpRelay passes a control-plane response straight through. Error bodies from
// the control plane are already {"error": "..."} so the browser can show them.
func cpRelay(w http.ResponseWriter, code int, data []byte, err error) {
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "control plane unreachable"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}

// handleAppClusterSRE returns one cluster's SRE view: its incidents and
// remediation proposals (the control plane lists them tenant-wide; we keep only
// this cluster's) plus the automation policy state.
func (a *app) handleAppClusterSRE(w http.ResponseWriter, r *http.Request) {
	_, tenant, ok := a.appSREUser(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	q := "?tenantId=" + url.QueryEscape(tenant)

	var incs struct {
		Incidents []map[string]any `json:"incidents"`
	}
	var props struct {
		Proposals []map[string]any `json:"proposals"`
	}
	var pol struct {
		Mode           string   `json:"mode"`
		Paused         bool     `json:"paused"`
		PausedClusters []string `json:"pausedClusters"`
	}
	for _, c := range []struct {
		path string
		out  any
	}{
		{"/api/v1/incidents" + q, &incs},
		{"/api/v1/proposals" + q, &props},
		{"/api/v1/policy" + q, &pol},
	} {
		code, data, err := a.cpDo("GET", c.path, nil)
		if err != nil {
			writeJSON(w, 502, map[string]any{"error": "control plane unreachable"})
			return
		}
		if code != 200 || json.Unmarshal(data, c.out) != nil {
			writeJSON(w, 502, map[string]any{"error": "could not load SRE data"})
			return
		}
	}
	mine := func(items []map[string]any) []map[string]any {
		out := []map[string]any{}
		for _, it := range items {
			if cid, _ := it["clusterId"].(string); cid == id {
				out = append(out, it)
			}
		}
		return out
	}
	clusterPaused := false
	for _, c := range pol.PausedClusters {
		if c == id {
			clusterPaused = true
		}
	}
	myIncs, myProps := mine(incs.Incidents), mine(props.Proposals)
	writeJSON(w, 200, map[string]any{
		"incidents": myIncs,
		"proposals": myProps,
		"policy":    map[string]any{"mode": pol.Mode, "paused": pol.Paused, "clusterPaused": clusterPaused},
		"activity":  a.clusterActivity(tenant, id, myIncs, myProps),
	})
}

// clusterActivity returns the audit events that concern one cluster. Audit
// events carry no cluster id, so they're matched through this cluster's
// proposals and incidents, plus pause/resume of this cluster and tenant-wide
// policy changes (which apply to every cluster). Best effort: an audit failure
// yields an empty list rather than failing the whole SRE view.
func (a *app) clusterActivity(tenant, id string, incs, props []map[string]any) []map[string]any {
	out := []map[string]any{}
	code, data, err := a.cpDo("GET", "/api/v1/audit?tenantId="+url.QueryEscape(tenant), nil)
	if err != nil || code != 200 {
		return out
	}
	var aud struct {
		Events []map[string]any `json:"events"`
	}
	if json.Unmarshal(data, &aud) != nil {
		return out
	}
	ids := map[string]bool{}
	for _, it := range append(append([]map[string]any{}, incs...), props...) {
		if v, _ := it["id"].(string); v != "" {
			ids[v] = true
		}
	}
	for _, ev := range aud.Events {
		pid, _ := ev["proposalId"].(string)
		iid, _ := ev["incidentId"].(string)
		name, _ := ev["event"].(string)
		det, _ := ev["details"].(string)
		policy := strings.HasPrefix(name, "policy.") && (det == "cluster:"+id || det == "tenant" || name == "policy.updated")
		if ids[pid] || ids[iid] || policy {
			out = append(out, ev)
		}
	}
	return out
}

// handleAppIncidentAction runs AI diagnosis or turns it into a policy-checked
// remediation proposal: POST /api/app/incidents/{id}/{diagnose|propose}.
func (a *app) handleAppIncidentAction(w http.ResponseWriter, r *http.Request) {
	_, tenant, ok := a.appSREUser(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	if action != "diagnose" && action != "propose" {
		writeJSON(w, 404, map[string]any{"error": "not found"})
		return
	}
	code, data, err := a.cpDo("POST", "/api/v1/incidents/"+url.PathEscape(r.PathValue("id"))+"/"+action+"?tenantId="+url.QueryEscape(tenant), nil)
	cpRelay(w, code, data, err)
}

// handleAppProposalAction approves or rejects a remediation proposal:
// POST /api/app/proposals/{id}/{approve|reject}. The approver is always the
// logged-in user, never taken from the request.
func (a *app) handleAppProposalAction(w http.ResponseWriter, r *http.Request) {
	u, tenant, ok := a.appSREUser(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	if action != "approve" && action != "reject" {
		writeJSON(w, 404, map[string]any{"error": "not found"})
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
	body := map[string]any{"approver": "user:" + u.Email}
	if action == "reject" && strings.TrimSpace(in.Reason) != "" {
		body["reason"] = strings.TrimSpace(in.Reason)
	}
	code, data, err := a.cpDo("POST", "/api/v1/proposals/"+url.PathEscape(r.PathValue("id"))+"/"+action+"?tenantId="+url.QueryEscape(tenant), body)
	cpRelay(w, code, data, err)
}

// handleAppClusterAutomation pauses or resumes automated remediation for one
// cluster: POST /api/app/clusters/{id}/automation/{pause|resume}.
func (a *app) handleAppClusterAutomation(w http.ResponseWriter, r *http.Request) {
	_, tenant, ok := a.appSREUser(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	if action != "pause" && action != "resume" {
		writeJSON(w, 404, map[string]any{"error": "not found"})
		return
	}
	code, data, err := a.cpDo("POST", "/api/v1/policy/"+action+"?tenantId="+url.QueryEscape(tenant)+"&clusterId="+url.QueryEscape(r.PathValue("id")), nil)
	cpRelay(w, code, data, err)
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
	render(w, appTmpl, map[string]any{"Name": u.Name, "Email": u.Email, "ManifestURL": envOr("AGENT_MANIFEST_URL", "https://clusterheart.com/agent.yaml")})
}

var appTmpl = page(appBody)

const burgerSVG = `<svg width="18" height="18" viewBox="0 0 18 18" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"><path d="M2.5 5h13M2.5 9h13M2.5 13h13"/></svg>`

const appBody = `{{define "body"}}<div class="app2">
<header class="topbar">
  <a class="tb-brand" href="/app">` + logoSVG + `<span class="wordmark">Clusterheart</span></a>
  <div class="tb-right">
    <div class="menu" id="menu">
      <button class="menu-btn" id="menu-btn" aria-label="Menu" aria-haspopup="true" aria-expanded="false">` + burgerSVG + `</button>
      <div class="menu-pop" role="menu">
        <div class="menu-email">{{.Email}}</div>
        <button class="menu-item accent" role="menuitem" id="add-cluster">+ Add cluster</button>
        <a class="menu-item" role="menuitem" href="/account">Account</a>
        <a class="menu-item" role="menuitem" href="/docs/connect-a-cluster.md" target="_blank" rel="noopener">Help</a>
        <div class="menu-sep"></div>
        <a class="menu-item danger" role="menuitem" href="#" id="logout">Log out</a>
      </div>
    </div>
  </div>
</header>
<main class="app2-main">
  <div id="list-head"></div>
  <div id="clusters"></div>
</main>
</div>{{end}}
{{define "script"}}<script>
var NODE_MANIFEST_URL={{.ManifestURL}};
var _lo=document.getElementById('logout');
if(_lo)_lo.addEventListener('click',async function(e){e.preventDefault();await _post('/api/logout',{});location.href='/login';});

// burger dropdown menu (options)
var _menu=document.getElementById('menu'),_mb=document.getElementById('menu-btn');
if(_mb){
  _mb.addEventListener('click',function(e){e.stopPropagation();var o=_menu.classList.toggle('open');_mb.setAttribute('aria-expanded',o?'true':'false');});
  document.addEventListener('click',function(e){if(_menu.classList.contains('open')&&!_menu.contains(e.target)){_menu.classList.remove('open');_mb.setAttribute('aria-expanded','false');}});
  document.addEventListener('keydown',function(e){if(e.key==='Escape'&&_menu.classList.contains('open')){_menu.classList.remove('open');_mb.setAttribute('aria-expanded','false');}});
}

// three-dot options menus (on cards + detail header): toggle on the button,
// close on any other click or Escape.
document.addEventListener('click',function(e){
  var btn=e.target.closest&&e.target.closest('.kebab-btn');
  var open=document.querySelectorAll('.kebab.open');
  if(btn){var k=btn.closest('.kebab');var was=k.classList.contains('open');open.forEach(function(o){o.classList.remove('open');});if(!was)k.classList.add('open');return;}
  open.forEach(function(o){o.classList.remove('open');});
});
document.addEventListener('keydown',function(e){if(e.key==='Escape')document.querySelectorAll('.kebab.open').forEach(function(o){o.classList.remove('open');});});

function esc(s){return (s||'').replace(/[&<>"]/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];});}
function ago(ts){if(!ts)return 'never';var s=(Date.now()-new Date(ts).getTime())/1000;if(s<0)s=0;if(s<60)return 'just now';if(s<3600)return Math.floor(s/60)+'m ago';if(s<86400)return Math.floor(s/3600)+'h ago';return Math.floor(s/86400)+'d ago';}
function fbadge(nf){return nf>0?'<span class="fail-badge" title="'+nf+' failed">'+nf+' failed</span>':'';}
// dedupeJobs collapses CronJob runs to the latest run per owner (one entry per
// scheduled job); standalone Jobs are kept. Used by both the cards and detail
// so their Job counts match.
function dedupeJobs(list){
  var s=(list||[]).slice().sort(function(a,b){return new Date(b.startedAt||b.finishedAt||0)-new Date(a.startedAt||a.finishedAt||0);});
  var seen={},out=[];
  s.forEach(function(j){if(j.owner){if(seen[j.owner])return;seen[j.owner]=true;}out.push(j);});
  return out;
}
var dotsSVG='<svg width="18" height="18" viewBox="0 0 18 18" fill="currentColor"><circle cx="9" cy="3.6" r="1.5"/><circle cx="9" cy="9" r="1.5"/><circle cx="9" cy="14.4" r="1.5"/></svg>';
// kebab builds the three-dot options menu used on both the cluster cards and
// the detail header. All items are class-based so multiple menus coexist.
function kebab(c){
  var connected=c.connected===true,id=esc(c.clusterId||'');
  var h='<div class="kebab"><button class="kebab-btn" aria-label="Options" aria-haspopup="true">'+dotsSVG+'</button><div class="kebab-pop">';
  h+='<button class="kebab-item info-btn" data-id="'+id+'" data-env="'+esc(c.environment||'')+'" data-k8s="'+esc(c.k8sVersion||'')+'" data-agent="'+esc(c.agentVersion||'')+'" data-seen="'+esc(c.lastHeartbeat||'')+'" data-conn="'+(connected?'Connected':'Offline')+'">Info</button>';
  h+='<button class="kebab-item rename-btn" data-id="'+id+'" data-name="'+esc(c.name||'')+'">Rename</button>';
  h+='<button class="kebab-item env-btn" data-id="'+id+'" data-env="'+esc(c.environment||'')+'">Environment</button>';
  if(!connected){h+='<button class="kebab-item connect-btn" data-id="'+id+'">Install</button>';}
  h+='<div class="kebab-sep"></div>';
  h+='<button class="kebab-item danger del-btn" data-id="'+id+'" data-name="'+esc(c.name||c.clusterId||'')+'">Delete</button>';
  return h+'</div></div>';
}

// Apple-style modal. Resolves to the input value (or true) on confirm, null on cancel.
function chModal(o){return new Promise(function(res){
  var bg=document.createElement('div');bg.className='modal-bg';
  var ph=o.placeholder?' placeholder="'+esc(o.placeholder)+'"':'';
  var extra=o.requireMatch?' autocomplete="off" autocapitalize="off" spellcheck="false"':'';
  var inp=o.input!=null?'<input class="toolbar-input" id="_mi"'+ph+extra+' value="'+esc(o.input)+'">':'';
  var cancelBtn=o.single?'':'<button id="_mc">'+esc(o.cancel||'Cancel')+'</button>';
  bg.innerHTML='<div class="modal"><h3>'+esc(o.title||'')+'</h3>'+(o.body?'<p>'+o.body+'</p>':'')+inp+'<div class="modal-actions">'+cancelBtn+'<button class="'+(o.danger?'danger':'primary')+'" id="_mo">'+esc(o.confirm||'OK')+'</button></div></div>';
  document.body.appendChild(bg);
  var mi=bg.querySelector('#_mi'),mo=bg.querySelector('#_mo');
  function matched(){return !o.requireMatch||(mi&&mi.value.trim()===o.requireMatch);}
  function sync(){if(o.requireMatch)mo.disabled=!matched();}
  if(mi){mi.focus();if(o.input)mi.select();mi.addEventListener('input',sync);mi.addEventListener('keydown',function(e){if(e.key==='Enter'&&matched())mo.click();});}
  sync();
  function done(v){bg.remove();res(v);}
  var mc=bg.querySelector('#_mc');if(mc)mc.onclick=function(){done(null);};
  mo.onclick=function(){if(!matched())return;done(mi?mi.value.trim():true);};
  bg.addEventListener('click',function(e){if(e.target===bg)done(null);});
});}

async function load(){
  var box=document.getElementById('clusters');
  box.innerHTML='<div class="spin-wrap"><div class="spinner"></div></div>';
  try{
    var r=await fetch('/api/app/clusters',{headers:{'Accept':'application/json'}});
    if(!r.ok){box.innerHTML='<p class="sub">Could not load clusters.</p>';return;}
    var payload=await r.json();
    var list=(payload&&payload.clusters)||[];
    if(list.length===0){
      box.innerHTML='<section class="card2"><div class="card2-body"><p class="sub" style="margin:0">No clusters yet. Click <b>Add cluster</b> to connect your first one.</p></div></section>';
      return;
    }
    var html='<div class="cl-grid">';
    list.forEach(function(c){
      var connected=c.connected===true;
      var L=c.latest||{},n=L.nodes,p=L.pods||{},wl=L.workloads||{};
      // Pods show running / total (service pods only); Jobs show completed /
      // total scheduled jobs from the deduped Job objects — same source as the
      // detail view, so the numbers match.
      var all=L.podList||[];
      var svc=all.filter(function(x){return (x.owner||'').indexOf('Job/')!==0;});
      var jl=dedupeJobs(L.jobList);
      var havePL=all.length>0,haveJL=jl.length>0;
      var podRun=svc.filter(function(x){return x.phase==='Running';}).length;
      var podFail=svc.filter(function(x){return x.phase==='Failed';}).length;
      var jobDone=jl.filter(function(x){return x.status==='Complete';}).length;
      var nodes=n?(n.ready+' / '+n.total):'—';
      var pods=havePL?(podRun+' / '+svc.length):(p.running!=null?(p.running+' / '+p.total):'—');
      var jobsN=haveJL?(jobDone+' / '+jl.length):(wl.jobs!=null?(''+wl.jobs):'—');
      var link='/app?cluster='+encodeURIComponent(c.clusterId||'');
      html+='<div class="cl-card">'+
        '<div class="cl-head"><div style="min-width:0"><a class="cl-name" href="'+link+'">'+esc(c.name||c.clusterId)+'</a>'+(c.environment?'<div class="cl-env">'+esc(c.environment)+'</div>':'')+'</div>'+
          '<span class="cl-status '+(connected?'on':'off')+'"><span class="dot"></span>'+(connected?'Connected':'Offline')+'</span></div>'+
        '<div class="cl-metrics"><div class="cl-metric"><b>'+nodes+'</b><span>Nodes</span></div><div class="cl-metric"><b>'+pods+'</b><span>Pods'+fbadge(havePL?podFail:0)+'</span></div><div class="cl-metric"><b>'+jobsN+'</b><span>Jobs</span></div></div>'+
        '<div class="cl-foot"><span class="cl-seen">'+ago(c.lastHeartbeat)+'</span>'+kebab(c)+'</div>'+
      '</div>';
    });
    html+='</div>';
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
  if(_menu){_menu.classList.remove('open');if(_mb)_mb.setAttribute('aria-expanded','false');}
  var name=prompt('Name this cluster (e.g. prod-us-east):','my-cluster');
  if(name===null)return;
  var btn=this;btn.disabled=true;btn.textContent='Creating…';
  var r=await _post('/api/app/clusters',{name:name});
  btn.disabled=false;btn.textContent='+ Add cluster';
  if(!r.ok){alert((r.data&&r.data.error)||'Failed to create cluster');return;}
  showInstall(r.data);load();
});

function stat(label,val,sub){return '<div class="statcard"><div class="stat-label">'+label+'</div><div class="stat-val">'+val+'</div>'+(sub?'<div class="stat-sub">'+sub+'</div>':'')+'</div>';}

function fmtMem(mi){mi=mi||0;return mi>=1024?(mi/1024).toFixed(1)+'Gi':mi+'Mi';}
function fmtCpu(m){m=m||0;return m>=1000?(m/1000).toFixed(m%1000?1:0):(m+'m');}
// utilCell renders a utilization bar (usage vs allocatable) with %, colour-coded,
// or just the capacity text when live metrics aren't available.
function utilCell(usage,alloc,sub,hasM){
  if(hasM&&alloc){
    var pct=Math.round((usage||0)/alloc*100);
    var cls=pct>=90?'crit':(pct>=70?'warn':'ok');
    return '<div class="util-val '+cls+'">'+sub+'</div>';
  }
  return '<div class="util-val cap">'+sub+'</div>';
}
function nodesSection(nl){
  if(!nl||!nl.length)return '';
  var hasM=nl.some(function(n){return n.cpuUsageMilli||n.memUsageMi;});
  var rows=nl.map(function(n){
    var ok=n.ready===true;
    var cpuSub=(hasM?fmtCpu(n.cpuUsageMilli):'—')+' / '+fmtCpu(n.cpuAllocMilli);
    var memSub=(hasM?fmtMem(n.memUsageMi):'—')+' / '+fmtMem(n.memAllocMi);
    var meta=esc(n.roles||'')+(n.kubeletVersion?' · '+esc(n.kubeletVersion):'');
    var podsTxt=(n.pods||0)+(n.podsCapacity?' / '+n.podsCapacity:'');
    return '<tr><td><div class="u-name pod-name" title="'+esc(n.name)+'">'+esc(n.name)+'</div><div class="u-email">'+meta+'</div></td>'+
      '<td><span class="pill '+(ok?'on':'off')+'">'+(ok?'Ready':'NotReady')+'</span></td>'+
      '<td>'+utilCell(n.cpuUsageMilli,n.cpuAllocMilli,cpuSub,hasM)+'</td>'+
      '<td>'+utilCell(n.memUsageMi,n.memAllocMi,memSub,hasM)+'</td>'+
      '<td class="u-time">'+podsTxt+'</td></tr>';
  }).join('');
  var head=hasM?'':'<div class="card2-head"><p class="card2-desc" style="margin:0">Allocatable capacity per node. Run the setup above for live usage.</p></div>';
  var table='<section class="card2">'+head+
    '<div class="tscroll"><table class="users ptable"><thead><tr><th>Node</th><th>Status</th><th>CPU</th><th>Memory</th><th>Pods</th></tr></thead><tbody>'+rows+'</tbody></table></div></section>';
  if(hasM)return table;
  // No live usage yet: surface the exact one-time setup (adds the namespace,
  // the agent + its node-metrics permission, and installs metrics-server).
  var cmds='kubectl apply -f '+NODE_MANIFEST_URL+'\n'+
    'kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml\n'+
    'kubectl -n clusterheart-sre rollout restart deployment clusterheart-agent';
  var setup='<section class="card2"><div class="card2-head"><p class="card2-title">Enable live CPU &amp; memory</p><p class="card2-desc">Live node usage needs metrics-server and the latest agent permissions. Run this once, then reopen this tab.</p></div>'+
    '<div class="card2-body"><textarea class="nb" rows="3" readonly onclick="this.select()">'+esc(cmds)+'</textarea>'+
    '<div class="nb-actions"><button class="act approve copy-setup">Copy</button><span class="nb-msg">Adds the clusterheart-sre namespace &amp; agent, grants node-metrics access, and installs metrics-server.</span></div></div></section>';
  return setup+table;
}

// ---------- AI chat ----------
// Answers come back as bullet lines: [+] green, [~] yellow, [!] red, [-] neutral.
var aiState={cluster:null,history:[],busy:false};
function aiPanelHtml(){
  return '<section class="card2"><div class="card2-body"><div id="ai-log" class="ai-log"></div>'+
    '<form id="ai-form" class="toolbar-row" style="flex-wrap:nowrap"><input id="ai-input" class="toolbar-input" style="flex:1;min-width:0" placeholder="Ask about this cluster" autocomplete="off"><button class="act approve" id="ai-send" type="submit">Ask</button></form>'+
    '</div></section>';
}
function aiFormat(text){
  return (text||'').split('\n').map(function(l){return l.trim();}).filter(Boolean).map(function(l){
    var m=l.match(/^\[([+~!\-])\]\s*(.*)$/),sig=m?m[1]:'-',body=esc(m?m[2]:l).replace(/\x60([^\x60]+)\x60/g,'<code>$1</code>');
    return '<div class="ai-line ai-'+(sig==='+'?'ok':sig==='~'?'warn':sig==='!'?'bad':'neutral')+'">'+body+'</div>';
  }).join('');
}
function aiRender(){
  var log=document.getElementById('ai-log');if(!log)return;
  log.style.marginBottom=aiState.history.length||aiState.busy?'12px':'0';
  var clear=aiState.history.length&&!aiState.busy?'<div class="ai-clear-row"><button type="button" class="ai-clear">Clear</button></div>':'';
  log.innerHTML=clear+aiState.history.map(function(t){
    if(t.role==='user')return '<div class="ai-q">'+esc(t.text)+'</div>';
    if(t.role==='error')return '<div class="ai-a"><div class="ai-line ai-bad">'+esc(t.text)+'</div></div>';
    return '<div class="ai-a">'+aiFormat(t.text)+'</div>';
  }).join('')+(aiState.busy?'<div class="ai-a"><div class="ai-line ai-neutral">Thinking…</div></div>':'');
  log.scrollTop=log.scrollHeight;
}
// aiAsk starts a chat job and polls until the answer is ready (free models can
// take over a minute, longer than one proxied request may stay open).
async function aiAsk(id,hist){
  var r=await _post('/api/app/clusters/'+encodeURIComponent(id)+'/chat',{history:hist});
  if(!r.ok||!r.data.jobId)return {error:(r.data&&r.data.error)||('Could not start the AI request (HTTP '+r.status+').')};
  var deadline=Date.now()+5*60*1000;
  while(Date.now()<deadline){
    await new Promise(function(ok){setTimeout(ok,2000);});
    try{
      var pr=await fetch('/api/app/chat/'+encodeURIComponent(r.data.jobId),{headers:{'Accept':'application/json'},signal:AbortSignal.timeout(15000)});
      var d=await pr.json().catch(function(){return null;});
      if(!d)continue;
      if(!pr.ok)return {error:d.error||('AI request failed (HTTP '+pr.status+').')};
      if(d.done)return d.error?{error:d.error}:{text:d.text};
    }catch(e){}
  }
  return {error:'The AI took too long to answer. Please try again.'};
}
function aiWire(id){
  if(aiState.cluster!==id){aiState={cluster:id,history:[],busy:false,loaded:false};}
  var f=document.getElementById('ai-form'),log=document.getElementById('ai-log');
  if(!f)return;
  // Restore the last saved turns (kept server-side, newest 5 per cluster).
  if(!aiState.loaded){
    aiState.loaded=true;
    fetch('/api/app/clusters/'+encodeURIComponent(id)+'/chat',{headers:{'Accept':'application/json'}}).then(function(r){return r.ok?r.json():null;}).then(function(d){
      if(!d||!d.turns||!d.turns.length||aiState.cluster!==id||aiState.history.length)return;
      d.turns.forEach(function(t){aiState.history.push({role:'user',text:t.q},{role:'assistant',text:t.a});});
      aiRender();
    }).catch(function(){});
  }
  log.addEventListener('click',async function(e){
    if(!e.target.closest('.ai-clear')||aiState.busy)return;
    if(!await chModal({title:'Clear this chat?',body:'Removes the saved questions and answers for this cluster.',confirm:'Clear',danger:true}))return;
    await fetch('/api/app/clusters/'+encodeURIComponent(id)+'/chat',{method:'DELETE'});
    aiState.history=[];aiRender();
  });
  f.addEventListener('submit',async function(e){
    e.preventDefault();
    var inp=document.getElementById('ai-input'),q=inp.value.trim();
    if(!q||aiState.busy)return;
    inp.value='';aiState.history.push({role:'user',text:q});aiState.busy=true;aiRender();
    var hist=aiState.history.filter(function(t){return t.role!=='error';});
    var res=await aiAsk(id,hist);
    aiState.busy=false;
    aiState.history.push(res.text!=null?{role:'assistant',text:res.text}:{role:'error',text:res.error});
    aiRender();
  });
  aiRender();
}

// ---------- Secrets ----------
// Exposed-credential findings from the agent: secrets printed in container logs
// or written literally into pod specs. Only where/what is shown, never the value.
var SECRET_LABELS={'private-key':'Private key','aws-access-key':'AWS access key','github-token':'GitHub token','slack-token':'Slack token','google-api-key':'Google API key','stripe-key':'Stripe live key','jwt':'JWT','bearer-token':'Bearer token','url-credentials':'Password in a URL','password-or-token':'Password or token','hardcoded-env':'Hard-coded secret'};
function secretSeverity(f){
  // In logs or args it has already been exposed; a known key format in env is
  // a real credential; a sensitive-looking env name is a likely one.
  if(f.source==='log'||f.source==='args')return {c:'bad',v:'High'};
  return f.type==='hardcoded-env'?{c:'off',v:'Medium'}:{c:'bad',v:'High'};
}
function agentAtLeast(v,want){
  var a=String(v||'0').replace(/^v/,'').split('.').map(Number),b=want.split('.').map(Number);
  for(var i=0;i<3;i++){if((a[i]||0)!==(b[i]||0))return (a[i]||0)>(b[i]||0);}
  return true;
}
function secretsSection(list,c){
  if(!agentAtLeast(c.agentVersion,'0.2.0')){
    return '<section class="card2"><div class="card2-head"><p class="card2-title">Leaked secrets</p><p class="card2-desc">Update the agent to turn this on.</p></div>'+
      '<div class="card2-body"><textarea class="nb" rows="2" readonly onclick="this.select()">kubectl apply -f https://clusterheart.com/agent.yaml\nkubectl -n clusterheart-sre rollout restart deployment/clusterheart-agent</textarea>'+
      '<div class="nb-actions"><button class="act approve copy-setup">Copy</button></div></div></section>';
  }
  var head='<section class="card2"><div class="card2-head"><p class="card2-title">Leaked secrets'+(list.length?' ('+list.length+')':'')+'</p></div>';
  if(!list.length)return head+'<div class="card2-body"><p class="sub" style="margin:0"><span class="pill on">None found.</span></p></div></section>';
  var rank={bad:0,off:1};
  var rows=list.slice().sort(function(a,b){var sa=secretSeverity(a),sb=secretSeverity(b);return (rank[sa.c]-rank[sb.c])||(new Date(b.lastSeen)-new Date(a.lastSeen));});
  // On phones the Severity and Seen columns are hidden; span only what's visible.
  var span=window.matchMedia&&window.matchMedia('(max-width:640px)').matches?3:5;
  var html=head+'<div class="tscroll sec-scroll"><table class="users ptable sec-table"><thead><tr><th>Workload</th><th class="sec-sev">Severity</th><th>Leaked</th><th class="sec-seen">Seen</th><th></th></tr></thead><tbody>';
  rows.forEach(function(f,i){
    var sev=secretSeverity(f),parts=(f.owner||'').split('/'),kind=parts[0],name=parts.slice(1).join('/');
    var label=SECRET_LABELS[f.type]||f.type,src=f.source==='log'?'in logs':(f.source==='args'?'in command args':'in env');
    var what=f.key?'<span class="mono-chip">'+esc(f.key)+'</span><div class="u-email" style="margin-top:4px">'+esc(label)+' · '+src+'</div>'
                  :'<div class="u-name" style="font-size:13px">'+esc(label)+'</div><div class="u-email">'+src+'</div>';
    var seen=f.source==='log'?(f.count||1)+'× · '+ago(f.lastSeen):'since '+ago(f.firstSeen);
    html+='<tr class="sec-row" data-i="'+i+'"><td><div class="u-name pod-name" title="'+esc(f.owner)+'">'+esc(name||f.owner)+'</div><div class="u-email">'+esc(f.namespace)+' · '+esc(kind)+'</div><span class="pill '+sev.c+' sec-sev-inline">'+sev.v+'</span></td>'+
      '<td class="sec-sev"><span class="pill '+sev.c+'">'+sev.v+'</span></td><td>'+what+'</td>'+
      '<td class="u-time sec-seen" style="white-space:nowrap">'+seen+'</td><td class="sec-caret">›</td></tr>'+
      '<tr class="sec-detail" data-i="'+i+'" hidden><td colspan="'+span+'">'+secretDetail(f,label,src)+'</td></tr>';
  });
  return html+'</tbody></table></div></section>';
}

// secretHighlight escapes text and marks every redaction.
function secretHighlight(t){return esc(t).replace(/\[REDACTED\]/g,'<mark class="sec-mark">[REDACTED]</mark>');}

// secretDetail is the expanded view of one finding: exactly what leaked, where,
// the full (redacted) line and what to do about it.
function secretDetail(f,label,src){
  var parts=(f.owner||'').split('/'),kind=parts[0],name=parts.slice(1).join('/');
  var secret=(f.key?'<span class="mono-chip">'+esc(f.key)+'</span> = ':'')+
    (f.masked?'<span class="mono-chip sec-masked">'+esc(f.masked)+'</span>':'<span class="mono-chip sec-masked">hidden</span>')+
    (f.length?' <span class="u-email">'+f.length+' characters</span>':'');
  var line=f.context||'';
  try{var parsed=JSON.parse(line);line=JSON.stringify(parsed,null,2);}catch(e){}
  var steps;
  if(f.source==='log')steps=['Rotate or revoke this credential now: anyone who can read these logs may have it.',
    'Find where the app prints '+(f.key?'<b>'+esc(f.key)+'</b>':'it')+' and stop logging it.',
    'This entry clears 24 hours after it was last seen.'];
  else if(f.source==='env')steps=['Store the value in a Kubernetes Secret.',
    'Point <b>'+esc(f.key||f.field||'the variable')+'</b> at it with <span class="mono-chip">valueFrom.secretKeyRef</span> instead of a plain value.',
    'Rotate it if this manifest is kept in git or shared.'];
  else steps=['Rotate this credential.','Pass it in through a Secret (env var or mounted file) instead of command arguments.'];
  var row=function(k,v){return '<div class="sec-k">'+k+'</div><div class="sec-v">'+v+'</div>';};
  return '<div class="sec-box"><div class="sec-grid">'+
    row('Secret',secret)+
    row('Type',esc(label)+' · '+src)+
    row('Workload',esc(kind)+' <b>'+esc(name)+'</b> in <b>'+esc(f.namespace)+'</b>')+
    row('Found in',(f.pod?'pod <span class="mono-chip">'+esc(f.pod)+'</span> · ':'')+'container <span class="mono-chip">'+esc(f.container||'')+'</span>')+
    row('Seen',f.source==='log'?(f.count||1)+' time'+((f.count||1)===1?'':'s')+' · first '+ago(f.firstSeen)+' · last '+ago(f.lastSeen):'since '+ago(f.firstSeen))+
    '</div>'+
    (line?'<div class="sec-k" style="margin-top:14px">'+(f.source==='log'?'Log line':(f.source==='env'?'Setting':'Command'))+'</div><pre class="log-pre sec-pre">'+secretHighlight(line)+'</pre>':'')+
    '<div class="sec-k" style="margin-top:14px">What to do</div><ol class="sec-steps">'+steps.map(function(x){return '<li>'+x+'</li>';}).join('')+'</ol>'+
    '<p class="u-email" style="margin:10px 0 0">The full value never leaves your cluster; the hint above is enough to tell which credential it is.</p></div>';
}

// ---------- Rightsizing ----------
// Per workload (namespace + owner, so history survives pod restarts and
// rollouts): current requests from the latest snapshot vs the 6h peak from the
// control plane's stored snapshots. Rules mirror the desktop app.
var RS_WINDOW_MINS=360;
var rsState={cluster:null,pods:[],usage:null,canApply:false,applied:{}};
// Platform add-ons the user doesn't size themselves: shown as Managed.
var RS_MANAGED_NS=/^(kube-system|kube-public|kube-node-lease|amazon-[\w-]+|aws-[\w-]+|clusterheart-sre)$/;
var RS_MANAGED_NAME=/^(coredns|kube-proxy|aws-node|ebs-csi-(controller|node)|efs-csi-(controller|node)|metrics-server|aws-load-balancer-controller|cluster-autoscaler|karpenter|eks-pod-identity-agent|snapshot-controller|adot-collector|cloudwatch-agent|fluent-bit)(-.*)?$/;
function rsManaged(ns,name){return RS_MANAGED_NS.test(ns)||RS_MANAGED_NAME.test(name);}
function rsVerdict(r){
  r.sugCpu=0;r.sugMem=0;
  if(r.managed)return {v:'Managed',c:'mgd'};
  if(!r.cpuReq&&!r.memReq)return {v:'No requests',c:'off'};
  if(r.window<RS_WINDOW_MINS*0.9)return {v:'Learning '+Math.floor(r.window/60)+'h/6h',c:'off'};
  // New request = peak + 20%: raises an undersized request, lowers one where
  // the peak stays under 40% of it.
  function up(peak,req){return (req&&peak>=req*0.9)?Math.ceil(peak*1.2):0;}
  function down(peak,req,floor){return (req&&peak<req*0.4)?Math.min(Math.max(Math.ceil(peak*1.2),floor),req-1):0;}
  var uc=up(r.cpuPeak,r.cpuReq),um=up(r.memPeak,r.memReq);
  if(uc||um){r.sugCpu=uc;r.sugMem=um;return {v:uc&&um?'Upsize':uc?'Upsize CPU':'Upsize memory',c:'bad'};}
  var dc=down(r.cpuPeak,r.cpuReq,10),dm=down(r.memPeak,r.memReq,16);
  if(dc||dm){r.sugCpu=dc;r.sugMem=dm;return {v:dc&&dm?'Downsize':dc?'Downsize CPU':'Downsize memory',c:'off'};}
  return {v:'OK',c:'on'};
}
function rightsizingSection(pods,canApply){
  rsState.pods=pods;rsState.usage=null;rsState.canApply=!!canApply;rsState.applied={};
  return '<div class="spin-wrap"><div class="spinner"></div></div>';
}
async function loadRightsizing(){
  var id=rsState.cluster,p=document.querySelector('.tabpanel[data-panel="rightsizing"]');
  try{
    var r=await fetch('/api/app/clusters/'+encodeURIComponent(id)+'/usage',{headers:{'Accept':'application/json'}});
    var d=await r.json().catch(function(){return {};});
    if(id!==rsState.cluster)return;
    rsState.usage=r.ok?(d.workloads||[]):[];
    if(!r.ok&&p){p.innerHTML='<section class="card2"><div class="card2-body"><p class="sub" style="margin:0">Could not load usage history'+(d.error?': '+esc(d.error):'')+'.</p></div></section>';return;}
  }catch(e){if(p)p.innerHTML='<section class="card2"><div class="card2-body"><p class="sub" style="margin:0">Could not load usage history.</p></div></section>';return;}
  if(p)p.innerHTML=renderRightsizing();
}
function renderRightsizing(){
  var pods=rsState.pods,hist={};
  (rsState.usage||[]).forEach(function(u){hist[u.namespace+'|'+u.owner]=u;});
  var g={};
  pods.forEach(function(x){
    if(x.phase!=='Running')return;
    var owner=x.owner||'Pod/'+x.name;
    if(owner.indexOf('Job/')===0)return;
    var k=x.namespace+'|'+owner,r=g[k];
    if(!r){
      var name=owner.split('/').slice(1).join('/'),h=hist[k];
      r=g[k]={ns:x.namespace,owner:owner,replicas:0,cpuReq:0,memReq:0,
        cpuPeak:h?h.cpuPeakMilli:0,memPeak:h?h.memPeakMi:0,
        window:h?Math.round((new Date(h.last)-new Date(h.first))/60000):0,
        managed:rsManaged(x.namespace,name)};
    }
    r.replicas++;
    r.cpuReq=Math.max(r.cpuReq,x.cpuReqMilli||0);r.memReq=Math.max(r.memReq,x.memReqMi||0);
    r.cpuPeak=Math.max(r.cpuPeak,x.cpuMilli||0);r.memPeak=Math.max(r.memPeak,x.memMi||0);
  });
  var rows=Object.keys(g).map(function(k){var r=g[k];r.verdict=rsVerdict(r);return r;});
  var rank={bad:0,off:1,on:2,mgd:3};
  rows.sort(function(a,b){return (rank[a.verdict.c]-rank[b.verdict.c])||((b.sugCpu||b.sugMem?1:0)-(a.sugCpu||a.sugMem?1:0))||a.owner.localeCompare(b.owner);});
  // One cell per resource: "peak / request" with the % of request below.
  function usage(peak,req,fmt){
    var top=fmt(peak)+(req?' <span class="rs-dim">/ '+fmt(req)+'</span>':'');
    var p=req?Math.round(peak/req*100):null;
    var cls=p==null?'':(p>=90?' rs-bad':(p<40?' rs-low':''));
    return '<td class="rs-num"><div>'+top+'</div><div class="rs-sub'+cls+'">'+(p==null?'no request':p+'%')+'</div></td>';
  }
  var html='<section class="card2"><div class="card2-head"><div class="toolbar-row"><div><p class="card2-title">Rightsizing</p><p class="card2-desc">Requests vs 6h peak. Suggested = peak + 20%.</p></div>'+
    (rsState.canApply?'':'<button class="act revoke rs-enable" style="margin-left:auto">Enable Apply</button>')+'</div></div>'+
    '<div id="rs-install"></div>'+
    '<div class="tscroll"><table class="users ptable rs-table"><thead><tr><th>Workload</th><th>Verdict</th><th class="rs-num">CPU <span class="rs-dim">peak / req</span></th><th class="rs-num">Memory <span class="rs-dim">peak / req</span></th><th class="rs-num">Suggested request</th><th></th></tr></thead><tbody>';
  rows.forEach(function(r){
    var parts=r.owner.split('/'),kind=parts[0],name=parts.slice(1).join('/');
    var sug='<span class="rs-dim">-</span>',copy='';
    if(r.sugCpu||r.sugMem){
      var req=[],txt=[];
      if(r.sugCpu){req.push('cpu='+r.sugCpu+'m');txt.push(fmtCpu(r.sugCpu)+' CPU');}
      if(r.sugMem){req.push('memory='+r.sugMem+'Mi');txt.push(fmtMem(r.sugMem)+' mem');}
      sug=txt.join('<br>');
      if(/^(Deployment|StatefulSet|DaemonSet)$/.test(kind)){
        var cmd='kubectl -n '+r.ns+' set resources '+kind.toLowerCase()+'/'+name+' --requests='+req.join(',');
        copy='<button class="act revoke copy-cmd" data-cmd="'+esc(cmd)+'" title="'+esc(cmd)+'">Copy</button>';
        var key=r.ns+'|'+r.owner;
        if(rsState.applied[key])copy='<span class="rs-queued">'+esc(rsState.applied[key])+'</span>';
        else if(rsState.canApply)copy+=' <button class="act approve rs-apply" data-kind="'+esc(kind)+'" data-ns="'+esc(r.ns)+'" data-name="'+esc(name)+'" data-cpu="'+(r.sugCpu||0)+'" data-mem="'+(r.sugMem||0)+'" data-cpureq="'+r.cpuReq+'" data-memreq="'+r.memReq+'">Apply</button>';
      }
    }
    html+='<tr><td><div class="u-name pod-name" title="'+esc(r.owner)+'">'+esc(name)+'</div><div class="u-email">'+esc(r.ns)+' · '+esc(kind)+' · '+r.replicas+' pod'+(r.replicas===1?'':'s')+'</div></td>'+
      '<td><span class="pill '+r.verdict.c+'">'+esc(r.verdict.v)+'</span></td>'+
      usage(r.cpuPeak,r.cpuReq,fmtCpu)+usage(r.memPeak,r.memReq,fmtMem)+
      '<td class="rs-num">'+sug+'</td><td class="rs-act">'+copy+'</td></tr>';
  });
  if(!rows.length)html+='<tr><td colspan="6" class="u-time">No running workloads in the latest snapshot.</td></tr>';
  return html+'</tbody></table></div></section>';
}

async function loadDetail(id){
  var box=document.getElementById('clusters');
  box.innerHTML='<div class="spin-wrap"><div class="spinner"></div></div>';
  try{
    var r=await fetch('/api/app/clusters',{headers:{'Accept':'application/json'}});
    var payload=await r.json();
    var list=(payload&&payload.clusters)||[];
    var c=null;list.forEach(function(x){if(x.clusterId===id)c=x;});
    if(!c){box.innerHTML='<a href="/app" style="color:var(--muted)">All clusters</a><p class="sub" style="margin-top:14px">Cluster not found.</p>';return;}
    var connected=c.connected===true;
    var env=c.environment?esc(c.environment):'';
    var statusHtml='<span class="pill '+(connected?'on':'off')+'" style="font-size:13px;margin-left:10px;vertical-align:middle">'+(connected?'Connected':'Offline')+'</span>';
    var html='<div class="detail-head"><h1 style="margin:0"><span class="rename-name" data-id="'+esc(c.clusterId)+'" data-name="'+esc(c.name||'')+'" title="Double-click to rename">'+esc(c.name||c.clusterId)+'</span>'+statusHtml+'</h1><div class="actbar">'+kebab(c)+'</div></div>';
    var L=c.latest;
    if(!L){html+='<section class="card2"><div class="card2-body"><p class="sub" style="margin:0">No data yet. Once the agent reports (every ~30s), health shows up here. Make sure the agent pod is Running in <b>clusterheart-sre</b>.</p></div></section>';box.innerHTML=html;return;}
    var n=L.nodes||{},p=L.pods||{},wl=L.workloads||{};
    var all=L.podList||[];
    // One row per scheduled job (latest CronJob run); see dedupeJobs.
    var jl=dedupeJobs(L.jobList);
    function isJob(x){return (x.owner||'').indexOf('Job/')===0;}
    var svc=all.filter(function(x){return !isJob(x);});
    var havePL=all.length>0,haveJL=jl.length>0;
    var podRun=svc.filter(function(x){return x.phase==='Running';}).length;
    var podFail=svc.filter(function(x){return x.phase==='Failed';}).length;
    // Job status comes from the Job objects (not their pods, which get GC'd), so
    // failed/complete runs stay visible.
    var jobDone=jl.filter(function(x){return x.status==='Complete';}).length;
    var jobRun=jl.filter(function(x){return x.status==='Running';}).length;
    var jobFail=jl.filter(function(x){return x.status==='Failed';}).length;
    var jobPend=Math.max(jl.length-jobDone-jobRun-jobFail,0);
    var podsVal=havePL?(podRun+' / '+svc.length):((p.running||0)+' / '+(p.total||0));
    var podsSub=havePL?(svc.length+' total'):((p.pending||0)+' pending · '+(p.failed||0)+' failed');
    var jobsVal=haveJL?(jobDone+' / '+jl.length):(''+(wl.jobs||0));
    var jobsSub=haveJL?(jobRun+' running · '+jobPend+' pending'):((wl.jobs||0)+' configured');
    html+='<div class="statgrid">'+
      stat('Nodes ready',(n.ready||0)+' / '+(n.total||0),(n.notReady?n.notReady+' not ready':'all ready'))+
      stat('Pods'+fbadge(havePL?podFail:(p.failed||0)),podsVal,podsSub)+
      stat('Jobs',jobsVal,jobsSub)+
      stat('Restarts',(p.highRestart||0),'pods restarting')+
    '</div>';
    var nodesHtml=nodesSection(L.nodeList);
    var hasMetrics=all.some(function(x){return x.cpuMilli||x.memMi;});
    function byCpu(a,b){return (b.cpuMilli||0)-(a.cpuMilli||0);}
    svc.sort(byCpu);
    function rowK(x){
      var ok=(x.phase==='Running'||x.phase==='Succeeded');
      return '<tr data-ns="'+esc(x.namespace)+'" data-name="'+esc((x.name||'').toLowerCase())+'"><td><div class="u-name pod-name" title="'+esc(x.name)+'">'+esc(x.name)+'</div><div class="u-email">'+esc(x.namespace)+'</div></td>'+
        '<td><span class="pill '+(ok?'on':'off')+'"><span class="dot"></span>'+esc(x.phase||'')+'</span></td>'+
        '<td class="u-time">'+esc(x.ready||'')+'</td><td class="u-time">'+(x.restarts||0)+'</td>'+
        '<td class="u-time">'+(x.cpuMilli?x.cpuMilli+'m':'-')+'</td><td class="u-time">'+(x.memMi?x.memMi+'Mi':'-')+'</td>'+
        '<td class="u-email">'+esc(x.node||'')+'</td></tr>';
    }
    function rowJob(j){
      var st=j.status||'Pending';
      var cls=st==='Failed'?'bad':(st==='Pending'?'off':'on');
      var fin=j.finishedAt?ago(j.finishedAt):(j.startedAt?ago(j.startedAt):'');
      var sub=j.owner?esc(j.owner):esc(j.namespace);
      return '<tr data-ns="'+esc(j.namespace)+'" data-name="'+esc((j.name||'').toLowerCase())+'"><td><div class="u-name pod-name" title="'+esc(j.name)+'">'+esc(j.name)+'</div><div class="u-email">'+sub+'</div></td>'+
        '<td><span class="pill '+cls+'">'+esc(st)+'</span></td>'+
        '<td class="u-time">'+esc(j.completions||'')+'</td>'+
        '<td class="'+(j.failed?'util-val crit':'u-time')+'">'+(j.failed||0)+'</td>'+
        '<td class="u-time">'+fin+'</td>'+
        '<td class="u-email">'+esc(j.namespace)+'</td></tr>';
    }
    var podsHtml='';
    if(svc.length||jl.length){
      var nss=Array.from(new Set(all.map(function(x){return x.namespace;}).concat(jl.map(function(x){return x.namespace;})))).sort();
      podsHtml='<section class="card2">'+
        '<div class="card2-head">'+
          '<div class="toolbar-row">'+
            '<div class="seg" id="pj-seg"><button class="seg-btn active" data-kind="pod">'+svc.length+' Pods</button><button class="seg-btn" data-kind="job">'+jl.length+' Jobs</button></div>'+
            '<select id="pod-ns" class="toolbar-input toolbar-ns"><option value="">All namespaces</option>'+nss.map(function(n){return '<option>'+esc(n)+'</option>';}).join('')+'</select>'+
            '<input id="pod-search" class="toolbar-input toolbar-search" placeholder="Search">'+
          '</div>'+
        '</div>'+
        '<div class="tscroll" id="pods-wrap"><table class="users ptable"><thead><tr><th>Name</th><th>Status</th><th>Ready</th><th>Restarts</th><th>CPU</th><th>Mem</th><th>Node</th></tr></thead><tbody id="pods-body">'+
        svc.map(rowK).join('')+
        '</tbody></table>'+
        (L.podListTruncated?'<div class="card2-body"><p class="sub" style="margin:0">+'+L.podListTruncated+' more pods (capped).</p></div>':'')+'</div>'+
        '<div class="tscroll" id="jobs-wrap" style="display:none"><table class="users ptable"><thead><tr><th>Job</th><th>Status</th><th>Completions</th><th>Failed</th><th>Finished</th><th>Namespace</th></tr></thead><tbody id="jobs-body">'+
        jl.map(rowJob).join('')+
        '</tbody></table>'+
        (L.jobListTruncated?'<div class="card2-body"><p class="sub" style="margin:0">+'+L.jobListTruncated+' more jobs (capped).</p></div>':'')+'</div>'+
      '</section>';
    }else{
      podsHtml='<section class="card2"><div class="card2-body"><p class="sub" style="margin:0">No pods or jobs reported in the latest snapshot.</p></div></section>';
    }
    var healthHtml='';
    var probs=[];
    (L.podProblems||[]).forEach(function(x){probs.push({type:x.type,where:x.namespace+'/'+x.pod+(x.container?' ['+x.container+']':''),msg:x.message});});
    (L.workloadProblems||[]).forEach(function(x){probs.push({type:x.type,where:x.namespace+'/'+x.kind+' '+x.name,msg:x.message});});
    if(probs.length){
      healthHtml+='<section class="card2"><div class="card2-head"><p class="card2-title">Problems ('+probs.length+')</p><p class="card2-desc">Measured fault signals from the latest snapshot.</p></div><div class="tscroll health-scroll"><table class="users ptable"><thead><tr><th>Type</th><th>Object</th><th>Detail</th></tr></thead><tbody>';
      probs.forEach(function(x){healthHtml+='<tr><td><span class="pill off"><span class="dot"></span>'+esc(x.type)+'</span></td><td class="u-email" title="'+esc(x.where)+'">'+esc(x.where)+'</td><td class="u-time" title="'+esc(x.msg||'')+'">'+esc(x.msg||'')+'</td></tr>';});
      healthHtml+='</tbody></table></div></section>';
    }
    // Merge repeats of the same warning on the same object (counts add up,
    // newest time wins), newest first.
    var wmap={},w=[];
    (L.warnings||[]).forEach(function(e){
      var k=e.reason+'|'+e.namespace+'|'+e.object,g=wmap[k];
      if(!g){g=wmap[k]={reason:e.reason,namespace:e.namespace,object:e.object,count:0,lastSeen:e.lastSeen};w.push(g);}
      g.count+=(e.count||1);
      if(e.lastSeen&&(!g.lastSeen||new Date(e.lastSeen)>new Date(g.lastSeen)))g.lastSeen=e.lastSeen;
    });
    w.sort(function(a,b){return new Date(b.lastSeen||0)-new Date(a.lastSeen||0);});
    if(w.length){
      healthHtml+='<section class="card2" style="margin-top:16px"><div class="card2-head"><p class="card2-title">Recent warnings ('+w.length+')</p></div><div class="tscroll health-scroll"><table class="users ptable"><thead><tr><th>Reason</th><th>Object</th><th>Count</th><th>Last seen</th></tr></thead><tbody>';
      w.forEach(function(e){healthHtml+='<tr><td class="u-name" style="font-size:13.5px">'+esc(e.reason)+'</td><td class="u-email" title="'+esc(e.namespace)+'/'+esc(e.object)+'">'+esc(e.namespace)+'/'+esc(e.object)+'</td><td class="u-time">'+(e.count||0)+'</td><td class="u-time" title="'+(e.lastSeen?esc(new Date(e.lastSeen).toLocaleString()):'')+'">'+ago(e.lastSeen)+'</td></tr>';});
      healthHtml+='</tbody></table></div></section>';
    }
    if(!probs.length&&!w.length){healthHtml+='<section class="card2"><div class="card2-body"><p class="sub" style="margin:0">No problems or warnings in the latest snapshot. Looks healthy.</p></div></section>';}
    // Log tails the agent read from failing pods (newest crash first in agent order).
    var pl=L.podLogs||[];
    if(pl.length){
      healthHtml+='<section class="card2" style="margin-top:16px"><div class="card2-head"><p class="card2-title">Logs from failing pods ('+pl.length+')</p><p class="card2-desc">Last lines the agent read, secrets scrubbed.</p></div><div class="card2-body">'+
        pl.map(function(l,i){
          var when=l.previous?'before the last restart':'current';
          return '<details class="log-box"'+(i===0?' open':'')+'><summary><span class="u-name" style="font-size:13.5px">'+esc(l.namespace)+'/'+esc(l.pod)+'</span> <span class="u-email">'+esc(l.container)+' · '+esc(l.reason||'')+' · '+when+' · '+ago(l.collectedAt)+'</span></summary>'+
            '<pre class="log-pre">'+esc((l.lines||[]).join('\n'))+'</pre></details>';
        }).join('')+'</div></section>';
    }
    var secrets=L.secretFindings||[];
    // Pods / Nodes / SRE tabs. The Nodes tab only appears when the agent reports
    // node data. The SRE panels load lazily from the control plane on first open.
    html+='<div class="tabs" role="tablist">'+
      '<button class="tab active" data-tab="pods">Pods</button>'+
      (nodesHtml?'<button class="tab" data-tab="nodes">Nodes</button>':'')+
      '<button class="tab" data-tab="health">Health'+(probs.length+w.length?' <span class="tab-badge">'+(probs.length+w.length)+'</span>':'')+'</button>'+
      '<button class="tab" data-tab="secrets">Secrets'+(secrets.length?' <span class="tab-badge">'+secrets.length+'</span>':'')+'</button>'+
      '<button class="tab" data-tab="rightsizing">Rightsizing</button>'+
      '<button class="tab" data-tab="sre-overview">SRE overview</button>'+
      '<button class="tab" data-tab="incidents">Incidents</button>'+
      '<button class="tab" data-tab="remediation">Remediation</button>'+
      '<button class="tab" data-tab="activity">Activity</button>'+
      '<button class="tab" data-tab="ai">AI</button>'+
    '</div>'+
    '<div class="tabpanel active" data-panel="pods">'+podsHtml+'</div>'+
    (nodesHtml?'<div class="tabpanel" data-panel="nodes">'+nodesHtml+'</div>':'')+
    '<div class="tabpanel" data-panel="health">'+healthHtml+'</div>'+
    '<div class="tabpanel" data-panel="secrets">'+secretsSection(secrets,c)+'</div>'+
    '<div class="tabpanel" data-panel="rightsizing">'+rightsizingSection(all,c.remediationEnabled===true)+'</div>'+
    '<div class="tabpanel" data-panel="sre-overview"></div>'+
    '<div class="tabpanel" data-panel="incidents"></div>'+
    '<div class="tabpanel" data-panel="remediation"></div>'+
    '<div class="tabpanel" data-panel="activity"></div>'+
    '<div class="tabpanel" data-panel="ai">'+aiPanelHtml()+'</div>';
    box.innerHTML=html;
    var ps=document.getElementById('pod-search'),pn=document.getElementById('pod-ns'),seg=document.getElementById('pj-seg'),_kind='pod';
    function applyPodFilter(){
      var q=((ps&&ps.value)||'').toLowerCase(),ns=(pn&&pn.value)||'';
      var pw=document.getElementById('pods-wrap'),jw=document.getElementById('jobs-wrap');
      if(pw)pw.style.display=_kind==='pod'?'':'none';
      if(jw)jw.style.display=_kind==='job'?'':'none';
      document.querySelectorAll((_kind==='pod'?'#pods-body':'#jobs-body')+' tr').forEach(function(tr){
        var okn=!ns||tr.dataset.ns===ns, okq=!q||(tr.dataset.name||'').indexOf(q)>=0;
        tr.style.display=(okn&&okq)?'':'none';
      });
    }
    if(ps)ps.addEventListener('input',applyPodFilter);
    if(pn)pn.addEventListener('change',applyPodFilter);
    if(seg)seg.querySelectorAll('.seg-btn').forEach(function(b){b.addEventListener('click',function(){seg.querySelectorAll('.seg-btn').forEach(function(x){x.classList.remove('active');});b.classList.add('active');_kind=b.dataset.kind;applyPodFilter();});});
    applyPodFilter();
    sreState.cluster=id;sreState.data=null;rsState.cluster=id;
    aiWire(id);
    document.querySelectorAll('.tab').forEach(function(t){t.addEventListener('click',function(){
      var name=t.dataset.tab;
      _activeTab=name;
      document.querySelectorAll('.tab').forEach(function(x){x.classList.toggle('active',x===t);});
      document.querySelectorAll('.tabpanel').forEach(function(pnl){pnl.classList.toggle('active',pnl.dataset.panel===name);});
      if(SRE_TABS.indexOf(name)>=0&&!sreState.data)loadSRE();
      if(name==='rightsizing'&&!rsState.usage)loadRightsizing();
    });});
    // Keep the open tab across re-renders (rename, environment change).
    var _keep=document.querySelector('.tab[data-tab="'+_activeTab+'"]');
    if(_keep&&_activeTab!=='pods')_keep.click();
    document.querySelectorAll('.copy-setup').forEach(function(btn){btn.addEventListener('click',function(){var ta=btn.closest('.card2').querySelector('textarea');if(ta){navigator.clipboard.writeText(ta.value);btn.textContent='Copied';}});});
  }catch(e){box.innerHTML='<a href="/app" style="color:var(--muted)">All clusters</a><p class="sub" style="margin-top:14px">Could not load cluster.</p>';}
}

// ---------- SRE: overview / incidents / remediation ----------
// One fetch (/api/app/clusters/{id}/sre) feeds all three tabs; the server keeps
// only this cluster's incidents and proposals.
var SRE_TABS=['sre-overview','incidents','remediation','activity'];
var _activeTab='pods';
var sreState={cluster:null,data:null,incFilter:'open',propFilter:'',diag:{}};
function srePanel(name){return document.querySelector('.tabpanel[data-panel="'+name+'"]');}
function sreMsg(text){return '<section class="card2"><div class="card2-body"><p class="sub" style="margin:0">'+text+'</p></div></section>';}
function sevPill(s){var c=s==='high'||s==='critical'?'bad':(s==='medium'?'off':'on');return '<span class="pill '+c+'">'+esc(s||'-')+'</span>';}
function propPill(s){var c=s==='executed'?'on':(s==='failed'||s==='blocked'||s==='rejected'?'bad':'off');return '<span class="pill '+c+'">'+esc(s||'-')+'</span>';}
function count(list,f){return list.filter(f).length;}

async function loadSRE(){
  var id=sreState.cluster;
  SRE_TABS.forEach(function(t){var p=srePanel(t);if(p)p.innerHTML='<div class="spin-wrap"><div class="spinner"></div></div>';});
  try{
    var r=await fetch('/api/app/clusters/'+encodeURIComponent(id)+'/sre',{headers:{'Accept':'application/json'}});
    var d=await r.json().catch(function(){return {};});
    if(id!==sreState.cluster)return;
    if(!r.ok){var m=sreMsg('Could not load SRE data'+(d&&d.error?': '+esc(d.error):'')+'.');SRE_TABS.forEach(function(t){var p=srePanel(t);if(p)p.innerHTML=m;});return;}
    sreState.data=d;
  }catch(e){SRE_TABS.forEach(function(t){var p=srePanel(t);if(p)p.innerHTML=sreMsg('Could not load SRE data.');});return;}
  renderSRE();
}
function renderSRE(){renderSREOverview();renderIncidents();renderRemediation();renderActivity();}

// Activity: the control plane's audit trail for this cluster, newest first.
var EVENT_LABELS={'proposal.created':'Fix proposed','proposal.blocked':'Blocked by policy','proposal.approved':'Fix approved','proposal.rejected':'Fix rejected','remediation.executed':'Fix applied','remediation.failed':'Fix failed','policy.updated':'Policy changed','policy.paused':'Automation paused','policy.resumed':'Automation resumed'};
function eventPill(ev){
  var c=/blocked|rejected|failed|paused/.test(ev)?'bad':(/executed|approved|resumed/.test(ev)?'on':'off');
  return '<span class="pill '+c+'">'+esc(EVENT_LABELS[ev]||ev)+'</span>';
}
function renderActivity(){
  var p=srePanel('activity'),d=sreState.data;if(!p||!d)return;
  var props={};(d.proposals||[]).forEach(function(x){props[x.id]=x;});
  var incs={};(d.incidents||[]).forEach(function(x){incs[x.id]=x;});
  var list=(d.activity||[]).slice().sort(function(a,b){return new Date(b.at||0)-new Date(a.at||0);});
  var html='<section class="card2"><div class="card2-head"><div class="toolbar-row"><div><p class="card2-title">Activity</p><p class="card2-desc">Who proposed, approved, rejected or paused what on this cluster.</p></div><button class="act revoke sre-refresh" style="margin-left:auto">Refresh</button></div></div>';
  if(!list.length){p.innerHTML=html+'<div class="card2-body"><p class="sub" style="margin:0">No activity yet.</p></div></section>';return;}
  html+='<div class="tscroll"><table class="users ptable"><thead><tr><th>When</th><th>Event</th><th>About</th><th>By</th><th>Details</th></tr></thead><tbody>';
  list.forEach(function(e){
    var about='-',pr=props[e.proposalId],inc=incs[e.incidentId];
    if(pr){var t=pr.target||{};about=esc(pr.action)+'<div class="u-email">'+esc((t.kind||'')+'/'+(t.namespace||'')+'/'+(t.name||''))+'</div>';}
    else if(inc){about=esc(inc.title||inc.type);}
    var actor=(e.actor||'').replace(/^user:/,'');
    html+='<tr><td class="u-time" title="'+esc(e.at?new Date(e.at).toLocaleString():'')+'">'+ago(e.at)+'</td><td>'+eventPill(e.event)+'</td><td><div class="u-name" style="font-size:13.5px">'+about+'</div></td>'+
      '<td class="u-email">'+esc(actor)+'</td><td class="u-time" style="white-space:normal;max-width:320px">'+esc(e.details||'')+'</td></tr>';
  });
  p.innerHTML=html+'</tbody></table></div></section>';
}

function automationCard(pol){
  var paused=pol.paused||pol.clusterPaused;
  var state=pol.paused?'Paused for all clusters':(pol.clusterPaused?'Paused for this cluster':'Active');
  var btn=pol.paused?'':(pol.clusterPaused
    ?'<button class="act approve sre-auto" data-action="resume">Resume automation</button>'
    :'<button class="act revoke sre-auto" data-action="pause">Pause automation</button>');
  return '<section class="card2"><div class="card2-head"><p class="card2-title">Automated remediation</p><p class="card2-desc">Mode <b>'+esc(pol.mode||'-')+'</b> · <span class="pill '+(paused?'off':'on')+'">'+state+'</span>. Pausing is an emergency stop: no fix runs on this cluster until you resume.</p></div>'+
    (btn?'<div class="card2-body"><div class="nb-actions">'+btn+'</div></div>':'')+'</section>';
}

function renderSREOverview(){
  var p=srePanel('sre-overview'),d=sreState.data;if(!p||!d)return;
  var inc=d.incidents||[],pr=d.proposals||[],pol=d.policy||{};
  var open=inc.filter(function(i){return i.status==='open';});
  var hi=count(open,function(i){return i.severity==='high'||i.severity==='critical';});
  var pend=count(pr,function(x){return x.status==='proposed';});
  var exe=count(pr,function(x){return x.status==='executed';}),fail=count(pr,function(x){return x.status==='failed';});
  var paused=pol.paused||pol.clusterPaused;
  var html='<div class="statgrid">'+
    stat('Open incidents',open.length,hi+' high · '+(open.length-hi)+' other')+
    stat('Awaiting approval',pend,'remediation proposals')+
    stat('Executed fixes',exe,fail+' failed')+
    stat('Automation',paused?'Paused':'Active',esc(pol.mode||'-'))+
  '</div>';
  var byType={};open.forEach(function(i){byType[i.type]=(byType[i.type]||0)+1;});
  var types=Object.keys(byType).sort(function(a,b){return byType[b]-byType[a];});
  if(types.length){
    html+='<section class="card2"><div class="card2-head"><p class="card2-title">Open incidents by type</p></div><table class="users ptable"><thead><tr><th>Type</th><th>Open</th></tr></thead><tbody>'+
      types.map(function(t){return '<tr><td class="u-name" style="font-size:13.5px">'+esc(t)+'</td><td class="u-time">'+byType[t]+'</td></tr>';}).join('')+
      '</tbody></table></section>';
  }else{
    html+=sreMsg('No open incidents on this cluster.');
  }
  html+='<div style="margin-top:16px">'+automationCard(pol)+'</div>';
  p.innerHTML=html;
}

function diagHtml(dg){
  if(!dg)return '';
  if(dg.loading)return '<p class="sub" style="margin:0">Diagnosing…</p>';
  if(dg.error)return '<p class="sub" style="margin:0;color:var(--red)">'+esc(dg.error)+'</p>';
  var h='<div style="font-size:13px;line-height:1.6"><div><b>'+esc(dg.summary||'')+'</b> <span class="u-email">('+esc(dg.confidence||'-')+' confidence, '+esc(dg.source||'')+')</span></div>';
  if((dg.observedFacts||[]).length)h+='<div style="margin-top:6px;color:var(--muted)">Observed</div><ul style="margin:2px 0 0 18px;padding:0">'+dg.observedFacts.map(function(f){return '<li>'+esc(f)+'</li>';}).join('')+'</ul>';
  if((dg.hypotheses||[]).length)h+='<div style="margin-top:6px;color:var(--muted)">Likely causes</div><ul style="margin:2px 0 0 18px;padding:0">'+dg.hypotheses.map(function(x){return '<li>'+esc(x.cause)+' <span class="u-email">('+esc(x.confidence||'-')+')</span></li>';}).join('')+'</ul>';
  var t=dg.recommendedTarget;
  h+='<div style="margin-top:6px"><span style="color:var(--muted)">Recommended:</span> '+esc(dg.recommendedAction||'-')+(t?' on '+esc(t.kind+'/'+t.namespace+'/'+t.name):'')+'</div>';
  if(dg.risks)h+='<div><span style="color:var(--muted)">Risks:</span> '+esc(dg.risks)+'</div>';
  if(dg.rollback)h+='<div><span style="color:var(--muted)">Rollback:</span> '+esc(dg.rollback)+'</div>';
  return h+'<div class="u-email" style="margin-top:6px">Advisory only. A fix still needs a policy-checked proposal and your approval.</div></div>';
}

function renderIncidents(){
  var p=srePanel('incidents'),d=sreState.data;if(!p||!d)return;
  var all=d.incidents||[],f=sreState.incFilter;
  var list=all.filter(function(i){return !f||i.status===f;}).sort(function(a,b){return new Date(b.lastSeen||0)-new Date(a.lastSeen||0);});
  var nOpen=count(all,function(i){return i.status==='open';});
  var seg='<div class="seg" id="inc-seg">'+[['open',nOpen+' Open'],['resolved',(all.length-nOpen)+' Resolved'],['','All']].map(function(o){return '<button class="seg-btn'+(o[0]===f?' active':'')+'" data-f="'+o[0]+'">'+o[1]+'</button>';}).join('')+'</div>';
  var html='<section class="card2"><div class="card2-head"><div class="toolbar-row">'+seg+'<button class="act revoke sre-refresh" style="margin-left:auto">Refresh</button></div></div>';
  if(!list.length){
    html+='<div class="card2-body"><p class="sub" style="margin:0">No '+(f||'')+' incidents.</p></div></section>';
  }else{
    html+='<div class="tscroll"><table class="users ptable"><thead><tr><th>Incident</th><th>Severity</th><th>Status</th><th>Seen</th><th>Last seen</th><th></th></tr></thead><tbody>';
    list.forEach(function(i){
      var where=(i.namespace?i.namespace+'/':'')+(i.workload||i.node||'');
      var acts=i.status==='open'?'<button class="act revoke sre-diag" data-id="'+esc(i.id)+'">Diagnose</button> <button class="act approve sre-propose" data-id="'+esc(i.id)+'" title="Turn the recommendation into a policy-checked proposal">Propose fix</button>':'';
      html+='<tr><td><div class="u-name pod-name" title="'+esc(i.title||i.type)+'">'+esc(i.title||i.type)+'</div><div class="u-email">'+esc(i.type)+(where?' · '+esc(where):'')+'</div></td>'+
        '<td>'+sevPill(i.severity)+'</td><td><span class="pill '+(i.status==='open'?'bad':'on')+'">'+esc(i.status||'-')+'</span></td>'+
        '<td class="u-time">'+(i.occurrences||0)+'×</td><td class="u-time">'+ago(i.lastSeen)+'</td>'+
        '<td style="white-space:nowrap;text-align:right">'+acts+'</td></tr>';
      var dg=sreState.diag[i.id];
      if(dg)html+='<tr><td colspan="6" style="background:var(--active-bg)">'+diagHtml(dg)+'</td></tr>';
    });
    html+='</tbody></table></div></section>';
  }
  p.innerHTML=html;
}

function renderRemediation(){
  var p=srePanel('remediation'),d=sreState.data;if(!p||!d)return;
  var all=d.proposals||[],f=sreState.propFilter;
  var list=all.filter(function(x){return !f||x.status===f;}).sort(function(a,b){return new Date(b.createdAt||0)-new Date(a.createdAt||0);});
  var opts=['','proposed','approved','executed','failed','blocked','rejected'];
  var sel='<select id="prop-status" class="toolbar-input">'+opts.map(function(o){return '<option value="'+o+'"'+(o===f?' selected':'')+'>'+(o?o.charAt(0).toUpperCase()+o.slice(1):'All statuses')+'</option>';}).join('')+'</select>';
  var html=automationCard(d.policy||{})+'<section class="card2" style="margin-top:16px"><div class="card2-head"><div class="toolbar-row">'+sel+'<button class="act revoke sre-refresh" style="margin-left:auto">Refresh</button></div></div>';
  if(!list.length){
    html+='<div class="card2-body"><p class="sub" style="margin:0">No proposals'+(f?' with status '+esc(f):'')+'. Use <b>Propose fix</b> on an incident to create one.</p></div></section>';
  }else{
    html+='<div class="tscroll"><table class="users ptable"><thead><tr><th>Action</th><th>Status</th><th>Risk</th><th>Why / result</th><th>Created</th><th></th></tr></thead><tbody>';
    list.forEach(function(x){
      var t=x.target||{};
      var res='';
      if(x.execution){var e=x.execution;res='<div class="'+(e.success?'u-time':'util-val crit')+'">'+(e.success?'OK':'Failed')+(e.dryRun?' (dry run)':'')+(e.error?': '+esc(e.error):'')+'</div>';}
      else if(x.decision&&(x.decision.reasons||[]).length&&!x.decision.allowed){res='<div class="u-email">'+esc(x.decision.reasons.join('; '))+'</div>';}
      var acts=x.status==='proposed'?'<button class="act approve sre-approve" data-id="'+esc(x.id)+'">Approve</button> <button class="act revoke sre-reject" data-id="'+esc(x.id)+'">Reject</button>':'';
      html+='<tr><td><div class="u-name pod-name">'+esc(x.action)+'</div><div class="u-email">'+esc((t.kind||'')+'/'+(t.namespace||'')+'/'+(t.name||''))+'</div></td>'+
        '<td>'+propPill(x.status)+'</td><td>'+sevPill(x.riskClass)+'</td>'+
        '<td><div class="u-time" title="'+esc(x.rationale||'')+'" style="max-width:340px;white-space:normal">'+esc(x.rationale||'-')+'</div>'+res+'</td>'+
        '<td class="u-time">'+ago(x.createdAt)+'</td>'+
        '<td style="white-space:nowrap;text-align:right">'+acts+'</td></tr>';
    });
    html+='</tbody></table></div></section>';
  }
  p.innerHTML=html;
}

var _sbox=document.getElementById('clusters');
if(_sbox)_sbox.addEventListener('click',async function(e){
  var t=e.target;if(!t.closest)return;
  var b;
  if(b=t.closest('tr.sec-row')){
    var d=b.nextElementSibling;
    if(d&&d.classList.contains('sec-detail')){d.hidden=!d.hidden;b.classList.toggle('open',!d.hidden);}
    return;
  }
  if(b=t.closest('.copy-cmd')){navigator.clipboard.writeText(b.dataset.cmd);b.textContent='Copied';return;}
  if(b=t.closest('.rs-apply')){
    var d=b.dataset,cpu=+d.cpu,mem=+d.mem,lines=[];
    if(cpu)lines.push('CPU request <b>'+fmtCpu(+d.cpureq)+'</b> → <b>'+fmtCpu(cpu)+'</b>');
    if(mem)lines.push('Memory request <b>'+fmtMem(+d.memreq)+'</b> → <b>'+fmtMem(mem)+'</b>');
    var ok=await chModal({title:'Apply to '+esc(d.kind)+' '+esc(d.name)+'?',body:lines.join('<br>')+'<br><br>Namespace <b>'+esc(d.ns)+'</b>. Pods restart one by one with the new requests. The previous values are kept in Remediation for rollback.',confirm:'Apply'});
    if(!ok)return;
    b.disabled=true;b.textContent='Applying…';
    var ar=await _post('/api/app/clusters/'+encodeURIComponent(rsState.cluster)+'/rightsize',{kind:d.kind,namespace:d.ns,name:d.name,cpuMilli:cpu,memMi:mem});
    var key=d.ns+'|'+d.kind+'/'+d.name;
    if(ar.ok&&ar.data.status==='approved'){
      rsState.applied[key]='Queued';
      var p0=document.querySelector('.tabpanel[data-panel="rightsizing"]');if(p0)p0.innerHTML=renderRightsizing();
      sreState.data=null;
      await chModal({title:'Queued',body:'The remediation controller in your cluster applies it within about 15 seconds. Track the result in the <b>Remediation</b> and <b>Activity</b> tabs.',confirm:'OK',single:true});
      return;
    }
    b.disabled=false;b.textContent='Apply';
    var why=(ar.data&&ar.data.decision&&ar.data.decision.reasons&&ar.data.decision.reasons.length)?ar.data.decision.reasons.join('; '):((ar.data&&ar.data.error)||'Could not apply.');
    await chModal({title:ar.data&&ar.data.status==='blocked'?'Blocked by policy':'Failed',body:esc(why),confirm:'OK',single:true});
    return;
  }
  if(b=t.closest('.rs-enable')){
    var go=await chModal({title:'Enable Apply for this cluster?',body:'Apply needs the Clusterheart remediation controller running in your cluster. It can only change CPU/memory requests, restart or scale workloads, and only after you approve. This issues a new controller token (any previous one stops working).',confirm:'Continue'});
    if(!go)return;
    b.disabled=true;
    var er=await _post('/api/app/clusters/'+encodeURIComponent(rsState.cluster)+'/remediation',{});
    b.disabled=false;
    if(!er.ok){await chModal({title:'Failed',body:esc((er.data&&er.data.error)||'Could not enable Apply.'),confirm:'OK',single:true});return;}
    var box=document.getElementById('rs-install');
    if(box)box.innerHTML='<div class="card2-body" style="border-bottom:1px solid var(--border)"><p class="sub" style="margin:0 0 10px">Run this once against the cluster, then reload this page. <b>The token is shown only once.</b></p>'+
      '<textarea class="nb" rows="7" readonly onclick="this.select()">'+esc(er.data.install)+'</textarea>'+
      '<div class="nb-actions"><button class="act approve copy-cmd" data-cmd="'+esc(er.data.install)+'">Copy</button></div></div>';
    return;
  }
  if(b=t.closest('#inc-seg .seg-btn')){sreState.incFilter=b.dataset.f;renderIncidents();return;}
  if(t.closest('.sre-refresh')){sreState.diag={};loadSRE();return;}
  if(b=t.closest('.sre-diag')){
    var iid=b.dataset.id;sreState.diag[iid]={loading:true};renderIncidents();
    var r=await _post('/api/app/incidents/'+encodeURIComponent(iid)+'/diagnose',{});
    sreState.diag[iid]=r.ok?r.data:{error:(r.data&&r.data.error)||'Diagnosis failed.'};
    renderIncidents();return;
  }
  if(b=t.closest('.sre-propose')){
    b.disabled=true;
    var pr=await _post('/api/app/incidents/'+encodeURIComponent(b.dataset.id)+'/propose',{});
    if(!pr.ok){b.disabled=false;await chModal({title:'No fix proposed',body:esc((pr.data&&pr.data.error)||'Could not create a proposal.'),confirm:'OK',single:true});return;}
    await chModal({title:'Proposal created',body:'Status: <b>'+esc(pr.data.status||'')+'</b>. Review it in the Remediation tab.',confirm:'OK',single:true});
    await loadSRE();return;
  }
  if(b=t.closest('.sre-approve')){
    if(!await chModal({title:'Approve this fix?',body:'The remediation controller in your cluster will apply it (subject to policy).',confirm:'Approve'}))return;
    var ar=await _post('/api/app/proposals/'+encodeURIComponent(b.dataset.id)+'/approve',{});
    if(!ar.ok){await chModal({title:'Failed',body:esc((ar.data&&ar.data.error)||'Could not approve.'),confirm:'OK',single:true});}
    await loadSRE();return;
  }
  if(b=t.closest('.sre-reject')){
    var reason=await chModal({title:'Reject this fix?',input:'',placeholder:'Reason (optional)',confirm:'Reject',danger:true});
    if(reason==null)return;
    var rr=await _post('/api/app/proposals/'+encodeURIComponent(b.dataset.id)+'/reject',{reason:reason});
    if(!rr.ok){await chModal({title:'Failed',body:esc((rr.data&&rr.data.error)||'Could not reject.'),confirm:'OK',single:true});}
    await loadSRE();return;
  }
  if(b=t.closest('.sre-auto')){
    var act=b.dataset.action;
    if(act==='pause'&&!await chModal({title:'Pause automation?',body:'No remediation will run on this cluster until you resume.',confirm:'Pause',danger:true}))return;
    var pr2=await _post('/api/app/clusters/'+encodeURIComponent(sreState.cluster)+'/automation/'+act,{});
    if(!pr2.ok){await chModal({title:'Failed',body:esc((pr2.data&&pr2.data.error)||'Could not update automation.'),confirm:'OK',single:true});}
    await loadSRE();return;
  }
});
document.addEventListener('change',function(e){if(e.target&&e.target.id==='prop-status'){sreState.propFilter=e.target.value;renderRemediation();}});

async function doRename(id,cur){
  var nn=await chModal({title:'Rename cluster',input:cur,confirm:'Save'});
  if(nn==null||nn===''||nn===cur)return;
  var rn=await _post('/api/app/clusters/'+encodeURIComponent(id)+'/rename',{name:nn});
  if(!rn.ok){await chModal({title:'Failed',body:'Could not rename the cluster.',confirm:'OK'});return;}
  if(new URLSearchParams(location.search).get('cluster')){loadDetail(id);}else{load();}
}

var _cbox=document.getElementById('clusters');
if(_cbox)_cbox.addEventListener('dblclick',function(e){
  var rn=e.target.closest&&e.target.closest('.rename-name');
  if(rn){e.preventDefault();doRename(rn.dataset.id,rn.dataset.name||'');}
});
if(_cbox)_cbox.addEventListener('click',async function(e){
  var inDetail=!!new URLSearchParams(location.search).get('cluster');
  var ib=e.target.closest&&e.target.closest('.info-btn');
  if(ib){
    var d=ib.dataset;var seen=d.seen?new Date(d.seen).toLocaleString():'never';
    var rows=[['Status',d.conn],['Cluster ID',d.id],['Environment',d.env||'-'],['Kubernetes',d.k8s||'-'],['Agent',d.agent||'-'],['Last seen',seen]];
    var body='<div style="text-align:left;font-size:12.5px;line-height:1.95;word-break:break-all">'+rows.map(function(r){return '<div><span style="color:var(--muted)">'+r[0]+':</span> '+esc(r[1])+'</div>';}).join('')+'</div>';
    await chModal({title:'Cluster info',body:body,confirm:'Close',single:true});return;
  }
  var del=e.target.closest&&e.target.closest('.del-btn');
  if(del){
    var rawNm=del.dataset.name||del.dataset.id,nm=esc(rawNm);
    if(!await chModal({title:'Delete cluster?',body:'Permanently delete <b>'+nm+'</b> and its history? This cannot be undone.',confirm:'Continue',danger:true}))return;
    var ok=await chModal({title:'Confirm deletion',body:'Type <b>'+nm+'</b> to confirm.',input:'',placeholder:rawNm,requireMatch:rawNm,confirm:'Delete permanently',danger:true});
    if(ok==null)return;
    var dr=await fetch('/api/app/clusters/'+encodeURIComponent(del.dataset.id),{method:'DELETE',headers:{'Accept':'application/json'}});
    if(!dr.ok){await chModal({title:'Failed',body:'Could not delete the cluster.',confirm:'OK'});return;}
    if(inDetail){location.href='/app';}else{load();}
    return;
  }
  var rb=e.target.closest&&e.target.closest('.rename-btn');
  if(rb){doRename(rb.dataset.id,rb.dataset.name||'');return;}
  var eb=e.target.closest&&e.target.closest('.env-btn');
  if(eb){
    var curEnv=eb.dataset.env||'';
    var nv=await chModal({title:'Set environment',body:'Common: <b>production</b>, <b>staging</b>, <b>dev</b>. <b>production</b> is protected from auto-remediation.',input:curEnv,confirm:'Save'});
    if(nv==null||nv===''||nv===curEnv)return;
    var er=await _post('/api/app/clusters/'+encodeURIComponent(eb.dataset.id)+'/environment',{environment:nv});
    if(!er.ok){await chModal({title:'Failed',body:(er.data&&er.data.error)||'Could not update the environment.',confirm:'OK'});return;}
    loadDetail(eb.dataset.id);
    return;
  }
  var cb=e.target.closest&&e.target.closest('.connect-btn');
  if(!cb)return;
  if(!await chModal({title:'Issue a new token?',body:'This creates a NEW agent token and invalidates any previous one for this cluster.',confirm:'Continue'}))return;
  var rr=await _post('/api/app/clusters/'+encodeURIComponent(cb.dataset.id)+'/connect',{});
  if(!rr.ok){await chModal({title:'Failed',body:(rr.data&&rr.data.error)||'Failed to issue token',confirm:'OK'});return;}
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

