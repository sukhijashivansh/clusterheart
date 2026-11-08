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
	"strconv"
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
			Kind        string `json:"kind"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in)
		kind := "kubernetes"
		if in.Kind == "server" {
			kind = "server"
		}
		name := strings.TrimSpace(in.Name)
		if name == "" {
			name = "my-cluster"
			if kind == "server" {
				name = "my-server"
			}
		}
		env := strings.TrimSpace(in.Environment)
		if env == "" {
			env = "production"
		}
		code, data, err := a.cpDo("POST", "/api/v1/clusters", map[string]any{
			"tenantId":    tenant,
			"clusterName": name,
			"environment": env,
			"kind":        kind,
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
			"kind":       kind,
			"install":    installSnippet(kind, reg.ClusterID, reg.AgentToken),
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
	var in struct {
		Kind string `json:"kind"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
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
		"install":    installSnippet(in.Kind, reg.ClusterID, reg.AgentToken),
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

// handleAppServerMetrics returns a server's downsampled history (CPU, memory,
// disk, load, network, GPUs) for the Timeline charts:
// GET /api/app/clusters/{id}/metrics?range=15m|1h|6h|24h|7d[&from=ms&to=ms].
func (a *app) handleAppServerMetrics(w http.ResponseWriter, r *http.Request) {
	_, tenant, ok := a.appSREUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	rg := q.Get("range")
	switch rg {
	case "15m", "1h", "6h", "24h", "7d":
	default:
		rg = "7d"
	}
	extra := ""
	// A zoomed-in window: from/to in Unix milliseconds.
	if f, t := q.Get("from"), q.Get("to"); f != "" && t != "" {
		if _, err := strconv.ParseInt(f, 10, 64); err == nil {
			if _, err := strconv.ParseInt(t, 10, 64); err == nil {
				extra = "&from=" + f + "&to=" + t
			}
		}
	}
	code, data, err := a.cpDo("GET", "/api/v1/clusters/"+url.PathEscape(r.PathValue("id"))+"/metrics?range="+rg+extra+"&tenantId="+url.QueryEscape(tenant), nil)
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
		reveal := name == "secret.revealed" && strings.HasPrefix(det, "cluster:"+id+" ")
		if reveal {
			ev["details"] = strings.TrimPrefix(det, "cluster:"+id+" ")
		}
		if ids[pid] || ids[iid] || policy || reveal {
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
// installSnippet returns the setup commands for a cluster or a server.
func installSnippet(kind, id, token string) string {
	if kind == "server" {
		return hostInstallSnippet(id, token)
	}
	return agentInstallSnippet(id, token)
}

// hostInstallSnippet is the one-line install of the host agent on a Linux
// server (see install-host.sh).
func hostInstallSnippet(id, token string) string {
	script := envOr("HOST_INSTALL_URL", "https://clusterheart-server.clusterheart.com/install-host.sh")
	extra := ""
	if cp := os.Getenv("AGENT_CONTROLPLANE_URL"); cp != "" && cp != "https://clusterheart-server.clusterheart.com/cp" {
		extra = " --url " + cp
	}
	return "curl -fsSL " + script + " | sudo sh -s -- --id " + id + " --token " + token + extra
}

// hostUpdateSnippet updates the host agent in place (same id and token).
func hostUpdateSnippet() string {
	return "curl -fsSL " + envOr("HOST_INSTALL_URL", "https://clusterheart-server.clusterheart.com/install-host.sh") + " | sudo sh -s -- --update"
}

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
	render(w, appTmpl, map[string]any{"Name": u.Name, "Email": u.Email, "ManifestURL": envOr("AGENT_MANIFEST_URL", "https://clusterheart.com/agent.yaml"),
		"HostUpdateCmd": hostUpdateSnippet()})
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
        <button class="menu-item accent" role="menuitem" id="add-server">+ Add server</button>
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
var HOST_UPDATE_CMD={{.HostUpdateCmd}};
// HOST_AGENT_LATEST is the host agent with GPU and network metrics.
var HOST_AGENT_LATEST='0.2.1';
function hostAgentAtLeast(v,want){return agentAtLeast(String(v||'').replace(/^host-/,''),want);}
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
  if(!connected){h+='<button class="kebab-item connect-btn" data-id="'+id+'" data-kind="'+esc(c.kind||'kubernetes')+'">Install</button>';}
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
    list=clOrdered(list);
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
      var metrics='<div class="cl-metric"><b>'+nodes+'</b><span>Nodes</span></div><div class="cl-metric"><b>'+pods+'</b><span>Pods'+fbadge(havePL?podFail:0)+'</span></div><div class="cl-metric"><b>'+jobsN+'</b><span>Jobs</span></div>';
      if(c.kind==='server'){
        var hs=L.host,dk=hs?fullestDisk(hs):null;
        metrics='<div class="cl-metric"><b>'+(hs?fmtPct(hs.cpuPercent):'—')+'</b><span>CPU</span></div>'+
          '<div class="cl-metric"><b>'+(hs?pctOf(hs.memUsedMi,hs.memTotalMi)+'%':'—')+'</b><span>Memory</span></div>'+
          '<div class="cl-metric"><b>'+(dk?pctOf(dk.usedMi,dk.totalMi)+'%':'—')+'</b><span>Disk'+fbadge(hs&&hs.problems?hs.problems.filter(function(x){return x.severity==='high';}).length:0)+'</span></div>'+
          (hs&&(hs.gpus||[]).length?'<div class="cl-metric"><b>'+fmtPct(gpuStats(hs.gpus).avg)+'</b><span>GPU ×'+hs.gpus.length+'</span></div>':'');
      }
      html+='<div class="cl-card" data-id="'+esc(c.clusterId||'')+'">'+
        '<div class="cl-head"><div style="min-width:0"><a class="cl-name" href="'+link+'">'+esc(c.name||c.clusterId)+'</a><div class="cl-env">'+(c.kind==='server'?'Server':'Kubernetes')+(c.environment?' · '+esc(c.environment):'')+'</div></div>'+
          '<span class="cl-status '+(connected?'on':'off')+'"><span class="dot"></span>'+(connected?'Connected':'Offline')+'</span></div>'+
        '<div class="cl-metrics">'+metrics+'</div>'+
        '<div class="cl-foot"><span class="cl-seen">'+ago(c.lastHeartbeat)+(c.kind==='server'&&c.agentVersion&&!hostAgentAtLeast(c.agentVersion,HOST_AGENT_LATEST)?' · <span class="cl-upd">agent update available</span>':'')+'</span>'+kebab(c)+'</div>'+
      '</div>';
    });
    html+='</div>';
    box.innerHTML=html;
  }catch(e){box.innerHTML='<p class="sub">Could not load clusters.</p>';}
}

