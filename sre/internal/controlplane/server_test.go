package controlplane

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"clusterheart/sre/internal/contract"
)

const adminTok = "admin-secret-test-token"

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := NewServer(NewMemStore(), NewMemIncidentStore(),
		NewMemRemediationStore(), NewMemPolicyStore(), NewMemAuditStore(),
		NewMemDiagnosisStore(), NewMemRunbookStore(), adminTok, nil)
	return httptest.NewServer(srv.Routes())
}

func do(t *testing.T, method, url, bearer, clusterID string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if clusterID != "" {
		req.Header.Set("X-Cluster-ID", clusterID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func register(t *testing.T, base, tenant, name string) contract.RegisterResponse {
	t.Helper()
	code, body := do(t, http.MethodPost, base+"/api/v1/clusters", adminTok, "",
		contract.RegisterRequest{TenantID: tenant, ClusterName: name, Environment: "test"})
	if code != http.StatusCreated {
		t.Fatalf("register %s: want 201, got %d: %s", name, code, body)
	}
	var rr contract.RegisterResponse
	if err := json.Unmarshal(body, &rr); err != nil {
		t.Fatal(err)
	}
	if rr.ClusterID == "" || rr.AgentToken == "" {
		t.Fatalf("register returned empty id/token: %s", body)
	}
	return rr
}

func sampleBatch() contract.TelemetryBatch {
	return contract.TelemetryBatch{
		APIVersion: contract.APIVersion,
		Snapshots: []contract.ClusterSnapshot{{
			CollectedAt: time.Now().UTC(),
			Nodes:       contract.NodeSummary{Total: 3, Ready: 3},
			Pods:        contract.PodSummary{Total: 10, Running: 9, Failed: 1},
		}},
	}
}

func TestRegisterRequiresAdmin(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	// no token
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/clusters", "", "",
		contract.RegisterRequest{TenantID: "t1", ClusterName: "c1"}); code != http.StatusUnauthorized {
		t.Fatalf("want 401 without admin token, got %d", code)
	}
	// wrong token
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/clusters", "nope", "",
		contract.RegisterRequest{TenantID: "t1", ClusterName: "c1"}); code != http.StatusUnauthorized {
		t.Fatalf("want 401 with wrong admin token, got %d", code)
	}
}

func TestAgentHappyPath(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	c := register(t, ts.URL, "tenantA", "prod-eks")

	if code, body := do(t, http.MethodPost, ts.URL+"/api/v1/telemetry", c.AgentToken, c.ClusterID, sampleBatch()); code != http.StatusAccepted {
		t.Fatalf("telemetry: want 202, got %d: %s", code, body)
	}
	hb := contract.Heartbeat{APIVersion: contract.APIVersion, AgentVersion: "0.1.0", SentAt: time.Now()}
	if code, body := do(t, http.MethodPost, ts.URL+"/api/v1/agents/heartbeat", c.AgentToken, c.ClusterID, hb); code != http.StatusOK {
		t.Fatalf("heartbeat: want 200, got %d: %s", code, body)
	}
	// operator read-back shows the latest snapshot and connected=true
	code, body := do(t, http.MethodGet, ts.URL+"/api/v1/clusters?tenantId=tenantA", adminTok, "", nil)
	if code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", code)
	}
	var out struct {
		Clusters []contract.ClusterStatus `json:"clusters"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Clusters) != 1 || !out.Clusters[0].Connected || out.Clusters[0].Latest == nil {
		t.Fatalf("unexpected cluster status: %s", body)
	}
	if out.Clusters[0].Latest.Pods.Total != 10 {
		t.Fatalf("snapshot not stored correctly: %s", body)
	}
}

// TestTenantIsolation is the security-critical test: an agent credential issued
// for cluster A must NOT be usable to write telemetry as cluster B, and listing
// a tenant must never return another tenant's clusters.
func TestTenantIsolation(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	a := register(t, ts.URL, "tenantA", "cluster-a")
	b := register(t, ts.URL, "tenantB", "cluster-b")

	// A's token + B's cluster id => must be rejected (no cross-cluster writes).
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/telemetry", a.AgentToken, b.ClusterID, sampleBatch()); code != http.StatusUnauthorized {
		t.Fatalf("cross-cluster write: want 401, got %d", code)
	}
	// B's data must not leak into tenantA's listing.
	_, body := do(t, http.MethodGet, ts.URL+"/api/v1/clusters?tenantId=tenantA", adminTok, "", nil)
	var out struct {
		Clusters []contract.ClusterStatus `json:"clusters"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Clusters) != 1 || out.Clusters[0].ClusterID != a.ClusterID {
		t.Fatalf("tenant isolation leak: %s", body)
	}
	_ = b
}

