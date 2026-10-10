package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"clusterheart/sre/internal/contract"
)

// setupRemediation wires a service with one registered cluster (given env) and a
// single open CrashLoopBackOff incident, and returns the service + incident id.
func setupRemediation(t *testing.T, env string) (*remediationService, string) {
	t.Helper()
	ctx := context.Background()
	clusters := NewMemStore()
	if err := clusters.CreateCluster(ctx, Cluster{ID: "cluA", TenantID: "t1", Name: "c", Environment: env}); err != nil {
		t.Fatal(err)
	}
	inc := NewMemIncidentStore()
	eng := newEngine(inc, nil)
	_ = eng.Reconcile(ctx, "t1", "cluA", snapWithProblems(time.Now(),
		[]contract.PodProblem{{Namespace: "web", Pod: "api-1", Owner: "Deployment/api", Container: "api", Type: contract.ProblemCrashLoopBackOff, Message: "x"}}, nil))
	incs, _ := inc.ListIncidents(ctx, "t1", "open")
	if len(incs) != 1 {
		t.Fatalf("setup expected 1 incident, got %d", len(incs))
	}
	svc := newRemediationService(NewMemRemediationStore(), NewMemPolicyStore(), NewMemAuditStore(), inc, clusters, nil)
	return svc, incs[0].ID
}

var deploymentTarget = TargetRef{Kind: "Deployment", Namespace: "web", Name: "api"}

