package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"clusterheart/sre/internal/contract"
)

func TestServerKindRegistrationAndHostTelemetry(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	if code, _ := do(t, http.MethodPost, ts.URL+"/api/v1/clusters", adminTok, "",
		contract.RegisterRequest{TenantID: "acme", ClusterName: "x", Kind: "toaster"}); code != http.StatusBadRequest {
		t.Fatalf("bad kind accepted: %d", code)
	}
	k8s := register(t, ts.URL, "acme", "prod")
	code, body := do(t, http.MethodPost, ts.URL+"/api/v1/clusters", adminTok, "",
		contract.RegisterRequest{TenantID: "acme", ClusterName: "web-01", Kind: contract.KindServer})
	if code != http.StatusCreated {
		t.Fatalf("register server: %d %s", code, body)
	}
	var srv contract.RegisterResponse
	_ = json.Unmarshal(body, &srv)

	snap := contract.ClusterSnapshot{CollectedAt: time.Now().UTC(), Host: &contract.HostSnapshot{
		Hostname: "web-01", CPUCores: 4, CPUPercent: 12.5, MemTotalMi: 8000, MemUsedMi: 3000,
		Disks:     []contract.HostDisk{{Mount: "/", FS: "ext4", TotalMi: 50000, UsedMi: 46000}},
		ErrorLogs: []contract.HostLogLine{{Time: time.Now().UTC(), Unit: "app.service", Message: "login failed password=hunter22"}},
		Problems:  []contract.HostProblem{{Type: "DiskFull", Severity: "high", Message: "/ is 92% full"}},
	}}
	batch := contract.TelemetryBatch{APIVersion: contract.APIVersion, Snapshots: []contract.ClusterSnapshot{snap}}
	if code, body := do(t, http.MethodPost, ts.URL+"/api/v1/telemetry", srv.AgentToken, srv.ClusterID, batch); code != http.StatusAccepted {
		t.Fatalf("host telemetry: %d %s", code, body)
	}

	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/clusters?tenantId=acme", adminTok, "", nil)
	var out struct {
		Clusters []contract.ClusterStatus `json:"clusters"`
	}
	_ = json.Unmarshal(body, &out)
	kinds := map[string]string{}
	var host *contract.HostSnapshot
	for _, c := range out.Clusters {
		kinds[c.ClusterID] = c.Kind
		if c.ClusterID == srv.ClusterID && c.Latest != nil {
			host = c.Latest.Host
		}
	}
	if kinds[k8s.ClusterID] != contract.KindKubernetes || kinds[srv.ClusterID] != contract.KindServer {
		t.Fatalf("kinds = %v", kinds)
	}
	if host == nil || host.Hostname != "web-01" || len(host.Disks) != 1 {
		t.Fatalf("host snapshot not stored: %s", body)
	}
	if strings.Contains(host.ErrorLogs[0].Message, "hunter22") {
		t.Fatalf("host log not redacted on ingest: %q", host.ErrorLogs[0].Message)
	}
}
