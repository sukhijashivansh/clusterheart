package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"clusterheart/sre/internal/contract"
)

func TestLogReadFlow(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	a := register(t, ts.URL, "acme", "prod")
	b := register(t, ts.URL, "other", "theirs")
	create := func(tenant string, target contract.LogTarget) (int, []byte) {
		return do(t, http.MethodPost, ts.URL+"/api/v1/clusters/"+a.ClusterID+"/logs?tenantId="+tenant, adminTok, "", target)
	}
	for _, bad := range []contract.LogTarget{{Namespace: "web"}, {Namespace: "web", Pod: "a", Job: "b"}, {Namespace: "web", Pod: "Bad Name"}, {Namespace: "web", Pod: "api", Container: "x y"}} {
		if code, body := create("acme", bad); code != http.StatusBadRequest {
			t.Errorf("bad target %+v accepted: %d %s", bad, code, body)
		}
	}
	if code, _ := create("other", contract.LogTarget{Namespace: "web", Pod: "api"}); code != http.StatusNotFound {
		t.Fatalf("other tenant could queue a log read: %d", code)
	}
	code, body := create("acme", contract.LogTarget{Namespace: "batch", Job: "nightly-29001", Tail: 9999})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)

	// Handed out on the probe poll, to this cluster's agent only, once.
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/agents/probes", b.AgentToken, b.ClusterID, nil)
	if strings.Contains(string(body), created.ID) {
		t.Fatal("another cluster's agent got the log read")
	}
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/agents/probes", a.AgentToken, a.ClusterID, nil)
	var q contract.ProbeRequestsResponse
	_ = json.Unmarshal(body, &q)
	if len(q.Logs) != 1 || q.Logs[0].ID != created.ID || q.Logs[0].Target.Job != "nightly-29001" || q.Logs[0].Target.Tail != maxLogTail {
		t.Fatalf("agent queue: %s", body)
	}
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/agents/probes", a.AgentToken, a.ClusterID, nil)
	if strings.Contains(string(body), created.ID) {
		t.Fatal("log read handed out twice")
	}

	res := contract.LogResult{Pod: "nightly-29001-x7k", Container: "main", Containers: []string{"main"}, Lines: []string{"starting", "password=hunter2secret"}}
	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/agents/logs/"+created.ID, b.AgentToken, b.ClusterID, res); code != http.StatusNotFound {
		t.Fatalf("another cluster's agent answered: %d", code)
	}
	if code, body := do(t, http.MethodPost, ts.URL+"/api/v1/agents/logs/"+created.ID, a.AgentToken, a.ClusterID, res); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, body)
	}
	if code, _ := do(t, http.MethodGet, ts.URL+"/api/v1/logs/"+created.ID+"?tenantId=other", adminTok, "", nil); code != http.StatusNotFound {
		t.Fatalf("other tenant read the log: %d", code)
	}
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/logs/"+created.ID+"?tenantId=acme", adminTok, "", nil)
	var got struct {
		Done   bool
		Result contract.LogResult
	}
	_ = json.Unmarshal(body, &got)
	if !got.Done || got.Result.Pod != "nightly-29001-x7k" || len(got.Result.Lines) != 2 || strings.Contains(string(body), "hunter2secret") {
		t.Fatalf("done state: %s", body)
	}
}
