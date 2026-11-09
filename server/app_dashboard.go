package main

// Dashboard API (backend-for-frontend). The browser only talks to this server
// (same origin, session cookie); this server calls the SRE control plane with
// the shared admin token, scoped to the tenant of the workspace the user works
// in (see team.go). No kubeconfig ever touches us: a read-only agent runs
// inside each cluster or server and streams health upward. The page itself is
// in app_page.go.

import (
	"bytes"
	"context"
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
	_, ws, ok := a.wsUser(w, r, r.Method != http.MethodGet)
	if !ok {
		return
	}
	tenant := ws.Tenant

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
		if !ws.Access.Logs {
			data = stripLogs(data)
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
	_, ws, ok := a.wsUser(w, r, true)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, 400, map[string]any{"error": "missing cluster id"})
		return
	}
	tenant := ws.Tenant
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
	_, ws, ok := a.wsUser(w, r, true)
	if !ok {
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
	tenant := ws.Tenant
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
	_, ws, ok := a.wsUser(w, r, true)
	if !ok {
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
	tenant := ws.Tenant
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
	_, ws, ok := a.wsUser(w, r, true)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, 400, map[string]any{"error": "missing cluster id"})
		return
	}
	tenant := ws.Tenant
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
	u, ws, ok := a.wsUser(w, r, false)
	return u, ws.Tenant, ok
}

// appSREManager is appSREUser for actions that change things: only the
// workspace owner and team admins may take them.
func (a *app) appSREManager(w http.ResponseWriter, r *http.Request) (*User, string, bool) {
	u, ws, ok := a.wsUser(w, r, true)
	return u, ws.Tenant, ok
}

// wsAllowed is wsUser for features the owner can switch off per person:
// "ai", "logs" or "apply".
func (a *app) wsAllowed(w http.ResponseWriter, r *http.Request, feature string) (*User, workspace, bool) {
	u, ws, ok := a.wsUser(w, r, false)
	if !ok {
		return nil, ws, false
	}
	allowed, what := false, ""
	switch feature {
	case "ai":
		allowed, what = ws.Access.AI, "use the AI assistant"
	case "logs":
		allowed, what = ws.Access.Logs, "read logs"
	case "apply":
		allowed, what = ws.Access.Apply, "apply changes"
	}
	if !allowed {
		writeJSON(w, 403, map[string]any{"error": "You can't " + what + " in this workspace. Ask the owner to turn it on."})
		return nil, workspace{}, false
	}
	return u, ws, true
}

// stripLogs removes everything log-derived from a control-plane cluster list,
// for people without log access: failing-pod log tails, host error logs, and
// the log line a secret finding was seen in.
func stripLogs(data []byte) []byte {
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if dec.Decode(&doc) != nil {
		return data
	}
	list, _ := doc["clusters"].([]any)
	for _, c := range list {
		cm, _ := c.(map[string]any)
		latest, _ := cm["latest"].(map[string]any)
		if latest == nil {
			continue
		}
		delete(latest, "podLogs")
		if h, ok := latest["host"].(map[string]any); ok {
			delete(h, "errorLogs")
		}
		fs, _ := latest["secretFindings"].([]any)
		for _, f := range fs {
			if fm, ok := f.(map[string]any); ok && fm["source"] == "log" {
				delete(fm, "context")
			}
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return data
	}
	return out
}

// wsUser resolves the signed-in, approved user and the workspace they work in.
// With manage set, team members without the admin role get a 403.
func (a *app) wsUser(w http.ResponseWriter, r *http.Request, manage bool) (*User, workspace, bool) {
	u := a.sessionUser(r)
	if u == nil || !u.Approved {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return nil, workspace{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	ws := a.workspaceFor(ctx, u)
	if manage && !ws.canManage() {
		writeJSON(w, 403, map[string]any{"error": "Only the workspace owner or a team admin can do this."})
		return nil, workspace{}, false
	}
	return u, ws, true
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
	// Diagnosis is AI; turning it into a fix proposal needs apply access.
	feature := "ai"
	if r.PathValue("action") == "propose" {
		feature = "apply"
	}
	_, ws, ok := a.wsAllowed(w, r, feature)
	if !ok {
		return
	}
	tenant := ws.Tenant
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
	u, ws, ok := a.wsAllowed(w, r, "apply")
	if !ok {
		return
	}
	tenant := ws.Tenant
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
	_, tenant, ok := a.appSREManager(w, r)
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
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	_ = a.store.setSeen(ctx, u.Email)
	notice := a.store.getNotice(ctx)
	ws := a.workspaceFor(ctx, u)
	// The menu lists every workspace this person can open (theirs + teams).
	type space struct {
		Owner, Name string
		Active      bool
	}
	spaces := []space{{"", "My workspace", ws.Owner == strings.ToLower(u.Email)}}
	if teams, err := a.store.teamsOf(ctx, u.Email); err == nil {
		for _, t := range teams {
			name := workspaceName("", t.Owner)
			if o, err := a.store.find(ctx, t.Owner); err == nil && o != nil {
				name = workspaceName(o.Name, o.Email)
			}
			spaces = append(spaces, space{t.Owner, name, ws.Owner == t.Owner})
		}
	}
	shared := ws.Owner != strings.ToLower(u.Email)
	render(w, appTmpl, map[string]any{"Name": u.Name, "Email": u.Email, "ManifestURL": envOr("AGENT_MANIFEST_URL", "https://clusterheart.com/agent.yaml"),
		"HostUpdateCmd": hostUpdateSnippet(), "Notice": notice.Message, "NoticeKey": notice.UpdatedAt.UnixMilli(), "Workspace": ws.Name, "Shared": shared, "Role": ws.Role, "CanManage": ws.canManage(), "Access": ws.Access, "Spaces": spaces})
}
