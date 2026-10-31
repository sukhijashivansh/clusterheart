package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"clusterheart/sre/internal/contract"
)

func crashSnapWithLogs(at time.Time, lines ...string) contract.ClusterSnapshot {
	s := snapWithProblems(at, []contract.PodProblem{{
		Namespace: "web", Pod: "api-1", Owner: "Deployment/api", Container: "api",
		Type: contract.ProblemCrashLoopBackOff, RestartCount: 3, Message: "back-off",
	}}, nil)
	s.PodLogs = []contract.PodLog{{Namespace: "web", Pod: "api-1", Container: "api", Previous: true, Lines: lines}}
	return s
}

// Log tails ride along on incident evidence and reach the RCA prompt, redacted.
func TestIncidentEvidenceCarriesLogs(t *testing.T) {
	ctx := context.Background()
	inc := NewMemIncidentStore()
	eng := newEngine(inc, nil, nil)
	if err := eng.Reconcile(ctx, "t1", "cluA", crashSnapWithLogs(time.Now(), "starting", "panic: db password=hunter22 rejected")); err != nil {
		t.Fatal(err)
	}
	incs, _ := inc.ListIncidents(ctx, "t1", "open")
	if len(incs) != 1 || len(incs[0].Evidence) == 0 {
		t.Fatalf("incidents = %+v", incs)
	}
	ev := incs[0].Evidence[len(incs[0].Evidence)-1]
	if len(ev.Logs) != 2 || ev.Logs[0] != "starting" {
		t.Fatalf("evidence logs = %v", ev.Logs)
	}
	_, user := buildRCAPrompt(incs[0])
	if !strings.Contains(user, "recentLogs:") || !strings.Contains(user, "panic: db") {
		t.Fatalf("prompt missing logs:\n%s", user)
	}
	if strings.Contains(user, "hunter22") {
		t.Fatalf("prompt leaked a secret:\n%s", user)
	}
}

// The control plane accepts telemetry carrying podLogs and redacts it on ingest.
func TestTelemetryWithPodLogsAccepted(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	c := register(t, ts.URL, "acme", "prod")
	batch := contract.TelemetryBatch{APIVersion: contract.APIVersion, Snapshots: []contract.ClusterSnapshot{
		crashSnapWithLogs(time.Now().UTC(), "token=abcd1234efgh boom"),
	}}
	if code, body := do(t, http.MethodPost, ts.URL+"/api/v1/telemetry", c.AgentToken, c.ClusterID, batch); code != http.StatusAccepted {
		t.Fatalf("telemetry with logs: %d %s", code, body)
	}
	code, body := do(t, http.MethodGet, ts.URL+"/api/v1/clusters?tenantId=acme", adminTok, "", nil)
	if code != http.StatusOK {
		t.Fatalf("list clusters: %d", code)
	}
	var out struct {
		Clusters []contract.ClusterStatus `json:"clusters"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Clusters) != 1 || out.Clusters[0].Latest == nil || len(out.Clusters[0].Latest.PodLogs) != 1 {
		t.Fatalf("latest snapshot lost its logs: %s", body)
	}
	if l := out.Clusters[0].Latest.PodLogs[0].Lines[0]; strings.Contains(l, "abcd1234efgh") {
		t.Fatalf("log line not redacted on ingest: %q", l)
	}
}

// Secret findings are accepted, and their context is re-redacted on ingest.
func TestTelemetryWithSecretFindingsAccepted(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	c := register(t, ts.URL, "acme", "prod")
	snap := contract.ClusterSnapshot{CollectedAt: time.Now().UTC(), SecretFindings: []contract.SecretFinding{{
		Source: "log", Type: "aws-access-key", Namespace: "web", Owner: "Deployment/api", Container: "api",
		Context: "using key AKIAIOSFODNN7EXAMPLE", Count: 3, FirstSeen: time.Now().UTC(), LastSeen: time.Now().UTC(),
	}}}
	batch := contract.TelemetryBatch{APIVersion: contract.APIVersion, Snapshots: []contract.ClusterSnapshot{snap}}
	if code, body := do(t, http.MethodPost, ts.URL+"/api/v1/telemetry", c.AgentToken, c.ClusterID, batch); code != http.StatusAccepted {
		t.Fatalf("telemetry with findings: %d %s", code, body)
	}
	_, body := do(t, http.MethodGet, ts.URL+"/api/v1/clusters?tenantId=acme", adminTok, "", nil)
	var out struct {
		Clusters []contract.ClusterStatus `json:"clusters"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Clusters) != 1 || out.Clusters[0].Latest == nil || len(out.Clusters[0].Latest.SecretFindings) != 1 {
		t.Fatalf("findings not stored: %s", body)
	}
	if ctx := out.Clusters[0].Latest.SecretFindings[0].Context; strings.Contains(ctx, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("context not re-redacted: %q", ctx)
	}
}