func TestTelemetryRejectsBadVersionAndEmpty(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	c := register(t, ts.URL, "t", "c")

	bad := sampleBatch()
	bad.APIVersion = "v999"
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/telemetry", c.AgentToken, c.ClusterID, bad); code != http.StatusConflict {
		t.Fatalf("bad apiVersion: want 409, got %d", code)
	}
	empty := contract.TelemetryBatch{APIVersion: contract.APIVersion}
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/telemetry", c.AgentToken, c.ClusterID, empty); code != http.StatusBadRequest {
		t.Fatalf("empty batch: want 400, got %d", code)
	}
}

// TestIngestionRedaction proves the control plane re-redacts agent-supplied
// text, so a non-compliant agent cannot land a raw secret in an incident.
func TestIngestionRedaction(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	c := register(t, ts.URL, "acme", "prod")

	batch := contract.TelemetryBatch{APIVersion: contract.APIVersion, Snapshots: []contract.ClusterSnapshot{{
		CollectedAt: time.Now().UTC(),
		PodProblems: []contract.PodProblem{{
			Namespace: "web", Pod: "api-1", Owner: "Deployment/api", Container: "api",
			Type: contract.ProblemCrashLoopBackOff, Message: "auth Bearer sk-abc123DEF failed",
		}},
	}}}
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/telemetry", c.AgentToken, c.ClusterID, batch); code != http.StatusAccepted {
		t.Fatalf("telemetry: want 202, got %d", code)
	}
	_, body := do(t, http.MethodGet, ts.URL+"/api/v1/incidents?tenantId=acme&status=open", adminTok, "", nil)
	var out struct {
		Incidents []Incident `json:"incidents"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Incidents) != 1 || len(out.Incidents[0].Evidence) != 1 {
		t.Fatalf("unexpected incidents: %s", body)
	}
	msg := out.Incidents[0].Evidence[0].Message
	if !bytes.Contains([]byte(msg), []byte("[REDACTED]")) || bytes.Contains([]byte(msg), []byte("sk-abc123DEF")) {
		t.Fatalf("secret not redacted at ingestion: %q", msg)
	}
}

func TestTelemetryBatchTooLarge(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	c := register(t, ts.URL, "t", "c")
	big := contract.TelemetryBatch{APIVersion: contract.APIVersion}
	for i := 0; i <= maxSnapshotsPerBatch; i++ {
		big.Snapshots = append(big.Snapshots, contract.ClusterSnapshot{CollectedAt: time.Now()})
	}
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/telemetry", c.AgentToken, c.ClusterID, big); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized batch: want 413, got %d", code)
	}
}

func TestProposeFromDiagnosisHTTP(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	c := register(t, ts.URL, "tenantP", "eks")

	// Telemetry with one actionable (CrashLoopBackOff) and one non-actionable
	// (ImagePullBackOff) fault → two incidents.
	batch := contract.TelemetryBatch{APIVersion: contract.APIVersion, Snapshots: []contract.ClusterSnapshot{{
		CollectedAt: time.Now().UTC(),
		PodProblems: []contract.PodProblem{
			{Namespace: "web", Pod: "api-1", Owner: "Deployment/api", Container: "api", Type: contract.ProblemCrashLoopBackOff, Message: "boom"},
			{Namespace: "web", Pod: "img-1", Owner: "Deployment/img", Container: "img", Type: contract.ProblemImagePullBackOff, Message: "pull denied"},
		},
	}}}
	if code, body := do(t, http.MethodPost, ts.URL+"/api/v1/telemetry", c.AgentToken, c.ClusterID, batch); code != http.StatusAccepted {
		t.Fatalf("telemetry: want 202, got %d: %s", code, body)
	}
	// Allow the web namespace so the proposal isn't blocked.
	pol := map[string]any{"mode": "approval-required", "allowedActions": []string{"RolloutRestart"},
		"namespaceAllowlist": []string{"web"}, "protectedEnvironments": []string{"production"},
		"maxConcurrent": 5, "perWorkloadCooldownSeconds": 0, "paused": false}
	if code, body := do(t, http.MethodPut, ts.URL+"/api/v1/policy?tenantId=tenantP", adminTok, "", pol); code != http.StatusOK {
		t.Fatalf("policy: want 200, got %d: %s", code, body)
	}

	_, body := do(t, http.MethodGet, ts.URL+"/api/v1/incidents?tenantId=tenantP&status=open", adminTok, "", nil)
	var il struct {
		Incidents []Incident `json:"incidents"`
	}
	_ = json.Unmarshal(body, &il)
	var crashID, imgID string
	for _, i := range il.Incidents {
		switch i.Type {
		case contract.ProblemCrashLoopBackOff:
			crashID = i.ID
		case contract.ProblemImagePullBackOff:
			imgID = i.ID
		}
	}
	if crashID == "" || imgID == "" {
		t.Fatalf("expected both incidents, got %s", body)
	}

	// Actionable incident → 201 Created, a proposed RolloutRestart on its workload.
	code, body := do(t, http.MethodPost, ts.URL+"/api/v1/incidents/"+crashID+"/propose?tenantId=tenantP", adminTok, "", nil)
	if code != http.StatusCreated {
		t.Fatalf("propose(crashloop): want 201, got %d: %s", code, body)
	}
	var p RemediationProposal
	_ = json.Unmarshal(body, &p)
	if p.Status != StatusProposed || p.Action != ActionRolloutRestart || p.Target.Name != "api" {
		t.Fatalf("unexpected proposal: %+v", p)
	}

	// Non-actionable incident → 422, no proposal.
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/incidents/"+imgID+"/propose?tenantId=tenantP", adminTok, "", nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("propose(imagepull): want 422, got %d", code)
	}
}

func TestStatsHTTP(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	c := register(t, ts.URL, "tenantS", "eks")
	batch := contract.TelemetryBatch{APIVersion: contract.APIVersion, Snapshots: []contract.ClusterSnapshot{{
		CollectedAt: time.Now().UTC(),
		PodProblems: []contract.PodProblem{
			{Namespace: "web", Pod: "a", Owner: "Deployment/api", Container: "api", Type: contract.ProblemCrashLoopBackOff, Message: "x"},
			{Namespace: "web", Pod: "b", Owner: "Deployment/img", Container: "img", Type: contract.ProblemImagePullBackOff, Message: "y"},
		},
	}}}
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/telemetry", c.AgentToken, c.ClusterID, batch); code != http.StatusAccepted {
		t.Fatalf("telemetry want 202, got %d", code)
	}
	code, body := do(t, http.MethodGet, ts.URL+"/api/v1/stats?tenantId=tenantS", adminTok, "", nil)
	if code != http.StatusOK {
		t.Fatalf("stats want 200, got %d: %s", code, body)
	}
	var st struct {
		Clusters  map[string]int            `json:"clusters"`
		Incidents map[string]map[string]int `json:"incidents"`
	}
	// incidents has mixed types; decode loosely.
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	_ = json.Unmarshal(body, &st)
	if st.Clusters["total"] != 1 {
		t.Fatalf("want 1 cluster, got %v", st.Clusters)
	}
	inc := raw["incidents"].(map[string]any)
	sev := inc["openBySeverity"].(map[string]any)
	if int(sev["high"].(float64)) != 1 || int(sev["medium"].(float64)) != 1 {
		t.Fatalf("expected 1 high + 1 medium open, got %v", sev)
	}
	if int(inc["total"].(float64)) != 2 {
		t.Fatalf("expected 2 incidents, got %v", inc["total"])
	}
}

func TestRunbookSaveValidateAndPrecedence(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	c := register(t, ts.URL, "tenantR", "eks")
	batch := contract.TelemetryBatch{APIVersion: contract.APIVersion, Snapshots: []contract.ClusterSnapshot{{
		CollectedAt: time.Now().UTC(),
		PodProblems: []contract.PodProblem{{Namespace: "web", Pod: "a", Owner: "Deployment/api", Container: "api", Type: contract.ProblemCrashLoopBackOff, Message: "x"}},
	}}}
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/telemetry", c.AgentToken, c.ClusterID, batch); code != http.StatusAccepted {
		t.Fatalf("telemetry want 202, got %d", code)
	}
	// Allow the namespace + Scale action so a runbook-driven Scale isn't blocked.
	pol := map[string]any{"mode": "approval-required", "allowedActions": []string{"RolloutRestart", "Scale"},
		"namespaceAllowlist": []string{"web"}, "maxConcurrent": 5, "maxReplicas": 10}
	if code, _ := do(t, http.MethodPut, ts.URL+"/api/v1/policy?tenantId=tenantR", adminTok, "", pol); code != http.StatusOK {
		t.Fatal("policy put failed")
	}

	// Invalid runbook (unknown action) is rejected.
	bad := map[string]any{"incidentType": "CrashLoopBackOff", "action": "Nuke", "enabled": true}
	if code, _ := do(t, http.MethodPut, ts.URL+"/api/v1/runbooks?tenantId=tenantR", adminTok, "", bad); code != http.StatusBadRequest {
		t.Fatalf("unknown action runbook should be 400, got %d", code)
	}
	// Valid runbook: CrashLoopBackOff → Scale to 4 (overrides the AI's RolloutRestart).
	rb := map[string]any{"incidentType": "CrashLoopBackOff", "action": "Scale", "params": map[string]string{"replicas": "4"}, "enabled": true}
	code, body := do(t, http.MethodPut, ts.URL+"/api/v1/runbooks?tenantId=tenantR", adminTok, "", rb)
	if code != http.StatusOK {
		t.Fatalf("valid runbook put want 200, got %d: %s", code, body)
	}
	var saved Runbook
	_ = json.Unmarshal(body, &saved)
	if saved.Version != 1 || saved.ID == "" {
		t.Fatalf("runbook not versioned: %+v", saved)
	}

	// Find the incident, then propose - the runbook must win over the AI rec.
	_, ib := do(t, http.MethodGet, ts.URL+"/api/v1/incidents?tenantId=tenantR&status=open", adminTok, "", nil)
	var il struct {
		Incidents []Incident `json:"incidents"`
	}
	_ = json.Unmarshal(ib, &il)
	if len(il.Incidents) != 1 {
		t.Fatalf("want 1 incident, got %d", len(il.Incidents))
	}
	_, pb := do(t, http.MethodPost, ts.URL+"/api/v1/incidents/"+il.Incidents[0].ID+"/propose?tenantId=tenantR", adminTok, "", nil)
	var p RemediationProposal
	_ = json.Unmarshal(pb, &p)
	if p.Action != ActionScale || p.Params["replicas"] != "4" || p.Target.Name != "api" {
		t.Fatalf("runbook should drive a Scale(4) on Deployment/api, got %+v", p)
	}
}