func allowNamespace(t *testing.T, svc *remediationService, mode Mode) {
	t.Helper()
	pol := DefaultPolicy("t1")
	pol.Mode = mode
	pol.NamespaceAllowlist = []string{"web"}
	if err := svc.PutPolicy(context.Background(), pol, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestProposalBlockedByDefault(t *testing.T) {
	svc, inc := setupRemediation(t, "staging")
	p, err := svc.CreateProposal(context.Background(), "t1", inc, ActionRolloutRestart, deploymentTarget, nil, "crashloop", "operator:test")
	if err != nil {
		t.Fatal(err)
	}
	// Default policy has an empty namespace allowlist => nothing is actionable.
	if p.Status != StatusBlocked {
		t.Fatalf("want blocked by default, got %s (reasons=%v)", p.Status, p.Decision.Reasons)
	}
	if !containsSubstr(p.Decision.Reasons, "allowlist") {
		t.Fatalf("expected allowlist denial, got %v", p.Decision.Reasons)
	}
}

func TestApprovalFlowDryRunExecution(t *testing.T) {
	svc, inc := setupRemediation(t, "staging")
	allowNamespace(t, svc, ModeApprovalRequired)

	p, err := svc.CreateProposal(context.Background(), "t1", inc, ActionRolloutRestart, deploymentTarget, nil, "crashloop", "operator:test")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusProposed {
		t.Fatalf("want proposed, got %s (%v)", p.Status, p.Decision.Reasons)
	}
	if p.Execution != nil {
		t.Fatal("must not execute before approval")
	}
	done, err := svc.Approve(context.Background(), "t1", p.ID, "operator:boss")
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusExecuted || done.Execution == nil {
		t.Fatalf("want executed, got %s", done.Status)
	}
	ex := done.Execution
	if !ex.DryRun || !ex.Success || !ex.RollbackAvailable || ex.PlannedChange == "" {
		t.Fatalf("unexpected execution: %+v", ex)
	}
	if !strings.Contains(ex.Result, "no changes applied") {
		t.Fatalf("dry-run should not apply changes: %q", ex.Result)
	}
}

func TestUnknownAndInvalidActionsRejected(t *testing.T) {
	svc, inc := setupRemediation(t, "staging")
	allowNamespace(t, svc, ModeApprovalRequired)
	// unknown action
	if _, err := svc.CreateProposal(context.Background(), "t1", inc, ActionType("DeleteCluster"), deploymentTarget, nil, "", "x"); err == nil {
		t.Fatal("unknown action must be rejected")
	}
	// unsupported target kind
	if _, err := svc.CreateProposal(context.Background(), "t1", inc, ActionRolloutRestart, TargetRef{Kind: "Pod", Namespace: "web", Name: "api-1"}, nil, "", "x"); err == nil {
		t.Fatal("RolloutRestart on a Pod must be rejected")
	}
	// stray params
	if _, err := svc.CreateProposal(context.Background(), "t1", inc, ActionRolloutRestart, deploymentTarget, map[string]string{"force": "true"}, "", "x"); err == nil {
		t.Fatal("unexpected params must be rejected")
	}
}

func TestEmergencyPauseBlocksEverything(t *testing.T) {
	svc, inc := setupRemediation(t, "staging")
	allowNamespace(t, svc, ModeApprovalRequired)
	if _, err := svc.SetPaused(context.Background(), "t1", true, "test"); err != nil {
		t.Fatal(err)
	}
	p, err := svc.CreateProposal(context.Background(), "t1", inc, ActionRolloutRestart, deploymentTarget, nil, "", "x")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusBlocked || !containsSubstr(p.Decision.Reasons, "pause") {
		t.Fatalf("pause must block, got %s %v", p.Status, p.Decision.Reasons)
	}
}

func TestCooldownAfterExecution(t *testing.T) {
	svc, inc := setupRemediation(t, "staging")
	allowNamespace(t, svc, ModeApprovalRequired)
	p, _ := svc.CreateProposal(context.Background(), "t1", inc, ActionRolloutRestart, deploymentTarget, nil, "", "x")
	if _, err := svc.Approve(context.Background(), "t1", p.ID, "boss"); err != nil {
		t.Fatal(err)
	}
	// Immediately proposing the same target again must hit the cooldown.
	p2, _ := svc.CreateProposal(context.Background(), "t1", inc, ActionRolloutRestart, deploymentTarget, nil, "", "x")
	if p2.Status != StatusBlocked || !containsSubstr(p2.Decision.Reasons, "cooldown") {
		t.Fatalf("want cooldown block, got %s %v", p2.Status, p2.Decision.Reasons)
	}
}

func TestProductionDowngradesAutoToApproval(t *testing.T) {
	svc, inc := setupRemediation(t, "production")
	allowNamespace(t, svc, ModeAuto) // ask for auto...
	p, err := svc.CreateProposal(context.Background(), "t1", inc, ActionRolloutRestart, deploymentTarget, nil, "", "x")
	if err != nil {
		t.Fatal(err)
	}
	// ...but a protected env downgrades auto to approval-required, so it must NOT
	// auto-execute.
	if p.Status != StatusProposed || p.Decision.Mode != ModeApprovalRequired {
		t.Fatalf("production should force approval, got status=%s mode=%s", p.Status, p.Decision.Mode)
	}
	if p.Execution != nil {
		t.Fatal("must not auto-execute in a protected environment")
	}
}

func TestRejectFlowAndTenantIsolationAndAudit(t *testing.T) {
	svc, inc := setupRemediation(t, "staging")
	allowNamespace(t, svc, ModeApprovalRequired)
	p, _ := svc.CreateProposal(context.Background(), "t1", inc, ActionRolloutRestart, deploymentTarget, nil, "", "x")
	rj, err := svc.Reject(context.Background(), "t1", p.ID, "boss", "not now")
	if err != nil || rj.Status != StatusRejected {
		t.Fatalf("reject failed: %v status=%v", err, rj.Status)
	}
	// Tenant isolation: another tenant cannot read this proposal.
	if _, err := svc.GetProposal(context.Background(), "t2", p.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant proposal read must be ErrNotFound, got %v", err)
	}
	// Audit trail recorded the decisions.
	evs, _ := svc.ListAudit(context.Background(), "t1")
	if len(evs) < 2 {
		t.Fatalf("expected audit events, got %d", len(evs))
	}
	seen := map[string]bool{}
	for _, e := range evs {
		seen[e.Event] = true
	}
	if !seen["proposal.created"] || !seen["proposal.rejected"] {
		t.Fatalf("missing audit events: %v", seen)
	}
}

func containsSubstr(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
