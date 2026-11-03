package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"clusterheart/sre/internal/contract"
)

func TestLatencyCheckFlow(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	a := register(t, ts.URL, "acme", "prod")
	b := register(t, ts.URL, "other", "theirs")
	target := contract.ProbeTarget{Namespace: "web", Pod: "api-5d8f-x2", Scheme: "https", Path: "/healthz", Samples: 50}

	// Validation and tenant checks.
	for _, bad := range []contract.ProbeTarget{{Namespace: "web", Pod: "Bad Name"}, {Namespace: "web", Pod: "api", Path: "no-slash"}, {Namespace: "web", Pod: "api", Scheme: "ftp"}, {Namespace: "web", Pod: "api", Port: 70000}} {
		if code, body := do(t, http.MethodPost, ts.URL+"/api/v1/clusters/"+a.ClusterID+"/probes?tenantId=acme", adminTok, "", bad); code != http.StatusBadRequest {
			t.Errorf("bad target %+v accepted: %d %s", bad, code, body)
		}
	}
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/clusters/"+a.ClusterID+"/probes?tenantId=other", adminTok, "", target); code != http.StatusNotFound {
		t.Fatalf("other tenant could queue a check: %d", code)
	}

	code, body := do(t, http.MethodPost, ts.URL+"/api/v1/clusters/"+a.ClusterID+"/probes?tenantId=acme", adminTok, "", target)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)

	// Only this cluster's agent sees it, once, with samples capped.
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/agents/probes", b.AgentToken, b.ClusterID, nil)
	if strings.Contains(string(body), created.ID) {
		t.Fatal("another cluster's agent got the check")
	}
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/agents/probes", a.AgentToken, a.ClusterID, nil)
	var q contract.ProbeRequestsResponse
	_ = json.Unmarshal(body, &q)
	if len(q.Requests) != 1 || q.Requests[0].ID != created.ID || q.Requests[0].Target.Samples != 20 || q.Requests[0].Target.Path != "/healthz" {
		t.Fatalf("agent queue: %s", body)
	}
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/agents/probes", a.AgentToken, a.ClusterID, nil)
	if strings.Contains(string(body), created.ID) {
		t.Fatal("check handed out twice")
	}
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/probes/"+created.ID+"?tenantId=acme", adminTok, "", nil)
	if !strings.Contains(string(body), `"done":false`) || !strings.Contains(string(body), `"started":true`) {
		t.Fatalf("pending state: %s", body)
	}

	res := contract.ProbeResult{URL: "https://10.0.0.7:8443/healthz", Port: 8443, Count: 1, OKCount: 1,
		Samples: []contract.ProbeSample{{ConnectMs: 0.4, TLSms: 3.1, WaitMs: 12, TransferMs: 0.2, FirstByte: 15.5, TotalMs: 15.7, Status: 200, Bytes: 2}}}
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/agents/probes/"+created.ID, b.AgentToken, b.ClusterID, res); code != http.StatusNotFound {
		t.Fatalf("another cluster's agent answered: %d", code)
	}
	if code, body := do(t, http.MethodPost, ts.URL+"/api/v1/agents/probes/"+created.ID, a.AgentToken, a.ClusterID, res); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, body)
	}
	if code, _ := do(t, http.MethodGet, ts.URL+"/api/v1/probes/"+created.ID+"?tenantId=other", adminTok, "", nil); code != http.StatusNotFound {
		t.Fatalf("other tenant read the result: %d", code)
	}
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/probes/"+created.ID+"?tenantId=acme", adminTok, "", nil)
	var got struct {
		Done   bool
		Result contract.ProbeResult
	}
	_ = json.Unmarshal(body, &got)
	if !got.Done || got.Result.Port != 8443 || got.Result.Samples[0].TLSms != 3.1 {
		t.Fatalf("done state: %s", body)
	}

	// At most a few pending checks per cluster.
	for i := 0; i < maxPendingProbes; i++ {
		do(t, http.MethodPost, ts.URL+"/api/v1/clusters/"+a.ClusterID+"/probes?tenantId=acme", adminTok, "", target)
	}
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/clusters/"+a.ClusterID+"/probes?tenantId=acme", adminTok, "", target); code != http.StatusTooManyRequests {
		t.Fatalf("pending cap: %d", code)
	}

	// Servers have no pods to check.
	_, body = do(t, http.MethodPost, ts.URL+"/api/v1/clusters", adminTok, "", contract.RegisterRequest{TenantID: "acme", ClusterName: "web-01", Kind: contract.KindServer})
	var srv contract.RegisterResponse
	_ = json.Unmarshal(body, &srv)
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/clusters/"+srv.ClusterID+"/probes?tenantId=acme", adminTok, "", target); code != http.StatusBadRequest {
		t.Fatalf("server accepted a pod check: %d", code)
	}
}

func TestLatencyCheckExpires(t *testing.T) {
	srv := NewServer(NewMemStore(), NewMemIncidentStore(), NewMemRemediationStore(), NewMemPolicyStore(), NewMemAuditStore(),
		NewMemDiagnosisStore(), NewMemRunbookStore(), adminTok, nil)
	now := time.Now()
	srv.now = func() time.Time { return now }
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	a := register(t, ts.URL, "acme", "prod")
	_, body := do(t, http.MethodPost, ts.URL+"/api/v1/clusters/"+a.ClusterID+"/probes?tenantId=acme", adminTok, "", contract.ProbeTarget{Namespace: "web", Pod: "api"})
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)

	now = now.Add(probePickupTimeout + time.Second) // an old agent never polls
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/probes/"+created.ID+"?tenantId=acme", adminTok, "", nil)
	if !strings.Contains(string(body), `"done":true`) || !strings.Contains(string(body), "Update the agent") {
		t.Fatalf("not expired: %s", body)
	}
	now = now.Add(probeKeep + time.Second)
	if code, _ := do(t, http.MethodGet, ts.URL+"/api/v1/probes/"+created.ID+"?tenantId=acme", adminTok, "", nil); code != http.StatusNotFound {
		t.Fatalf("old result kept: %d", code)
	}
}