// Cards can be lifted and rearranged: drag with the mouse, or long-press and
// drag on touch. The order is kept per browser in localStorage.
var CL_ORDER_KEY='ch-cl-order',TL_ORDER_KEY='ch-tl-order';
// ARRANGE lists every grid whose cards can be rearranged: the cluster cards and
// the Timeline charts. skip names the parts of a card that keep their own drag
// (a chart's drag-to-zoom).
var ARRANGE=[{grid:'cl-grid',card:'cl-card',key:CL_ORDER_KEY,skip:''},{grid:'tl-grid',card:'tl-card',key:TL_ORDER_KEY,skip:'.tl-chart'}];
function clOrdered(list){return arrOrdered(list,CL_ORDER_KEY,function(c){return c.clusterId;});}
function arrOrdered(list,key,idOf){
  var order=[];try{order=JSON.parse(localStorage.getItem(key)||'[]')||[];}catch(e){}
  var rank={};order.forEach(function(id,i){rank[id]=i;});
  return list.map(function(c,i){return {c:c,i:i};}).sort(function(a,b){
    var ra=rank[idOf(a.c)],rb=rank[idOf(b.c)];
    if(ra!=null&&rb!=null)return ra-rb;if(ra!=null)return -1;if(rb!=null)return 1;return a.i-b.i;
  }).map(function(x){return x.c;});
}
(function(){
  // Smooth by design: slot positions are measured once when a card lifts;
  // after that every card moves by transform only (no layout reads), updates
  // run once per frame, and the DOM is reordered only after the drop lands.
  var drag=null,suppressClick=false,EASE='transform .26s cubic-bezier(.2,.8,.2,1)';
  function cards(g){return Array.prototype.slice.call(g.children).filter(function(el){return el.dataset.id;});}
  function T(el,x,y){el.style.transform=(x||y)?'translate3d('+x+'px,'+y+'px,0)':'';}
  function start(){
    var d=drag;d.on=true;
    d.cards=cards(d.grid);d.from=d.cards.indexOf(d.card);d.to=d.from;
    d.slots=d.cards.map(function(el){var r=el.getBoundingClientRect();return {x:r.left+scrollX,y:r.top+scrollY,w:r.width,h:r.height};});
    var h=d.slots[d.from];d.ox=d.sx+scrollX-h.x;d.oy=d.sy+scrollY-h.y;
    d.cards.forEach(function(el){if(el!==d.card)el.style.transition=EASE;});
    d.grid.classList.add('arranging');d.card.classList.add('lifted');
    if(navigator.vibrate&&d.touch)try{navigator.vibrate(8);}catch(e){}
    tick();
  }
  function tick(){
    var d=drag;if(!d||!d.on)return;
    // nudge the page when the card is held near the top or bottom edge
    var e=60,v=d.y<e?-(e-d.y)/3:d.y>innerHeight-e?(d.y-innerHeight+e)/3:0;if(v)scrollBy(0,v);
    var px=d.x+scrollX-d.ox,py=d.y+scrollY-d.oy,h=d.slots[d.from];
    T(d.card,px-h.x,py-h.y);
    // the slot whose centre is nearest the lifted card's centre is the target
    var cx=px+h.w/2,cy=py+h.h/2,best=d.to,bd=Infinity;
    d.slots.forEach(function(s,k){var dd=Math.pow(s.x+s.w/2-cx,2)+Math.pow(s.y+s.h/2-cy,2);if(dd<bd){bd=dd;best=k;}});
    if(best!==d.to){
      d.to=best;
      var order=d.cards.filter(function(el){return el!==d.card;});order.splice(best,0,d.card);
      order.forEach(function(el,k){if(el===d.card)return;var o=d.slots[d.cards.indexOf(el)],n=d.slots[k];T(el,n.x-o.x,n.y-o.y);});
      d.order=order;
    }
    d.raf=requestAnimationFrame(tick);
  }
  function end(){
    if(!drag)return;
    var d=drag;drag=null;clearTimeout(d.timer);cancelAnimationFrame(d.raf);
    document.removeEventListener('pointermove',onMove);document.removeEventListener('pointerup',end);document.removeEventListener('pointercancel',end);
    if(!d.on)return;
    suppressClick=true;setTimeout(function(){suppressClick=false;},0);
    var c=d.card,h=d.slots[d.from],t=d.slots[d.to];
    c.classList.remove('lifted');c.classList.add('settling');c.style.transition=EASE+',scale .26s cubic-bezier(.2,.8,.2,1),box-shadow .26s';
    T(c,t.x-h.x,t.y-h.y);
    setTimeout(function(){
      var order=d.order||d.cards;
      d.cards.forEach(function(el){el.style.transition='none';});
      order.forEach(function(el){d.grid.appendChild(el);T(el,0,0);});
      d.grid.getBoundingClientRect();
      d.cards.forEach(function(el){el.style.transition='';});
      c.classList.remove('settling');d.grid.classList.remove('arranging');
      try{localStorage.setItem(d.conf.key,JSON.stringify(order.map(function(el){return el.dataset.id;})));}catch(e){}
    },270);
  }
  function onMove(e){
    if(!drag)return;
    if(!drag.on){
      var dist=Math.hypot(e.clientX-drag.sx,e.clientY-drag.sy);
      if(drag.touch){if(dist>8)end();return;} // moved before the long-press: let it scroll
      if(dist<5)return;
      start();
    }
    drag.x=e.clientX;drag.y=e.clientY;
  }
  document.addEventListener('pointerdown',function(e){
    if(drag)return;
    if(!e.target.closest)return;
    var conf=null,card=null;
    ARRANGE.forEach(function(c){var k=e.target.closest('.'+c.grid+' > .'+c.card);if(k&&!conf){conf=c;card=k;}});
    if(!card||e.button>0||e.target.closest('.kebab,button,input,textarea,select,a[href]:not(.cl-name)'+(conf.skip?','+conf.skip:'')))return;
    var grid=card.parentNode;if(cards(grid).length<2||grid.classList.contains('arranging'))return;
    drag={conf:conf,card:card,grid:grid,sx:e.clientX,sy:e.clientY,x:e.clientX,y:e.clientY,on:false,touch:e.pointerType!=='mouse'};
    if(drag.touch)drag.timer=setTimeout(function(){if(drag&&!drag.on)start();},320);
    document.addEventListener('pointermove',onMove);document.addEventListener('pointerup',end);document.addEventListener('pointercancel',end);
  });
  // While a card is lifted on touch, stop the page from scrolling under it.
  document.addEventListener('touchmove',function(e){if(drag&&drag.on){e.preventDefault();drag.x=e.touches[0].clientX;drag.y=e.touches[0].clientY;}},{passive:false});
  document.addEventListener('click',function(e){if(suppressClick&&e.target.closest&&e.target.closest('.cl-card,.tl-card')){e.preventDefault();e.stopPropagation();}},true);
  document.addEventListener('dragstart',function(e){if(e.target.closest&&e.target.closest('.cl-card,.tl-card'))e.preventDefault();});
  document.addEventListener('contextmenu',function(e){if(drag&&drag.touch)e.preventDefault();});
})();

function showInstall(d){
  var box=document.getElementById('clusters'),srv=d.kind==='server';
  var desc=srv?'Run this on the server (Linux with systemd). The agent is read-only, runs as its own user and only makes outbound HTTPS calls. <b>The token is shown only once.</b> To remove it later, run the same script with <b>--uninstall</b>.'
              :'Run this against the cluster you want to watch. The agent is read-only. <b>Save the AGENT_TOKEN now. It is shown only once.</b> <a href="/docs/connect-a-cluster.md" target="_blank" rel="noopener" style="color:var(--accent)">Full guide &rarr;</a>';
  var pre='<section class="card2"><div class="card2-head"><p class="card2-title">Install the agent</p><p class="card2-desc">'+desc+'</p></div><div class="card2-body">'+
    '<textarea class="nb" rows="'+(srv?3:6)+'" readonly onclick="this.select()">'+esc(d.install)+'</textarea>'+
    '<div class="nb-actions"><button class="act approve" id="copy-install">Copy</button><span class="nb-msg">'+(srv?'Server':'Cluster')+' ID: '+esc(d.clusterId)+'</span></div>'+
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
  await load();showInstall(r.data);
});

document.getElementById('add-server').addEventListener('click',async function(){
  if(_menu){_menu.classList.remove('open');if(_mb)_mb.setAttribute('aria-expanded','false');}
  var name=prompt('Name this server (e.g. web-01):','my-server');
  if(name===null)return;
  var r=await _post('/api/app/clusters',{name:name,kind:'server'});
  if(!r.ok){alert((r.data&&r.data.error)||'Failed to add server');return;}
  r.data.kind='server';
  if(new URLSearchParams(location.search).get('cluster')){location.href='/app';return;}
  await load();showInstall(r.data);
});

