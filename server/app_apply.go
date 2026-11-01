package main

// Rightsizing "Apply" for the browser dashboard. The web server never touches a
// cluster: Apply records a SetResources proposal on the SRE control plane as the
// logged-in user and approves it, and the opt-in remediation controller running
// in the user's cluster picks it up and applies it. Policy still decides
// (pause, cooldown, managed namespaces, ...), and every step lands in Activity.

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

// remediationManifest is deploy/k8s/remediation/remediation.yaml, served so the
// install commands can `kubectl apply -f` it (kept in sync by a test).
//
//go:embed remediation.yaml
var remediationManifest string

// hostInstallScript is install-host.sh, served for the one-line server install.
//
//go:embed install-host.sh
var hostInstallScript string

func handleHostInstallScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, hostInstallScript)
}

func handleRemediationManifest(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = io.WriteString(w, remediationManifest)
}

// handleAppEnableRemediation issues a remediation token for one of the user's
// clusters and returns the commands that install the controller:
// POST /api/app/clusters/{id}/remediation. Issuing a new token invalidates the
// previous one.
func (a *app) handleAppEnableRemediation(w http.ResponseWriter, r *http.Request) {
	_, tenant, ok := a.appSREUser(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	code, data, err := a.cpDo("POST", "/api/v1/clusters/"+url.PathEscape(id)+"/remediation-token?tenantId="+url.QueryEscape(tenant), nil)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "control plane unreachable"})
		return
	}
	if code == 404 {
		writeJSON(w, 404, map[string]any{"error": "cluster not found"})
		return
	}
	var out struct {
		RemediationToken string `json:"remediationToken"`
	}
	if code != 201 || json.Unmarshal(data, &out) != nil || out.RemediationToken == "" {
		writeJSON(w, 502, map[string]any{"error": "could not enable Apply for this cluster"})
		return
	}
	writeJSON(w, 200, map[string]any{"clusterId": id, "install": remediationInstallSnippet(id, out.RemediationToken)})
}

func remediationInstallSnippet(clusterID, token string) string {
	cpURL := envOr("AGENT_CONTROLPLANE_URL", "https://clusterheart-server.clusterheart.com/cp")
	manifest := envOr("REMEDIATION_MANIFEST_URL", "https://clusterheart-server.clusterheart.com/remediation.yaml")
	return "kubectl -n clusterheart-sre delete secret clusterheart-remediation --ignore-not-found\n" +
		"kubectl -n clusterheart-sre create secret generic clusterheart-remediation \\\n" +
		"  --from-literal=CONTROLPLANE_URL=" + cpURL + " \\\n" +
		"  --from-literal=CLUSTER_ID=" + clusterID + " \\\n" +
		"  --from-literal=REMEDIATION_TOKEN=" + token + "\n" +
		"kubectl apply -f " + manifest + "\n" +
		"kubectl -n clusterheart-sre rollout restart deployment/clusterheart-remediation"
}

var k8sNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// handleAppRightsize applies a Rightsizing suggestion:
// POST /api/app/clusters/{id}/rightsize with
// {"kind":"Deployment","namespace":"web","name":"api","cpuMilli":250,"memMi":512}.
// It creates a SetResources proposal as the user and approves it in one step
// (the click plus confirmation is the human approval).
func (a *app) handleAppRightsize(w http.ResponseWriter, r *http.Request) {
	u, tenant, ok := a.appSREUser(w, r)
	if !ok {
		return
	}
	var in struct {
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		CPUMilli  int    `json:"cpuMilli"`
		MemMi     int    `json:"memMi"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil ||
		!k8sNameRe.MatchString(in.Namespace) || !k8sNameRe.MatchString(in.Name) || (in.CPUMilli <= 0 && in.MemMi <= 0) {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	params := map[string]string{}
	if in.CPUMilli > 0 {
		params["cpu"] = strconv.Itoa(in.CPUMilli)
	}
	if in.MemMi > 0 {
		params["memory"] = strconv.Itoa(in.MemMi)
	}
	actor := "user:" + u.Email
	q := "?tenantId=" + url.QueryEscape(tenant)
	code, data, err := a.cpDo("POST", "/api/v1/clusters/"+url.PathEscape(r.PathValue("id"))+"/proposals"+q, map[string]any{
		"action":    "SetResources",
		"target":    map[string]string{"kind": in.Kind, "namespace": in.Namespace, "name": in.Name},
		"params":    params,
		"rationale": "Rightsizing: requests set to 6h peak + 20%",
		"createdBy": actor,
	})
	if err != nil || code != 201 {
		cpRelay(w, code, data, err)
		return
	}
	var p struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if json.Unmarshal(data, &p) != nil || p.ID == "" {
		writeJSON(w, 502, map[string]any{"error": "unexpected control plane response"})
		return
	}
	if p.Status != "proposed" {
		cpRelay(w, 200, data, nil) // blocked by policy (reasons included), or already approved
		return
	}
	code, data, err = a.cpDo("POST", "/api/v1/proposals/"+url.PathEscape(p.ID)+"/approve"+q, map[string]any{"approver": actor})
	cpRelay(w, code, data, err)
}
