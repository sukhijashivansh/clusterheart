package controlplane

import (
	"context"
	"errors"
	"testing"
)

var rsParams = map[string]string{"cpu": "250", "memory": "512"}

// A person applying Rightsizing needs no allowlist entry, even in production,
// and after approval the action is queued for the controller.
func TestSetResourcesHumanAppliedInProduction(t *testing.T) {
	svc, _ := setupRemediation(t, "production") // default policy: no namespaces allowlisted
	ctx := context.Background()
	p, err := svc.CreateClusterProposal(ctx, "t1", "cluA", ActionSetResources, deploymentTarget, rsParams, "rightsizing", "user:dev@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusProposed || p.IncidentID != "" || p.ClusterID != "cluA" {
		t.Fatalf("proposal = %+v (reasons %v)", p, p.Decision.Reasons)
	}
	p, err = svc.Approve(ctx, "t1", p.ID, "user:dev@example.com")
	if err != nil || p.Status != StatusApproved {
		t.Fatalf("approve: %v %+v", err, p)
	}
	pending, _ := svc.PendingForCluster(ctx, "t1", "cluA")
	if len(pending) != 1 || pending[0].Params["cpu"] != "250" {
		t.Fatalf("pending = %+v", pending)
	}
}

// AI, runbooks and operator automation can never apply SetResources.
func TestSetResourcesBlockedWhenNotHuman(t *testing.T) {
	svc, _ := setupRemediation(t, "staging")
	allowNamespace(t, svc, ModeApprovalRequired)
	p, err := svc.CreateClusterProposal(context.Background(), "t1", "cluA", ActionSetResources, deploymentTarget, rsParams, "", "operator:dashboard")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusBlocked {
		t.Fatalf("non-human SetResources should be blocked, got %s", p.Status)
	}
}

// Safety switches still apply to human-applied Rightsizing.
func TestSetResourcesRespectsPause(t *testing.T) {
	svc, _ := setupRemediation(t, "staging")
	ctx := context.Background()
	if _, err := svc.SetPauseScope(ctx, "t1", "cluster", "cluA", true, "test"); err != nil {
		t.Fatal(err)
	}
	p, err := svc.CreateClusterProposal(ctx, "t1", "cluA", ActionSetResources, deploymentTarget, rsParams, "", "user:dev@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusBlocked {
		t.Fatalf("paused cluster should block, got %s", p.Status)
	}
}

func TestSetResourcesValidation(t *testing.T) {
	svc, _ := setupRemediation(t, "staging")
	ctx := context.Background()
	bad := []struct {
		target TargetRef
		params map[string]string
	}{
		{TargetRef{Kind: "Deployment", Namespace: "kube-system", Name: "coredns"}, rsParams}, // managed
		{TargetRef{Kind: "Pod", Namespace: "web", Name: "api-1"}, rsParams},                  // wrong kind
		{deploymentTarget, nil}, // nothing to set
		{deploymentTarget, map[string]string{"cpu": "0"}},
		{deploymentTarget, map[string]string{"memory": "lots"}},
		{deploymentTarget, map[string]string{"cpu": "100", "limits": "1"}}, // unknown param
	}
	for i, c := range bad {
		if _, err := svc.CreateClusterProposal(ctx, "t1", "cluA", ActionSetResources, c.target, c.params, "", "user:x"); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
	// Another tenant's cluster is not found.
	if _, err := svc.CreateClusterProposal(ctx, "t2", "cluA", ActionSetResources, deploymentTarget, rsParams, "", "user:x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant: got %v", err)
	}
}