function pctOf(used,total){return total>0?Math.round(used/total*100):0;}
// fmtPct shows small usage precisely (0.3%, not 0%): one decimal under 10%,
// "<0.1%" for a trace, whole numbers above.
function fmtPct(v){v=+v||0;if(v<=0)return '0%';if(v<0.1)return '<0.1%';return (v<9.95?v.toFixed(1):Math.round(v))+'%';}
function gpuStats(gs){var n=gs.length||1,avg=gs.reduce(function(a,g){return a+(g.utilPercent||0);},0)/n,peak=gs.reduce(function(a,g){return Math.max(a,g.utilPeakPercent||g.utilPercent||0);},0);return {avg:avg,peak:peak};}
function fullestDisk(h){var best=null;(h.disks||[]).forEach(function(d){if(!best||d.usedMi/d.totalMi>best.usedMi/best.totalMi)best=d;});return best;}

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
  var hostAgent=c.kind==='server';
  if(!hostAgent&&!agentAtLeast(c.agentVersion,'0.2.0')){
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

function secretMaskedHtml(f){return '<span class="mono-chip sec-masked">'+esc(f.masked||'hidden')+'</span>';}

// secretHighlight escapes text and marks every redaction.
function secretHighlight(t){return esc(t).replace(/\[REDACTED\]/g,'<mark class="sec-mark">[REDACTED]</mark>');}

// secretDetail is the expanded view of one finding: exactly what leaked, where,
// the full (redacted) line and what to do about it.
function secretDetail(f,label,src){
  var parts=(f.owner||'').split('/'),kind=parts[0],name=parts.slice(1).join('/');
  var secret=(f.key?'<span class="mono-chip">'+esc(f.key)+'</span> = ':'')+
    '<span class="sec-val">'+secretMaskedHtml(f)+'</span>'+
    (f.length?' <span class="u-email">'+f.length+' characters</span>':'');
  var line=f.context||'';
  try{var parsed=JSON.parse(line);line=JSON.stringify(parsed,null,2);}catch(e){}
  var steps;
  if(f.source==='log')steps=['Rotate or revoke this credential now: anyone who can read these logs may have it.',
    'Find where the app prints '+(f.key?'<b>'+esc(f.key)+'</b>':'it')+' and stop logging it.',
    'This entry clears 24 hours after it was last seen.'];
  else if(f.source==='env'&&/^(Service|Host)\//.test(f.owner||''))steps=['Move the value out of the service settings into a file only root can read (chmod 600).',
    'Load it with <span class="mono-chip">EnvironmentFile=</span> or <span class="mono-chip">LoadCredential=</span> in the unit instead of <span class="mono-chip">Environment=</span>.',
    'Rotate it if the unit file is shared or kept in git.'];
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
    '<div class="sec-k" style="margin-top:14px">What to do</div><ol class="sec-steps">'+steps.map(function(x){return '<li>'+x+'</li>';}).join('')+'</ol></div>';
}

// ---------- Sub-tabs ----------
// A switch inside a tab, like Pods / Jobs: panes is [[key,label,html],...].
// The chosen pane is remembered per switch across re-renders.
var _subTab={};
function plural(n,one,many){return n+' '+(n===1?one:(many||one+'s'));}
function subTabs(id,panes){
  var cur=_subTab[id];
  if(!panes.some(function(p){return p[0]===cur;}))cur=panes[0][0];
  return '<section class="card2 subtabs" data-sub="'+id+'"><div class="card2-head"><div class="toolbar-row"><div class="seg" role="tablist">'+
    panes.map(function(p){return '<button class="seg-btn'+(p[0]===cur?' active':'')+'" role="tab" aria-selected="'+(p[0]===cur)+'" data-pane="'+p[0]+'">'+p[1]+'</button>';}).join('')+
    '</div></div></div>'+
    panes.map(function(p){return '<div class="subpane" data-pane="'+p[0]+'"'+(p[0]===cur?'':' hidden')+'>'+p[2]+'</div>';}).join('')+'</section>';
}
document.addEventListener('click',function(e){
  var b=e.target.closest&&e.target.closest('.subtabs > .card2-head .seg-btn');
  if(!b)return;
  var box=b.closest('.subtabs'),key=b.dataset.pane;
  _subTab[box.dataset.sub]=key;
  box.querySelectorAll(':scope > .card2-head .seg-btn').forEach(function(x){var on=x===b;x.classList.toggle('active',on);x.setAttribute('aria-selected',on);});
  box.querySelectorAll(':scope > .subpane').forEach(function(p){p.hidden=p.dataset.pane!==key;});
});

// ---------- Server detail ----------
// A plain Linux server watched by the host agent: Overview, Health, Secrets, AI.
function sevClass(p,warn,bad){return p>=bad?'bad':(p>=warn?'off':'on');}
// diskLabel says which disk the Disk card shows, in plain words: "/" is the
// main (system) disk; with several disks the card shows the fullest one.
function diskLabel(d,n){
  var name=d.mount==='/'?'Main disk':esc(d.mount);
  return name+' · '+fmtMem(d.usedMi)+' of '+fmtMem(d.totalMi)+(n>1?' · fullest of '+n:'');
}
function fmtUptime(sec){sec=sec||0;var d=Math.floor(sec/86400),h=Math.floor(sec%86400/3600),m=Math.floor(sec%3600/60);return d?d+'d '+h+'h':(h?h+'h '+m+'m':m+'m');}
function renderServerDetail(c,html,box){
  var id=c.clusterId,L=c.latest,h=L&&L.host;
  if(!h){
    box.innerHTML=html+'<section class="card2"><div class="card2-body"><p class="sub" style="margin:0">No data yet. Once the agent reports (every ~30s), this server shows up here. On the server, check it with <span class="mono-chip">systemctl status clusterheart-host-agent</span>.</p></div></section>';
    return;
  }
  var memP=pctOf(h.memUsedMi,h.memTotalMi),dk=fullestDisk(h),dkP=dk?pctOf(dk.usedMi,dk.totalMi):0,perCore=h.cpuCores?(h.load5||0)/h.cpuCores:0;
  var gpus=h.gpus||[],gs=gpuStats(gpus);
  html+='<div class="statgrid">'+
    stat('CPU','<span class="pill '+sevClass(h.cpuPercent||0,80,90)+'" style="font-size:inherit">'+fmtPct(h.cpuPercent)+'</span>',h.cpuCores+' cores · 30s average')+
    stat('Memory','<span class="pill '+sevClass(memP,80,90)+'" style="font-size:inherit">'+memP+'%</span>',fmtMem(h.memUsedMi)+' of '+fmtMem(h.memTotalMi))+
    stat('Disk',dk?'<span class="pill '+sevClass(dkP,80,90)+'" style="font-size:inherit">'+dkP+'%</span>':'—',dk?diskLabel(dk,(h.disks||[]).length):'no disks reported')+
    stat('Load',(h.load5||0).toFixed(2),perCore.toFixed(2)+' per core (5 min)')+
    (gpus.length?stat('GPU',fmtPct(gs.avg),gpus.length+' × '+esc(gpus[0].name)+' · 30s avg'+(gs.peak>gs.avg?' · peak '+fmtPct(gs.peak):'')):'')+
  '</div>';
  var probs=h.problems||[],secrets=L.secretFindings||[];
  html+='<div class="tabs" role="tablist">'+
    '<button class="tab active" data-tab="overview">Overview</button>'+
    '<button class="tab" data-tab="timeline">Timeline</button>'+
    '<button class="tab" data-tab="health">Health'+(probs.length?' <span class="tab-badge">'+probs.length+'</span>':'')+'</button>'+
    '<button class="tab" data-tab="secrets">Secrets'+(secrets.length?' <span class="tab-badge">'+secrets.length+'</span>':'')+'</button>'+
    '<button class="tab" data-tab="ai">AI</button>'+
  '</div>';
  // Overview
  var row=function(k,v){return '<div class="sec-k">'+k+'</div><div class="sec-v">'+v+'</div>';};
  var upd=hostAgentAtLeast(c.agentVersion,HOST_AGENT_LATEST)?'':
    '<section class="card2"><div class="card2-head"><p class="card2-title">Update the agent</p><p class="card2-desc">This server runs agent '+esc(c.agentVersion||'unknown')+'. Update it to see GPU and network usage. Run this on the server:</p></div>'+
    '<div class="card2-body"><textarea class="nb" rows="1" readonly onclick="this.select()">'+esc(HOST_UPDATE_CMD)+'</textarea><div class="nb-actions"><button class="act approve copy-setup">Copy</button></div></div></section>';
  var ovPanes=[['system','System','<div class="card2-body"><div class="sec-grid">'+
    row('Hostname','<span class="mono-chip">'+esc(h.hostname)+'</span>')+row('OS',esc(h.os||'-'))+row('Kernel',esc(h.kernel||'-'))+
    row('Architecture',esc(h.arch||'-'))+row('Uptime',fmtUptime(h.uptimeSec))+
    (h.swapTotalMi?row('Swap',fmtMem(h.swapUsedMi)+' of '+fmtMem(h.swapTotalMi)):'')+
  '</div></div>']];
  var gpuErr=(h.problems||[]).filter(function(x){return x.type==='GPUUnavailable';});
  if(!gpus.length&&gpuErr.length){
    ovPanes.push(['gpus','GPU','<div class="card2-body">'+
      gpuErr.map(function(x){return '<p class="sub" style="margin:0 0 6px">'+sevPill(x.severity)+' '+esc(x.message)+'</p>';}).join('')+'</div>']);
  }
  if(gpus.length){
    ovPanes.push(['gpus',plural(gpus.length,'GPU'),'<div class="tscroll"><table class="users ptable"><thead><tr><th>GPU</th><th>Use</th><th>Memory</th><th>Temp</th><th>Power</th></tr></thead><tbody>'+
      gpus.map(function(g){var mp=pctOf(g.memUsedMi,g.memTotalMi);
        return '<tr><td><div class="u-name pod-name">'+esc(g.name)+'</div><div class="u-email">GPU '+g.index+' · '+(g.vendor==='amd'?'AMD':'NVIDIA')+'</div></td>'+
          '<td class="u-time">'+fmtPct(g.utilPercent)+(g.utilPeakPercent>g.utilPercent?' <span class="u-email">peak '+fmtPct(g.utilPeakPercent)+'</span>':'')+'</td>'+
          '<td class="u-time">'+fmtMem(g.memUsedMi)+' of '+fmtMem(g.memTotalMi)+' <span class="u-email">'+mp+'%</span></td>'+
          '<td>'+(g.tempC?'<span class="pill '+sevClass(g.tempC,85,92)+'">'+Math.round(g.tempC)+'°C</span>':'—')+'</td>'+
          '<td class="u-time">'+(g.powerW?Math.round(g.powerW)+' W'+(g.powerLimitW?' of '+Math.round(g.powerLimitW)+' W':''):'—')+'</td></tr>';}).join('')+
      '</tbody></table></div>']);
  }
  if((h.disks||[]).length){
    ovPanes.push(['disks',plural(h.disks.length,'Disk'),'<div class="tscroll"><table class="users ptable"><thead><tr><th>Mount</th><th>Type</th><th>Used</th><th>Size</th><th>Full</th></tr></thead><tbody>'+
      h.disks.map(function(d){var p=pctOf(d.usedMi,d.totalMi);return '<tr><td><span class="u-name" style="font-size:13.5px">'+esc(d.mount)+'</span></td><td class="u-email">'+esc(d.fs)+'</td><td class="u-time">'+fmtMem(d.usedMi)+'</td><td class="u-time">'+fmtMem(d.totalMi)+'</td><td><span class="pill '+sevClass(p,80,90)+'">'+p+'%</span></td></tr>';}).join('')+
      '</tbody></table></div>']);
  }
  if((h.processes||[]).length){
    ovPanes.push(['procs',plural(h.processes.length,'Process','Processes'),'<div class="tscroll health-scroll"><table class="users ptable"><thead><tr><th>Process</th><th>User</th><th>CPU</th><th>Memory</th></tr></thead><tbody>'+
      h.processes.map(function(x){return '<tr><td><div class="u-name pod-name">'+esc(x.name)+'</div><div class="u-email">pid '+x.pid+'</div></td><td class="u-email">'+esc(x.user||'')+'</td><td class="u-time">'+(x.cpuPercent||0).toFixed(1)+'%</td><td class="u-time">'+fmtMem(x.memMi)+'</td></tr>';}).join('')+
      '</tbody></table></div>']);
  }
  // Health
  var hePanes=[];
  if(probs.length){
    var sevRank={high:0,medium:1};
    hePanes.push(['problems',plural(probs.length,'Problem'),'<div class="tscroll health-scroll"><table class="users ptable"><thead><tr><th>Problem</th><th>Severity</th><th>Detail</th></tr></thead><tbody>'+
      probs.slice().sort(function(a,b){return (sevRank[a.severity]||2)-(sevRank[b.severity]||2);}).map(function(x){return '<tr><td class="u-name" style="font-size:13.5px">'+esc(x.type)+'</td><td>'+sevPill(x.severity)+'</td><td class="u-time" style="white-space:normal">'+esc(x.message)+'</td></tr>';}).join('')+
      '</tbody></table></div>']);
  }
  var logs=(h.errorLogs||[]).slice().reverse();
  if(logs.length){
    hePanes.push(['errors',plural(logs.length,'Error'),'<div class="tscroll health-scroll"><table class="users ptable"><thead><tr><th>Service</th><th>Message</th><th>When</th></tr></thead><tbody>'+
      logs.map(function(l){return '<tr><td class="u-email">'+esc(l.unit||'system')+'</td><td class="u-time" style="white-space:normal;font-family:\'Geist Mono\',ui-monospace,monospace;font-size:12px">'+esc(l.message)+'</td><td class="u-time" style="white-space:nowrap" title="'+esc(l.time?new Date(l.time).toLocaleString():'')+'">'+ago(l.time)+'</td></tr>';}).join('')+
      '</tbody></table></div>']);
  }
  var ov=upd+subTabs('srv-ov',ovPanes);
  var he=hePanes.length?subTabs('srv-health',hePanes):'<section class="card2"><div class="card2-body"><p class="sub" style="margin:0"><span class="pill on">Healthy.</span> No problems or errors reported.</p></div></section>';
  html+='<div class="tabpanel active" data-panel="overview">'+ov+'</div>'+
    '<div class="tabpanel" data-panel="timeline"><div id="tl-root"></div></div>'+
    '<div class="tabpanel" data-panel="health">'+he+'</div>'+
    '<div class="tabpanel" data-panel="secrets">'+secretsSection(secrets,c)+'</div>'+
    '<div class="tabpanel" data-panel="ai">'+aiPanelHtml()+'</div>';
  box.innerHTML=html;
  sreState.cluster=id;sreState.data=null;rsState.cluster=id;
  aiWire(id);
  document.querySelectorAll('.tab').forEach(function(t){t.addEventListener('click',function(){
    _activeTab=t.dataset.tab;
    document.querySelectorAll('.tab').forEach(function(x){x.classList.toggle('active',x===t);});
    document.querySelectorAll('.tabpanel').forEach(function(pnl){pnl.classList.toggle('active',pnl.dataset.panel===t.dataset.tab);});
    if(t.dataset.tab==='timeline')loadTimeline(id);
  });});
  var keep=document.querySelector('.tab[data-tab="'+_activeTab+'"]');
  if(keep&&_activeTab!=='overview')keep.click();
  document.querySelectorAll('.copy-setup').forEach(function(btn){btn.addEventListener('click',function(){var ta=btn.closest('.card2').querySelector('textarea');if(ta){navigator.clipboard.writeText(ta.value);btn.textContent='Copied';}});});
}

// ---------- Server timeline ----------
// A week of history from the control plane (MongoDB time-series): one small
// chart per measure, one shared range picker above them, a hover crosshair,
// and a table view with every value.
// Each chart draws the bucket average as a line and the bucket peak as a
// thinner line above it, so a spike shorter than a bucket still shows at full
// height. Drag across a chart (or use "Zoom to peak") to zoom into a window;
// zoomed windows come back at up to 30s resolution.
// The range opens on the last one picked (1 hour the first time), so the page
// leads with what is happening now.
var TL_RANGE_KEY='ch-tl-range';
var tlState={cluster:null,range:(function(){try{var r=localStorage.getItem(TL_RANGE_KEY);if(/^(15m|1h|6h|24h|7d)$/.test(r||''))return r;}catch(e){}return '1h';})(),zoom:null,data:null,table:false,seq:0,charts:[]};
var TL_RANGES=[['15m','15 min'],['1h','1 hour'],['6h','6 hours'],['24h','24 hours'],['7d','7 days']];
var TL_RANGE_NAMES={'15m':'15 minutes','1h':'hour','6h':'6 hours','24h':'24 hours','7d':'7 days'};
var TL_MAX_SERIES=8;
function tlColor(i){return 'var(--s'+(i+1)+')';}
function tlPct(v){return v==null?'—':(v>0&&v<10?v.toFixed(1):Math.round(v))+'%';}
function tlRate(v){if(v==null)return '—';if(v>=1024)return (v/1024).toFixed(v>=10240?0:1)+' MB/s';return (v>=10?Math.round(v):v.toFixed(1))+' KB/s';}
function tlLoad(v){return v==null?'—':v.toFixed(2);}
function tlTemp(v){return v==null||!v?'—':Math.round(v)+'°C';}
function tlMap(arr,key,peak){return (arr||[]).map(function(p){return {t:new Date(p.t).getTime(),v:p[key]||0,pk:peak?(p[peak]||0):null};});}
function tlAny(series){return series.some(function(s){return s.pts.some(function(p){return p.v>0;});});}
function tlAxPct(v){return Math.round(v)+'%';}
function tlAxNum(v){return String(+v.toFixed(2));}
function tlAxRate(v){return v===0?'0':tlRate(v);}
function tlAxTemp(v){return Math.round(v)+'°C';}
// Rates get round numbers in the unit they are shown in (KB/s or MB/s).
function tlNiceRate(m){return m>=1024?tlNice(m/1024)*1024:tlNice(m);}
function tlNice(m){if(m<=0)return 1;var e=Math.pow(10,Math.floor(Math.log10(m))),f=m/e;return (f<=1?1:f<=2?2:f<=2.5?2.5:f<=5?5:10)*e;}
// Labels follow the window shown: dates for multi-day spans, the weekday when
// the window crosses midnight, seconds for 30s buckets.
function tlSpan(){var d=tlState.data;return d&&d.from?new Date(d.to).getTime()-new Date(d.from).getTime():86400000;}
function tlTimeLabel(t){
  var d=new Date(t),span=tlSpan(),stepSec=(tlState.data&&tlState.data.stepSec)||60;
  if(span>2*86400000)return d.toLocaleDateString([],{weekday:'short',day:'numeric',month:'short'})+', '+d.toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'});
  var o={hour:'2-digit',minute:'2-digit'};if(stepSec<60)o.second='2-digit';
  return d.toLocaleTimeString([],o)+(span>6*3600000||new Date(t).toDateString()!==new Date().toDateString()?' · '+d.toLocaleDateString([],{weekday:'short'}):'');
}
function tlRangeText(){
  if(!tlState.zoom)return 'the last '+(TL_RANGE_NAMES[tlState.range]||'24 hours');
  return tlTimeLabel(tlState.zoom.from)+' to '+tlTimeLabel(tlState.zoom.to);
}
function tlTicks(from,to){
  var out=[],d=new Date(from),span=to-from;
  if(span>3*86400000){
    d.setHours(0,0,0,0);d.setDate(d.getDate()+1);
    for(;d.getTime()<to;d.setDate(d.getDate()+1))out.push({t:d.getTime(),l:d.toLocaleDateString([],{weekday:'short',day:'numeric'})});
    return out;
  }
  // About six ticks on round minute boundaries.
  var opts=[1,2,5,10,15,30,60,120,180,240,360,720],stepMin=opts[opts.length-1];
  for(var i=0;i<opts.length;i++){if(span/(opts[i]*60000)<=7){stepMin=opts[i];break;}}
  d.setSeconds(0,0);
  var mins=d.getHours()*60+d.getMinutes(),next=Math.ceil((mins+1)/stepMin)*stepMin;
  d.setHours(0,next,0,0);
  for(;d.getTime()<to;d=new Date(d.getTime()+stepMin*60000))out.push({t:d.getTime(),l:d.toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'})});
  return out;
}

// tlCharts turns the history into chart specs; measures with no data are left out.
function tlCharts(d){
  var P=d.points||[],G=(d.gpus||[]).slice(0,TL_MAX_SERIES),out=[];
  out.push({id:'cpu',title:'CPU',fmt:tlPct,axis:tlAxPct,max:100,series:[{name:'CPU',pts:tlMap(P,'cpu','cpuMax')}]});
  out.push({id:'mem',title:'Memory',fmt:tlPct,axis:tlAxPct,max:100,series:[{name:'Memory',pts:tlMap(P,'mem','memMax')}]});
  out.push({id:'disk',title:'Disk',sub:'fullest disk',fmt:tlPct,axis:tlAxPct,max:100,series:[{name:'Disk',pts:tlMap(P,'disk')}]});
  out.push({id:'load',title:'Load',sub:'5 min',fmt:tlLoad,axis:tlAxNum,series:[{name:'Load',pts:tlMap(P,'load','loadMax')}]});
  var net=[{name:'In',pts:tlMap(P,'rx','rxMax')},{name:'Out',pts:tlMap(P,'tx','txMax')}];
  if(tlAny(net))out.push({id:'net',title:'Network',fmt:tlRate,axis:tlAxRate,nice:tlNiceRate,series:net});
  if(G.length){
    var gs=function(key,peak){return G.map(function(g){return {name:'GPU '+g.index,full:g.name,pts:tlMap(g.points,key,peak)};});};
    out.push({id:'gutil',title:'GPU utilization',fmt:tlPct,axis:tlAxPct,max:100,series:gs('util','utilMax')});
    out.push({id:'gmem',title:'GPU memory',fmt:tlPct,axis:tlAxPct,max:100,series:gs('mem','memMax')});
    var temp=gs('temp','tempMax');
    if(tlAny(temp))out.push({id:'gtemp',title:'GPU temperature',fmt:tlTemp,axis:tlAxTemp,series:temp});
  }
  out.forEach(function(ch){
    var seen={};ch.times=[];
    ch.series.forEach(function(s){s.pts.forEach(function(p){if(!seen[p.t]){seen[p.t]=1;ch.times.push(p.t);}});});
    ch.times.sort(function(a,b){return a-b;});
  });
  return out;
}

var TL_H=176,TL_PAD={r:12,t:12,b:26};
function tlGeom(ch,W,from,to){
  var max=ch.max;
  if(!max){var m=0;ch.series.forEach(function(s){s.pts.forEach(function(p){m=Math.max(m,p.v,p.pk||0);});});max=(ch.nice||tlNice)(m*1.05);}
  // The left margin fits the widest y-axis label.
  var widest=Math.max.apply(null,[0,0.5,1].map(function(f){return ch.axis(max*f).length;}));
  var padL=Math.max(30,Math.round(12+widest*6.6));
  var pw=W-padL-TL_PAD.r,ph=TL_H-TL_PAD.t-TL_PAD.b;
  return {W:W,max:max,pw:pw,ph:ph,from:from,to:to,padL:padL,
    x:function(t){return padL+(t-from)/(to-from)*pw;},
    y:function(v){return TL_PAD.t+ph-Math.min(Math.max(v,0),max)/max*ph;}};
}

function tlSvg(ch,g,step){
  var base=TL_PAD.t+g.ph,svg='<svg width="'+g.W+'" height="'+TL_H+'" viewBox="0 0 '+g.W+' '+TL_H+'" aria-hidden="true">';
  [0,0.5,1].forEach(function(f){
    var v=g.max*f,yy=g.y(v).toFixed(1);
    svg+='<line class="tl-gl'+(f===0?' tl-base':'')+'" x1="'+g.padL+'" x2="'+(g.W-TL_PAD.r)+'" y1="'+yy+'" y2="'+yy+'"/>'+
      '<text class="tl-ax" x="'+(g.padL-8)+'" y="'+yy+'" text-anchor="end" dominant-baseline="middle">'+esc(ch.axis(v))+'</text>';
  });
  var ticks=tlTicks(g.from,g.to),minGap=g.to-g.from>3*86400000?44:40,lastX=-1e9;
  ticks.forEach(function(tk){
    var xx=g.x(tk.t);
    if(xx<g.padL+16||xx>g.W-TL_PAD.r-16||xx-lastX<minGap)return;
    lastX=xx;
    svg+='<text class="tl-ax" x="'+xx.toFixed(1)+'" y="'+(TL_H-8)+'" text-anchor="middle">'+esc(tk.l)+'</text>';
  });
  // Agents report every ~30s, so a missing 30s bucket is jitter, not a gap.
  var gap=Math.max(step*1.5,90000),single=ch.series.length===1;
  ch.series.forEach(function(s,i){
    var segs=[],cur=[];
    s.pts.forEach(function(p){if(cur.length&&p.t-cur[cur.length-1].t>gap){segs.push(cur);cur=[];}cur.push(p);});
    if(cur.length)segs.push(cur);
    var col=tlColor(i);
    segs.forEach(function(seg){
      var d=seg.map(function(p,j){return (j?'L':'M')+g.x(p.t).toFixed(1)+' '+g.y(p.v).toFixed(1);}).join('');
      if(seg.length===1){svg+='<circle cx="'+g.x(seg[0].t).toFixed(1)+'" cy="'+g.y(seg[0].v).toFixed(1)+'" r="2.5" fill="'+col+'"/>';return;}
      // Peak envelope: band from the average up to the bucket peak, plus a thin
      // peak line, drawn only where the peak is above the average.
      var hasPk=seg.some(function(p){return p.pk!=null&&p.pk>p.v+0.01;});
      if(hasPk){
        var pd=seg.map(function(p,j){return (j?'L':'M')+g.x(p.t).toFixed(1)+' '+g.y(Math.max(p.pk||0,p.v)).toFixed(1);}).join('');
        var back=seg.slice().reverse().map(function(p){return 'L'+g.x(p.t).toFixed(1)+' '+g.y(p.v).toFixed(1);}).join('');
        svg+='<path d="'+pd+back+'Z" fill="'+col+'" fill-opacity="'+(single?'.16':'.1')+'" stroke="none"/>';
        svg+='<path class="tl-pkline" d="'+pd+'" fill="none" stroke="'+col+'" stroke-width="1.25" stroke-opacity=".7" stroke-dasharray="3 2" stroke-linejoin="round"/>';
      }
      if(single)svg+='<path d="'+d+'L'+g.x(seg[seg.length-1].t).toFixed(1)+' '+base+'L'+g.x(seg[0].t).toFixed(1)+' '+base+'Z" fill="'+col+'" fill-opacity=".1" stroke="none"/>';
      svg+='<path d="'+d+'" fill="none" stroke="'+col+'" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>';
    });
    var last=s.pts[s.pts.length-1];
    if(last)svg+='<circle class="tl-end" cx="'+g.x(last.t).toFixed(1)+'" cy="'+g.y(last.v).toFixed(1)+'" r="4" fill="'+col+'"/>';
    // Mark where the highest peak happened.
    var pp=tlNotablePeak(s);
    if(pp)svg+='<circle class="tl-pkdot" cx="'+g.x(pp.t).toFixed(1)+'" cy="'+g.y(tlPkVal(pp)).toFixed(1)+'" r="3.5" fill="none" stroke="'+col+'" stroke-width="1.5"/>';
  });
  svg+='<rect class="tl-sel" x="0" y="'+TL_PAD.t+'" width="0" height="'+g.ph+'" visibility="hidden"/>';
  svg+='<g class="tl-hover" visibility="hidden"><line class="tl-cross" x1="0" x2="0" y1="'+TL_PAD.t+'" y2="'+base+'"/>'+
    ch.series.map(function(s,i){return '<circle class="tl-hdot" r="4" fill="'+tlColor(i)+'"/>';}).join('')+'</g>';
  return svg+'</svg>';
}

function tlLast(s){return s.pts.length?s.pts[s.pts.length-1]:null;}
function tlPkVal(p){return p.pk!=null?Math.max(p.pk,p.v):p.v;}
function tlPeakPt(s){var m=null;s.pts.forEach(function(p){if(m==null||tlPkVal(p)>tlPkVal(m))m=p;});return m;}
function tlPeak(s){var p=tlPeakPt(s);return p?tlPkVal(p):null;}
// The series' peak across all series of a chart, for "Zoom to peak".
// Only a peak that stands out from the series' average is worth marking.
function tlChartPeak(ch){var best=null;ch.series.forEach(function(s){var p=tlNotablePeak(s);if(p&&(best==null||tlPkVal(p)>tlPkVal(best)))best=p;});return best;}
function tlNotablePeak(s){
  var p=tlPeakPt(s);if(!p||!s.pts.length)return null;
  var avg=s.pts.reduce(function(a,q){return a+q.v;},0)/s.pts.length,pk=tlPkVal(p);
  return pk>0&&pk>avg*1.2&&pk-avg>0.05?p:null;
}

function tlCardHtml(ch,ci,W,from,to,step){
  var g=tlGeom(ch,W,from,to),one=ch.series.length===1,s0=ch.series[0],last=tlLast(s0);
  var cp=tlChartPeak(ch),stepMs=step;
  // Zooming only helps when the peak sits in a bucket wider than 30s.
  var zoomBtn=cp&&stepMs>30000?' <button class="tl-pkzoom" data-ci="'+ci+'" title="Zoom in around this peak">at '+esc(tlTimeLabel(cp.t))+' &rsaquo;</button>':
    (cp?' <span class="tl-pkat">at '+esc(tlTimeLabel(cp.t))+'</span>':'');
  // Name the peak's window ("7-day peak") so an old spike never reads as now.
  var pw=tlState.zoom?'peak':({'15m':'15-min','1h':'1-hour','6h':'6-hour','24h':'24-hour','7d':'7-day'}[tlState.range]||'')+' peak';
  var sum=one&&last?'<p class="tl-sum"><b>'+esc(ch.fmt(last.v))+'</b> now · '+pw+' <b>'+esc(ch.fmt(tlPeak(s0)))+'</b>'+zoomBtn+'</p>':
    (cp?'<p class="tl-sum">'+pw+' <b>'+esc(ch.fmt(tlPkVal(cp)))+'</b>'+zoomBtn+'</p>':'');
  var legend='';
  if(!one){
    legend='<div class="tl-legend">'+ch.series.map(function(s,i){var l=tlLast(s);
      return '<span class="tl-li" title="'+esc(s.full||s.name)+'"><span class="tl-key" style="background:'+tlColor(i)+'"></span>'+esc(s.name)+(s.full?' <span class="tl-li-sub">'+esc(s.full)+'</span>':'')+(l?' <b>'+esc(ch.fmt(l.v))+'</b>':'')+'</span>';}).join('')+'</div>';
  }
  var label=ch.title+' over '+tlRangeText()+(one&&last?', now '+ch.fmt(last.v)+', peak '+ch.fmt(tlPeak(s0)):'')+'. Use the arrow keys to read values; drag across the chart to zoom in.';
  return '<section class="card2 tl-card" data-id="'+esc(ch.id)+'"><div class="card2-head tl-head"><p class="card2-title">'+esc(ch.title)+(ch.sub?' <span class="tl-title-sub">'+esc(ch.sub)+'</span>':'')+'</p>'+sum+'</div>'+
    '<div class="tl-chart" data-ci="'+ci+'" tabindex="0" role="img" aria-label="'+esc(label)+'">'+tlSvg(ch,g,step)+'<div class="tl-tip" hidden></div></div>'+legend+'</section>';
}

function tlTableHtml(d,charts){
  var cols=[];
  charts.forEach(function(ch){ch.series.forEach(function(s){cols.push({h:ch.series.length===1?ch.title:ch.title.replace('GPU ','')+' · '+s.name,fmt:ch.fmt,pts:s.pts});});});
  var byT={};
  cols.forEach(function(c,ci){c.pts.forEach(function(p){(byT[p.t]=byT[p.t]||{})[ci]=p;});});
  var times=Object.keys(byT).map(Number).sort(function(a,b){return b-a;});
  var html='<section class="card2"><div class="tscroll tl-tscroll"><table class="users ptable tl-table"><thead><tr><th>Time</th>'+cols.map(function(c){return '<th>'+esc(c.h)+'</th>';}).join('')+'</tr></thead><tbody>';
  times.forEach(function(t){
    html+='<tr><td class="u-time" style="white-space:nowrap">'+esc(tlTimeLabel(t))+'</td>'+cols.map(function(c,ci){var p=byT[t][ci];
      return '<td class="u-time tl-num">'+(p?esc(c.fmt(p.v))+(p.pk!=null&&p.pk>p.v?' <span class="u-email">(peak '+esc(c.fmt(p.pk))+')</span>':''):'—')+'</td>';}).join('')+'</tr>';
  });
  return html+'</tbody></table></div></section>';
}

function tlRender(){
  var root=document.getElementById('tl-root');
  if(!root)return;
  if(root.querySelector('.tl-grid.arranging')){clearTimeout(tlRender._t);tlRender._t=setTimeout(tlRender,400);return;}
  var d=tlState.data;
  var bar='<div class="tl-bar"><div class="seg tl-ranges" role="group" aria-label="Time range">'+
    TL_RANGES.map(function(r){var on=!tlState.zoom&&tlState.range===r[0];return '<button class="seg-btn tl-range'+(on?' active':'')+'" data-r="'+r[0]+'" aria-pressed="'+on+'">'+r[1]+'</button>';}).join('')+
    '<button class="seg-btn tl-custom'+(tlState.zoom?' active':'')+'" aria-expanded="false" aria-pressed="'+!!tlState.zoom+'">Custom</button>'+
    '</div>'+tlPickerHtml()+'<div class="tl-bar-r">'+
    (tlState.zoom?'<span class="tl-zoomed">'+esc(tlRangeText())+(d&&d.stepSec?' · '+(d.stepSec<60?d.stepSec+'s':Math.round(d.stepSec/60)+' min')+' buckets':'')+'</span><button class="tl-view tl-unzoom">Reset zoom</button>':
      (d&&d.stepSec?'<span class="tl-hint">'+(d.stepSec<60?d.stepSec+'s':Math.round(d.stepSec/60)+' min')+' buckets · drag a chart to zoom</span>':''))+
    '<button class="tl-view" aria-pressed="'+tlState.table+'">'+(tlState.table?'Charts':'Table')+'</button></div></div>';
  if(!d){root.innerHTML=bar+'<section class="card2"><div class="card2-body"><p class="sub" style="margin:0">Loading history…</p></div></section>';tlWireBar(root);return;}
  if(d.error){root.innerHTML=bar+'<section class="card2"><div class="card2-body"><p class="sub" style="margin:0">Could not load history'+(d.error?': '+esc(d.error):'')+'.</p></div></section>';tlWireBar(root);return;}
  if(!(d.points||[]).length){
    root.innerHTML=bar+'<section class="card2"><div class="card2-body"><p class="sub" style="margin:0">No history yet. It fills in as the agent reports, every 30 seconds, and keeps 7 days.</p></div></section>';
    tlWireBar(root);return;
  }
  var charts=arrOrdered(tlCharts(d),TL_ORDER_KEY,function(ch){return ch.id;});tlState.charts=charts;
  var from=new Date(d.from).getTime(),to=new Date(d.to).getTime(),step=(d.stepSec||60)*1000;
  if(tlState.table){root.innerHTML=bar+tlTableHtml(d,charts);tlWireBar(root);return;}
  root.innerHTML=bar+'<div class="tl-grid"></div>';
  var grid=root.querySelector('.tl-grid');
  // Measure the card's inner width once the grid is in the page.
  grid.innerHTML='<section class="card2 tl-card"><div class="tl-chart tl-probe"></div></section>';
  var W=Math.max(260,Math.floor(grid.querySelector('.tl-probe').clientWidth));
  grid.innerHTML=charts.map(function(ch,ci){return tlCardHtml(ch,ci,W,from,to,step);}).join('');
  tlWireBar(root);
  grid.querySelectorAll('.tl-pkzoom').forEach(function(b){b.addEventListener('click',function(){
    var p=tlChartPeak(charts[+b.dataset.ci]);if(!p)return;
    // 30 minutes around the peak bucket: fine enough for 30s resolution.
    var mid=p.t+step/2;tlZoom(mid-15*60000,mid+15*60000);
  });});
  grid.querySelectorAll('.tl-chart').forEach(function(el){tlWireHover(el,charts[+el.dataset.ci],tlGeom(charts[+el.dataset.ci],W,from,to));});
}

// tlPickerHtml is the Custom range popover: any window inside the 7 days of
// history, to the minute. It opens on the current window.
function tlDT(ms){var d=new Date(ms-new Date(ms).getTimezoneOffset()*60000);return d.toISOString().slice(0,16);}
function tlPickerHtml(){
  var now=Date.now(),d=tlState.data,from,to;
  if(tlState.zoom){from=tlState.zoom.from;to=tlState.zoom.to;}
  else if(d&&d.from){from=new Date(d.from).getTime();to=new Date(d.to).getTime();}
  else{to=now;from=now-3600000;}
  var min=tlDT(now-7*86400000+60000),max=tlDT(now);
  return '<div class="tl-picker" hidden><div class="tl-pick-row">'+
    '<label>From<input type="datetime-local" class="toolbar-input tl-from" min="'+min+'" max="'+max+'" value="'+tlDT(from)+'"></label>'+
    '<label>To<input type="datetime-local" class="toolbar-input tl-to" min="'+min+'" max="'+max+'" value="'+tlDT(to)+'"></label>'+
    '</div><p class="tl-pick-err" hidden></p><div class="tl-pick-actions"><span class="tl-pick-note">Up to 7 days back</span><button class="tl-view tl-pick-cancel">Cancel</button><button class="act approve tl-pick-apply">Apply</button></div></div>';
}
function tlWireBar(root){
  var cb=root.querySelector('.tl-custom'),pk=root.querySelector('.tl-picker');
  if(cb&&pk){
    function close(){pk.hidden=true;cb.setAttribute('aria-expanded','false');}
    cb.addEventListener('click',function(e){e.stopPropagation();pk.hidden=!pk.hidden;cb.setAttribute('aria-expanded',String(!pk.hidden));if(!pk.hidden)pk.querySelector('.tl-from').focus();});
    pk.addEventListener('click',function(e){e.stopPropagation();});
    document.addEventListener('click',function out(e){if(!document.body.contains(pk)){document.removeEventListener('click',out);return;}close();});
    pk.addEventListener('keydown',function(e){if(e.key==='Escape'){close();cb.focus();}if(e.key==='Enter')pk.querySelector('.tl-pick-apply').click();});
    pk.querySelector('.tl-pick-cancel').addEventListener('click',close);
    pk.querySelector('.tl-pick-apply').addEventListener('click',function(){
      var f=new Date(pk.querySelector('.tl-from').value).getTime(),t=new Date(pk.querySelector('.tl-to').value).getTime(),err=pk.querySelector('.tl-pick-err');
      var msg=isNaN(f)||isNaN(t)?'Pick both times.':t<=f?'"To" must be after "From".':f>Date.now()?'"From" is in the future.':'';
      if(msg){err.textContent=msg;err.hidden=false;return;}
      close();tlZoom(f,Math.min(t,Date.now()));
    });
  }
  root.querySelectorAll('.tl-range').forEach(function(b){b.addEventListener('click',function(){
    if(tlState.range===b.dataset.r&&!tlState.zoom)return;
    tlState.range=b.dataset.r;tlState.zoom=null;
    try{localStorage.setItem(TL_RANGE_KEY,tlState.range);}catch(e){}
    root.querySelectorAll('.tl-range').forEach(function(x){x.classList.toggle('active',x===b);x.setAttribute('aria-pressed',x===b);});
    loadTimeline(tlState.cluster);
  });});
  var v=root.querySelector('.tl-view:not(.tl-unzoom)');
  if(v)v.addEventListener('click',function(){tlState.table=!tlState.table;tlRender();});
  var uz=root.querySelector('.tl-unzoom');
  if(uz)uz.addEventListener('click',function(){tlState.zoom=null;loadTimeline(tlState.cluster);});
}

function tlZoom(from,to){
  var now=Date.now();
  if(to>now){from-=to-now;to=now;}
  tlState.zoom={from:Math.round(from),to:Math.round(to)};
  loadTimeline(tlState.cluster);
}

function tlWireHover(el,ch,g){
  var hover=el.querySelector('.tl-hover'),cross=el.querySelector('.tl-cross'),dots=el.querySelectorAll('.tl-hdot'),tip=el.querySelector('.tl-tip'),idx=-1;
  function show(i){
    if(i<0||i>=ch.times.length)return;
    idx=i;var t=ch.times[i],xx=g.x(t);
    cross.setAttribute('x1',xx);cross.setAttribute('x2',xx);
    var rows='';
    ch.series.forEach(function(s,si){
      var p=null;for(var k=0;k<s.pts.length;k++){if(s.pts[k].t===t){p=s.pts[k];break;}}
      var dot=dots[si];
      if(p){dot.setAttribute('cx',xx);dot.setAttribute('cy',g.y(p.v));dot.setAttribute('visibility','visible');}else dot.setAttribute('visibility','hidden');
      rows+='<div class="tl-row"><span class="tl-key" style="background:'+tlColor(si)+'"></span><b>'+(p?esc(ch.fmt(p.v)):'—')+'</b> <span class="tl-sn">'+esc(s.name)+(p&&p.pk!=null&&p.pk>p.v?' avg':'')+'</span>'+
        (p&&p.pk!=null&&p.pk>p.v?' <span class="tl-pk">peak '+esc(ch.fmt(p.pk))+'</span>':'')+'</div>';
    });
    tip.innerHTML='<div class="tl-tt">'+esc(tlTimeLabel(t))+'</div>'+rows;
    hover.setAttribute('visibility','visible');tip.hidden=false;
    var tw=tip.offsetWidth,left=xx+14;
    if(left+tw>g.W-4)left=xx-14-tw;
    tip.style.left=Math.max(4,left)+'px';
  }
  function hide(){hover.setAttribute('visibility','hidden');tip.hidden=true;idx=-1;}
  function nearest(px){
    var t=g.from+(px-g.padL)/g.pw*(g.to-g.from),best=0,bd=Infinity;
    for(var i=0;i<ch.times.length;i++){var dd=Math.abs(ch.times[i]-t);if(dd<bd){bd=dd;best=i;}}
    return best;
  }
  // Drag across the chart to zoom into that window.
  var sel=el.querySelector('.tl-sel'),dragX=null;
  function clampX(px){return Math.min(Math.max(px,g.padL),g.padL+g.pw);}
  function tAt(px){return g.from+(clampX(px)-g.padL)/g.pw*(g.to-g.from);}
  el.addEventListener('pointerdown',function(e){
    if(e.button!==0)return;
    var r=el.getBoundingClientRect();dragX=clampX(e.clientX-r.left);
    try{el.setPointerCapture(e.pointerId);}catch(_){}
  });
  el.addEventListener('pointermove',function(e){
    var r=el.getBoundingClientRect(),px=e.clientX-r.left;
    show(nearest(px));
    if(dragX!=null&&sel){
      var a=Math.min(dragX,clampX(px)),b=Math.max(dragX,clampX(px));
      sel.setAttribute('x',a);sel.setAttribute('width',b-a);sel.setAttribute('visibility',b-a>3?'visible':'hidden');
    }
  });
  el.addEventListener('pointerup',function(e){
    if(dragX==null)return;
    var r=el.getBoundingClientRect(),px=clampX(e.clientX-r.left),a=Math.min(dragX,px),b=Math.max(dragX,px);
    dragX=null;if(sel)sel.setAttribute('visibility','hidden');
    if(b-a>=8)tlZoom(tAt(a),tAt(b));
  });
  el.addEventListener('pointercancel',function(){dragX=null;if(sel)sel.setAttribute('visibility','hidden');});
  el.addEventListener('pointerleave',function(){if(dragX==null)hide();});
  el.addEventListener('focus',function(){show(ch.times.length-1);});
  el.addEventListener('blur',hide);
  el.addEventListener('keydown',function(e){
    if(e.key==='ArrowLeft'){e.preventDefault();show(Math.max(0,(idx<0?ch.times.length:idx)-1));}
    else if(e.key==='ArrowRight'){e.preventDefault();show(Math.min(ch.times.length-1,idx+1));}
    else if(e.key==='Escape')hide();
  });
}

// loadTimeline fetches the history. While it loads, the previous charts stay
// on screen, dimmed, so the layout does not jump.
async function loadTimeline(id){
  if(!id)return;
  if(tlState.cluster!==id){tlState.cluster=id;tlState.data=null;tlState.zoom=null;}
  var root=document.getElementById('tl-root');
  if(root&&!root.firstChild)tlRender();
  if(root)root.classList.add('tl-loading');
  var seq=++tlState.seq;
  try{
    var q='range='+encodeURIComponent(tlState.range)+(tlState.zoom?'&from='+tlState.zoom.from+'&to='+tlState.zoom.to:'');
    var r=await fetch('/api/app/clusters/'+encodeURIComponent(id)+'/metrics?'+q,{headers:{'Accept':'application/json'}});
    var d=await r.json().catch(function(){return {};});
    if(seq!==tlState.seq)return;
    tlState.data=r.ok?d:{error:d.error||('HTTP '+r.status)};
  }catch(e){if(seq!==tlState.seq)return;tlState.data={error:'network error'};}
  root=document.getElementById('tl-root');
  if(root)root.classList.remove('tl-loading');
  tlRender();
}
function tlVisible(){var p=document.querySelector('.tabpanel[data-panel="timeline"]');return !!(p&&p.classList.contains('active')&&!document.hidden);}
var _tlResize=null;
window.addEventListener('resize',function(){clearTimeout(_tlResize);_tlResize=setTimeout(function(){if(tlVisible()&&tlState.data)tlRender();},150);});
// Refresh once a minute while the Timeline is on screen.
setInterval(function(){if(tlVisible())loadTimeline(tlState.cluster);},60000);

// ---------- Latency check ----------
// The cluster's agent sends a few GET requests straight to one pod and reports
// where the time goes: TCP connect, TLS handshake, server wait (until the first
// byte) and download. Results are kept per cluster while the page is open.
var latState={cluster:null,pods:[],form:{},results:{},running:false};
var LAT_PHASES=[['connectMs','Connect','TCP handshake'],['tlsMs','TLS','TLS handshake'],['waitMs','Server wait','until the first byte'],['transferMs','Download','first byte to last']];
var LAT_SYSTEM_NS=/^(kube-|clusterheart-|amazon-|aws-|gke-|calico-|tigera-|cert-manager$|ingress-nginx$|karpenter$)/;
function latMs(v){v=v||0;if(v>=1000)return (v/1000).toFixed(2)+' s';return (v>=100?Math.round(v):v.toFixed(1))+' ms';}
function latSize(b){b=b||0;return b>=1048576?'1 MiB+':(b>=1024?(b/1024).toFixed(1)+' KiB':b+' B');}

function latencySection(c,pods){
  latState.cluster=c.clusterId;
  if(!agentAtLeast(c.agentVersion,'0.3.0')){
    return '<section class="card2"><div class="card2-head"><p class="card2-title">Latency check</p><p class="card2-desc">Restart the agent to turn this on (it fetches the latest version on start):</p></div>'+
      '<div class="card2-body"><textarea class="nb" rows="1" readonly onclick="this.select()">kubectl -n clusterheart-sre rollout restart deployment/clusterheart-agent</textarea>'+
      '<div class="nb-actions"><button class="act approve copy-setup">Copy</button></div></div></section>';
  }
  var run=(pods||[]).filter(function(p){return p.phase==='Running'&&(p.owner||'').indexOf('Job/')!==0;});
  run.sort(function(a,b){var sa=LAT_SYSTEM_NS.test(a.namespace),sb=LAT_SYSTEM_NS.test(b.namespace);return (sa-sb)||a.namespace.localeCompare(b.namespace)||a.name.localeCompare(b.name);});
  latState.pods=run;
  if(!run.length)return '<section class="card2"><div class="card2-body"><p class="sub" style="margin:0">No running pods to check.</p></div></section>';
  var f=latState.form[c.clusterId]||{},sel=f.pod||(run[0].namespace+'/'+run[0].name);
  var groups={},order=[];
  run.forEach(function(p){if(!groups[p.namespace]){groups[p.namespace]=[];order.push(p.namespace);}groups[p.namespace].push(p);});
  var opts=order.map(function(ns){return '<optgroup label="'+esc(ns)+'">'+groups[ns].map(function(p){var v=p.namespace+'/'+p.name;return '<option value="'+esc(v)+'"'+(v===sel?' selected':'')+'>'+esc(p.namespace+' / '+p.name)+'</option>';}).join('')+'</optgroup>';}).join('');
  var scheme=f.scheme||'http',n=f.n||5;
  return '<section class="card2"><div class="card2-head"><p class="card2-title">Latency check</p></div><div class="card2-body">'+
    '<div class="lat-form">'+
      '<label class="lat-f lat-pod"><span>Pod</span><select id="lat-pod" class="toolbar-input">'+opts+'</select></label>'+
      '<label class="lat-f lat-port"><span>Port</span><input id="lat-port" class="toolbar-input" inputmode="numeric" placeholder="auto" value="'+(f.port||'')+'"></label>'+
      '<label class="lat-f lat-path"><span>Path</span><input id="lat-path" class="toolbar-input" value="'+esc(f.path||'/')+'" spellcheck="false"></label>'+
      '<div class="lat-f"><span>Protocol</span><div class="seg" id="lat-scheme">'+['http','https'].map(function(s){return '<button class="seg-btn'+(s===scheme?' active':'')+'" data-v="'+s+'">'+s+'</button>';}).join('')+'</div></div>'+
      '<label class="lat-f lat-n"><span>Requests</span><select id="lat-n" class="toolbar-input">'+[1,3,5,10,20].map(function(k){return '<option'+(k===n?' selected':'')+'>'+k+'</option>';}).join('')+'</select></label>'+
      '<button class="act approve lat-run" id="lat-run">Measure</button>'+
    '</div><p class="u-email lat-status" id="lat-status" hidden></p></div></section>'+
    '<div id="lat-out">'+(latState.results[c.clusterId]?latResultHtml(latState.results[c.clusterId]):'')+'</div>';
}

function latForm(){
  var pod=document.getElementById('lat-pod');if(!pod)return null;
  var s=document.querySelector('#lat-scheme .seg-btn.active');
  return {pod:pod.value,port:(document.getElementById('lat-port').value||'').trim(),path:(document.getElementById('lat-path').value||'/').trim()||'/',
    scheme:s?s.dataset.v:'http',n:+document.getElementById('lat-n').value||5};
}

function latWire(){
  var run=document.getElementById('lat-run');if(!run)return;
  document.querySelectorAll('#lat-scheme .seg-btn').forEach(function(b){b.addEventListener('click',function(){
    document.querySelectorAll('#lat-scheme .seg-btn').forEach(function(x){x.classList.toggle('active',x===b);});
  });});
  run.addEventListener('click',latRun);
  ['lat-port','lat-path'].forEach(function(id){var el=document.getElementById(id);if(el)el.addEventListener('keydown',function(e){if(e.key==='Enter')latRun();});});
}

function latStatus(text,bad){
  var el=document.getElementById('lat-status');if(!el)return;
  el.hidden=!text;el.textContent=text||'';el.style.color=bad?'var(--red)':'';
}

async function latRun(){
  if(latState.running)return;
  var f=latForm(),id=latState.cluster;if(!f||!id)return;
  var port=0;
  if(f.port){port=parseInt(f.port,10);if(!(port>=1&&port<=65535)){latStatus('Port must be a number between 1 and 65535, or empty for the pod\'s own port.',true);return;}}
  if(f.path.charAt(0)!=='/')f.path='/'+f.path;
  latState.form[id]=f;
  var parts=f.pod.split('/'),btn=document.getElementById('lat-run');
  latState.running=true;if(btn){btn.disabled=true;btn.textContent='Measuring…';}
  latStatus('Waiting for the agent…');
  var out=document.getElementById('lat-out');if(out)out.classList.add('tl-loading');
  try{
    var r=await _post('/api/app/clusters/'+encodeURIComponent(id)+'/probes',{namespace:parts[0],pod:parts.slice(1).join('/'),port:port,scheme:f.scheme,path:f.path,samples:f.n});
    if(!r.ok||!r.data||!r.data.id){latStatus((r.data&&r.data.error)||'Could not start the check.',true);return;}
    var deadline=Date.now()+110000,res=null;
    while(Date.now()<deadline){
      await new Promise(function(ok){setTimeout(ok,1000);});
      var pr=await fetch('/api/app/probes/'+encodeURIComponent(r.data.id),{headers:{'Accept':'application/json'},cache:'no-store'});
      var d=await pr.json().catch(function(){return null;});
      if(!pr.ok){latStatus((d&&d.error)||'The check expired.',true);return;}
      if(d&&d.done){res=d;break;}
      if(d&&d.started)latStatus('Measuring '+f.n+' request'+(f.n===1?'':'s')+' from inside the cluster…');
    }
    if(!res){latStatus('The agent didn\'t answer in time.',true);return;}
    if(res.error){latStatus(res.error,true);return;}
    if(res.result&&res.result.error){latStatus(res.result.error,true);return;}
    res.result.pod=f.pod;
    if(id===latState.cluster){latState.results[id]=res.result;latStatus('');}
    out=document.getElementById('lat-out');if(out&&id===latState.cluster)out.innerHTML=latResultHtml(res.result);
  }catch(e){latStatus('Could not run the check.',true);}
  finally{
    latState.running=false;
    btn=document.getElementById('lat-run');if(btn){btn.disabled=false;btn.textContent='Measure';}
    out=document.getElementById('lat-out');if(out)out.classList.remove('tl-loading');
  }
}

// latVerdict names the phase that takes most of the time, in plain words.
function latVerdict(r){
  if(!r.okCount)return '';
  var parts=[['connect',r.connect.avg,'connecting (TCP)'],['tls',r.tls.avg,'the TLS handshake'],['wait',r.wait.avg,'the app working on the request (server wait)'],['transfer',r.transfer.avg,'downloading the response']];
  parts.sort(function(a,b){return b[1]-a[1];});
  var total=r.total.avg||1,share=Math.round(parts[0][1]/total*100);
  return 'Most of the time ('+share+'%) is '+parts[0][2]+'.';
}

function latResultHtml(r){
  var ok=r.okCount||0,count=r.count||0;
  var head='<section class="card2"><div class="card2-head tl-head lat-head"><p class="card2-title">Results</p><p class="tl-sum"><span class="mono-chip">GET '+esc(r.url||'')+'</span> · <b>'+ok+'</b> of '+count+' OK</p></div>';
  if(!ok){
    var errs=(r.samples||[]).map(function(s){return s.err;}).filter(Boolean);
    return head+'<div class="card2-body"><p class="sub" style="margin:0;color:var(--red)">'+esc(errs[0]||'No request succeeded.')+'</p></div></section>';
  }
  var tile=function(label,st,sub){return stat(label,latMs(st.avg),(st.min!==st.max?latMs(st.min)+' – '+latMs(st.max):'')+(sub?(st.min!==st.max?' · ':'')+sub:''));};
  var html=head+'<div class="card2-body">'+
    '<div class="statgrid lat-stats">'+tile('Connect',r.connect,'TCP')+tile('TLS',r.tls,r.tls.max?'handshake':'none (http)')+tile('Server wait',r.wait,'')+tile('First byte',r.firstByte,'')+tile('Total',r.total,'')+'</div>'+
    '<p class="lat-verdict">'+esc(latVerdict(r))+'</p>';
  // One bar per request, split into the four phases, on a shared scale.
  var maxT=0;(r.samples||[]).forEach(function(s){if(!s.err)maxT=Math.max(maxT,s.totalMs||0);});
  html+='<div class="lat-legend">'+LAT_PHASES.map(function(p,i){return '<span><i style="background:var(--s'+(i+1)+')"></i>'+p[1]+'</span>';}).join('')+'</div><div class="lat-bars">';
  (r.samples||[]).forEach(function(s,i){
    html+='<div class="lat-row"><span class="lat-lbl">#'+(i+1)+(s.status?' · '+s.status:'')+'</span>';
    if(s.err){html+='<span class="lat-err">'+esc(s.err)+'</span></div>';return;}
    var segs=LAT_PHASES.map(function(p,k){return [p,k,s[p[0]]||0];}).filter(function(x){return x[2]>0;});
    html+='<span class="lat-track">'+segs.map(function(x){
      return '<span class="lat-seg" style="width:'+(maxT?(x[2]/maxT*100):0).toFixed(2)+'%;background:var(--s'+(x[1]+1)+')" title="'+esc(x[0][1]+': '+latMs(x[2]))+'"></span>';}).join('')+
      '</span><span class="lat-val">'+latMs(s.totalMs)+'</span></div>';
  });
  html+='</div></div>';
  // Every value, for reading exact numbers.
  html+='<div class="tscroll"><table class="users ptable"><thead><tr><th>#</th><th>Status</th><th>Connect</th><th>TLS</th><th>Server wait</th><th>Download</th><th>First byte</th><th>Total</th><th>Size</th></tr></thead><tbody>'+
    (r.samples||[]).map(function(s,i){
      if(s.err)return '<tr><td class="u-time">'+(i+1)+'</td><td colspan="8" class="u-time" style="white-space:normal;color:var(--red)">'+esc(s.err)+'</td></tr>';
      return '<tr><td class="u-time">'+(i+1)+'</td><td class="u-time">'+s.status+'</td>'+
        [s.connectMs,s.tlsMs,s.waitMs,s.transferMs,s.firstByteMs,s.totalMs].map(function(v){return '<td class="u-time tl-num">'+latMs(v)+'</td>';}).join('')+
        '<td class="u-time tl-num">'+latSize(s.bytes)+'</td></tr>';
    }).join('')+'</tbody></table></div></section>';
  return html;
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
    if(c.kind==='server'){renderServerDetail(c,html,box);return;}
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
        '<td><span class="pill '+(ok?'on':'off')+(x.phase==='Pending'?' blip':'')+'"><span class="dot"></span>'+esc(x.phase||'')+'</span></td>'+
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
        '<td><span class="pill '+cls+(st==='Running'?' blip':'')+'">'+esc(st)+'</span></td>'+
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
      '<button class="tab" data-tab="latency">Latency</button>'+
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
    '<div class="tabpanel" data-panel="latency">'+latencySection(c,all)+'</div>'+
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
    latWire();
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
var EVENT_LABELS={'proposal.created':'Fix proposed','proposal.blocked':'Blocked by policy','proposal.approved':'Fix approved','proposal.rejected':'Fix rejected','remediation.executed':'Fix applied','remediation.failed':'Fix failed','policy.updated':'Policy changed','secret.revealed':'Secret revealed','policy.paused':'Automation paused','policy.resumed':'Automation resumed'};
function eventPill(ev){
  var c=/blocked|rejected|failed|paused|revealed/.test(ev)?'bad':(/executed|approved|resumed/.test(ev)?'on':'off');
  return '<span class="pill '+c+'">'+esc(EVENT_LABELS[ev]||ev)+'</span>';
}
function renderActivity(){
  var p=srePanel('activity'),d=sreState.data;if(!p||!d)return;
  var props={};(d.proposals||[]).forEach(function(x){props[x.id]=x;});
  var incs={};(d.incidents||[]).forEach(function(x){incs[x.id]=x;});
  var list=(d.activity||[]).slice().sort(function(a,b){return new Date(b.at||0)-new Date(a.at||0);});
  var html='<section class="card2"><div class="card2-head"><div class="toolbar-row"><div><p class="card2-title">Activity</p></div><button class="act revoke sre-refresh" style="margin-left:auto">Refresh</button></div></div>';
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
  var rr=await _post('/api/app/clusters/'+encodeURIComponent(cb.dataset.id)+'/connect',{kind:cb.dataset.kind||'kubernetes'});
  if(!rr.ok){await chModal({title:'Failed',body:(rr.data&&rr.data.error)||'Failed to issue token',confirm:'OK'});return;}
  rr.data.kind=cb.dataset.kind;
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

