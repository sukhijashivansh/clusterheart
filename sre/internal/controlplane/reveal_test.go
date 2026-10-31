package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"clusterheart/sre/internal/contract"
)

func TestRevealRelayEndToEnd(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	a := register(t, ts.URL, "acme", "prod")
	other := register(t, ts.URL, "acme", "staging")
	ref := "log|web|Deployment/api|api||password-or-token|secret"

	// Another tenant can't request a reveal on this cluster.
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/clusters/"+a.ClusterID+"/reveals?tenantId=evil", adminTok, "",
		map[string]string{"ref": ref, "requestedBy": "user:x"}); code != http.StatusNotFound {
		t.Fatalf("cross-tenant create: %d", code)
	}
	code, body := do(t, http.MethodPost, ts.URL+"/api/v1/clusters/"+a.ClusterID+"/reveals?tenantId=acme", adminTok, "",
		map[string]string{"ref": ref, "requestedBy": "user:dev@example.com", "description": "secret in api"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)

	// Not answered yet.
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/reveals/"+created.ID+"?tenantId=acme", adminTok, "", nil)
	if !strings.Contains(string(body), `"done":false`) {
		t.Fatalf("pending get: %s", body)
	}
	// Only the owning cluster's agent sees the request.
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/agents/reveals", other.AgentToken, other.ClusterID, nil)
	if strings.Contains(string(body), created.ID) {
		t.Fatalf("other cluster saw the request: %s", body)
	}
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/agents/reveals/"+created.ID, other.AgentToken, other.ClusterID,
		contract.RevealResult{Value: "spoof"}); code != http.StatusNotFound {
		t.Fatalf("other cluster answered: %d", code)
	}
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/agents/reveals", a.AgentToken, a.ClusterID, nil)
	var pending contract.RevealRequestsResponse
	_ = json.Unmarshal(body, &pending)
	if len(pending.Requests) != 1 || pending.Requests[0].Ref != ref {
		t.Fatalf("agent queue: %s", body)
	}
	if code, body := do(t, http.MethodPost, ts.URL+"/api/v1/agents/reveals/"+created.ID, a.AgentToken, a.ClusterID,
		contract.RevealResult{Value: "hunter22"}); code != http.StatusOK {
		t.Fatalf("answer: %d %s", code, body)
	}

	// Another tenant can't read it; the owner reads it exactly once.
	if code, _ := do(t, http.MethodGet, ts.URL+"/api/v1/reveals/"+created.ID+"?tenantId=evil", adminTok, "", nil); code != http.StatusNotFound {
		t.Fatalf("cross-tenant read: %d", code)
	}
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/reveals/"+created.ID+"?tenantId=acme", adminTok, "", nil)
	if !strings.Contains(string(body), `"value":"hunter22"`) {
		t.Fatalf("read: %s", body)
	}
	if code, _ := do(t, http.MethodGet, ts.URL+"/api/v1/reveals/"+created.ID+"?tenantId=acme", adminTok, "", nil); code != http.StatusNotFound {
		t.Fatalf("value readable twice: %d", code)
	}

	// The reveal is audited, without the value.
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/audit?tenantId=acme", adminTok, "", nil)
	if !strings.Contains(string(body), "secret.revealed") || !strings.Contains(string(body), "user:dev@example.com") {
		t.Fatalf("reveal not audited: %s", body)
	}
	if strings.Contains(string(body), "hunter22") {
		t.Fatalf("audit leaked the value: %s", body)
	}
}
