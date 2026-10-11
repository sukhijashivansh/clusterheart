package controlplane

import (
	"context"
	"testing"
	"time"

	"clusterheart/sre/internal/contract"
)

func snapWithProblems(at time.Time, pods []contract.PodProblem, wls []contract.WorkloadProblem) contract.ClusterSnapshot {
	return contract.ClusterSnapshot{CollectedAt: at, PodProblems: pods, WorkloadProblems: wls}
}

func openIncidents(t *testing.T, store IncidentStore, tenant string) []Incident {
	t.Helper()
	incs, err := store.ListIncidents(context.Background(), tenant, "open")
	if err != nil {
		t.Fatal(err)
	}
	return incs
}

func TestDetectorsCreateIncidents(t *testing.T) {
	store := NewMemIncidentStore()
	eng := newEngine(store, nil, nil)
	now := time.Now()

	snap := snapWithProblems(now,
		[]contract.PodProblem{
			{Namespace: "web", Pod: "api-abc", Owner: "Deployment/api", Container: "api", Type: contract.ProblemCrashLoopBackOff, RestartCount: 7, Message: "back-off restarting"},
			{Namespace: "web", Pod: "api-xyz", Owner: "Deployment/api", Container: "api", Type: contract.ProblemOOMKilled, RestartCount: 3, Message: "OOMKilled"},
			{Namespace: "batch", Pod: "job-1", Type: contract.ProblemImagePullBackOff, Container: "run", Message: "pull failed"},
		},
		[]contract.WorkloadProblem{
			{Namespace: "web", Kind: "Deployment", Name: "frontend", Type: contract.ProblemRolloutStuck, Message: "deadline exceeded"},
		},
	)
	if err := eng.Reconcile(context.Background(), "t1", "cluA", snap); err != nil {
		t.Fatal(err)
	}
	incs := openIncidents(t, store, "t1")
	if len(incs) != 4 {
		t.Fatalf("want 4 incidents, got %d", len(incs))
	}
	// severities
	sev := map[contract.ProblemType]string{}
	for _, in := range incs {
		sev[in.Type] = in.Severity
		if in.Status != "open" || in.Occurrences != 1 || len(in.Evidence) != 1 {
			t.Fatalf("unexpected incident state: %+v", in)
		}
	}
	if sev[contract.ProblemCrashLoopBackOff] != "high" || sev[contract.ProblemImagePullBackOff] != "medium" {
		t.Fatalf("unexpected severities: %v", sev)
	}
}

func TestDedupAndTimeline(t *testing.T) {
	store := NewMemIncidentStore()
	eng := newEngine(store, nil, nil)
	base := time.Now()
	prob := []contract.PodProblem{{Namespace: "web", Pod: "api-1", Owner: "Deployment/api", Container: "api", Type: contract.ProblemCrashLoopBackOff, Message: "x"}}

	// Same problem observed across three cycles -> ONE incident, occurrences=3.
	for i := 0; i < 3; i++ {
		if err := eng.Reconcile(context.Background(), "t1", "cluA", snapWithProblems(base.Add(time.Duration(i)*time.Minute), prob, nil)); err != nil {
			t.Fatal(err)
		}
	}
	incs := openIncidents(t, store, "t1")
	if len(incs) != 1 {
		t.Fatalf("dedup failed: want 1 incident, got %d", len(incs))
	}
	if incs[0].Occurrences != 3 {
		t.Fatalf("want occurrences=3, got %d", incs[0].Occurrences)
	}
	if !incs[0].LastSeen.After(incs[0].FirstSeen) {
		t.Fatalf("timeline not advanced: first=%v last=%v", incs[0].FirstSeen, incs[0].LastSeen)
	}
}

func TestResolveWhenProblemClears(t *testing.T) {
	store := NewMemIncidentStore()
	eng := newEngine(store, nil, nil)
	now := time.Now()
	prob := []contract.PodProblem{{Namespace: "web", Pod: "api-1", Owner: "Deployment/api", Container: "api", Type: contract.ProblemCrashLoopBackOff, Message: "x"}}

	_ = eng.Reconcile(context.Background(), "t1", "cluA", snapWithProblems(now, prob, nil))
	if len(openIncidents(t, store, "t1")) != 1 {
		t.Fatal("expected 1 open incident")
	}
	// Next snapshot: problem gone -> incident resolves.
	_ = eng.Reconcile(context.Background(), "t1", "cluA", snapWithProblems(now.Add(time.Minute), nil, nil))
	if n := len(openIncidents(t, store, "t1")); n != 0 {
		t.Fatalf("expected 0 open incidents after clear, got %d", n)
	}
	resolved, _ := store.ListIncidents(context.Background(), "t1", "resolved")
	if len(resolved) != 1 || resolved[0].ResolvedAt == nil {
		t.Fatalf("expected 1 resolved incident with timestamp, got %+v", resolved)
	}
}

func TestIncidentTenantIsolation(t *testing.T) {
	store := NewMemIncidentStore()
	eng := newEngine(store, nil, nil)
	now := time.Now()
	prob := []contract.PodProblem{{Namespace: "web", Pod: "api-1", Owner: "Deployment/api", Container: "api", Type: contract.ProblemOOMKilled, Message: "x"}}
	_ = eng.Reconcile(context.Background(), "tenantA", "cluA", snapWithProblems(now, prob, nil))

	// tenantB must not see tenantA's incidents.
	if incs, _ := store.ListIncidents(context.Background(), "tenantB", ""); len(incs) != 0 {
		t.Fatalf("tenant leak in list: %d", len(incs))
	}
	a := openIncidents(t, store, "tenantA")
	if len(a) != 1 {
		t.Fatal("tenantA should have 1 incident")
	}
	// GetIncident with the wrong tenant must 404 (not leak).
	if _, err := store.GetIncident(context.Background(), "tenantB", a[0].ID); err != ErrNotFound {
		t.Fatalf("GetIncident cross-tenant should be ErrNotFound, got %v", err)
	}
}
