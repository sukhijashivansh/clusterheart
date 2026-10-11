package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"clusterheart/sre/internal/contract"
)

// testCluster mirrors the cluster setupRemediation registers (for ReportResult).
var testCluster = Cluster{ID: "cluA", TenantID: "t1"}

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
	eng := newEngine(inc, nil, nil)
	_ = eng.Reconcile(ctx, "t1", "cluA", snapWithProblems(time.Now(),
		[]contract.PodProblem{{Namespace: "web", Pod: "api-1", Owner: "Deployment/api", Container: "api", Type: contract.ProblemCrashLoopBackOff, Message: "x"}}, nil))
	incs, _ := inc.ListIncidents(ctx, "t1", "open")
	if len(incs) != 1 {
		t.Fatalf("setup expected 1 incident, got %d", len(incs))
	}
	svc := newRemediationService(NewMemRemediationStore(), NewMemPolicyStore(), NewMemAuditStore(), inc, clusters, nil, nil)
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

func TestApprovalThenControllerExecutes(t *testing.T) {
	svc, inc := setupRemediation(t, "staging")
	allowNamespace(t, svc, ModeApprovalRequired)
	ctx := context.Background()

	p, err := svc.CreateProposal(ctx, "t1", inc, ActionRolloutRestart, deploymentTarget, nil, "crashloop", "operator:test")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusProposed || p.Execution != nil {
		t.Fatalf("want proposed/no-exec, got %s (%v)", p.Status, p.Decision.Reasons)
	}
	appr, err := svc.Approve(ctx, "t1", p.ID, "operator:boss")
	if err != nil {
		t.Fatal(err)
	}
	// Approval does NOT execute in the control plane — it hands off to the controller.
	if appr.Status != StatusApproved || appr.Execution != nil {
		t.Fatalf("approval must not execute in control plane: status=%s", appr.Status)
	}
	pend, _ := svc.PendingForCluster(ctx, "t1", "cluA")
	if len(pend) != 1 || pend[0].ID != p.ID {
		t.Fatalf("expected 1 pending action, got %d", len(pend))
	}
	// The in-cluster controller reports a (dry-run) success.
	tru := true
	res := contract.ActionResult{Success: true, DryRun: true, PlannedChange: "restart", Result: "ok",
		RollbackAvailable: true, VerificationPassed: &tru, StartedAt: time.Now(), FinishedAt: time.Now()}
	done, err := svc.ReportResult(ctx, testCluster, p.ID, res)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusExecuted || done.Execution == nil || !done.Execution.Success {
		t.Fatalf("want executed, got %s", done.Status)
	}
	// Re-reporting must be rejected (no replay/overwrite of a completed action).
	if _, err := svc.ReportResult(ctx, testCluster, p.ID, res); !errors.Is(err, errConflict) {
		t.Fatalf("re-report should conflict, got %v", err)
	}
	// A cluster may not report on a proposal that is not its own.
	if _, err := svc.ReportResult(ctx, Cluster{ID: "cluB", TenantID: "t1"}, p.ID, res); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-cluster report should be ErrNotFound, got %v", err)
	}
}

func TestCircuitBreakerOpensAfterRepeatedFailures(t *testing.T) {
	svc, inc := setupRemediation(t, "staging")
	ctx := context.Background()
	// Isolate the breaker from the per-workload cooldown and concurrency cap so
	// the test exercises only the failure-count guard.
	pol := DefaultPolicy("t1")
	pol.NamespaceAllowlist = []string{"web"}
	pol.PerWorkloadCooldown = 0
	pol.MaxConcurrent = 0
	pol.CircuitBreakerFailures = 3
	pol.CircuitBreakerWindow = 30 * time.Minute
	if err := svc.PutPolicy(ctx, pol, "test"); err != nil {
		t.Fatal(err)
	}

	// Three failed remediations for the same target.
	for i := 0; i < 3; i++ {
		p, err := svc.CreateProposal(ctx, "t1", inc, ActionRolloutRestart, deploymentTarget, nil, "", "x")
		if err != nil {
			t.Fatalf("iter %d create: %v", i, err)
		}
		if p.Status != StatusProposed {
			t.Fatalf("iter %d: want proposed, got %s (%v)", i, p.Status, p.Decision.Reasons)
		}
		if _, err := svc.Approve(ctx, "t1", p.ID, "boss"); err != nil {
			t.Fatalf("iter %d approve: %v", i, err)
		}
		now := time.Now()
		if _, err := svc.ReportResult(ctx, testCluster, p.ID, contract.ActionResult{
			Success: false, StartedAt: now, FinishedAt: now, Error: "boom",
		}); err != nil {
			t.Fatalf("iter %d report: %v", i, err)
		}
	}

	// The fourth proposal for the same target must trip the breaker.
	p, err := svc.CreateProposal(ctx, "t1", inc, ActionRolloutRestart, deploymentTarget, nil, "", "x")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusBlocked || !containsSubstr(p.Decision.Reasons, "circuit breaker") {
		t.Fatalf("want circuit-breaker block, got %s %v", p.Status, p.Decision.Reasons)
	}

	// A different target is unaffected by this target's breaker.
	other := TargetRef{Kind: "Deployment", Namespace: "web", Name: "other"}
	p2, err := svc.CreateProposal(ctx, "t1", inc, ActionRolloutRestart, other, nil, "", "x")
	if err != nil {
		t.Fatal(err)
	}
	if p2.Status != StatusProposed {
		t.Fatalf("other target should not be blocked, got %s %v", p2.Status, p2.Decision.Reasons)
	}
}

func TestControllerFailureReportMarksFailed(t *testing.T) {
	svc, inc := setupRemediation(t, "staging")
	allowNamespace(t, svc, ModeApprovalRequired)
	ctx := context.Background()
	p, _ := svc.CreateProposal(ctx, "t1", inc, ActionRolloutRestart, deploymentTarget, nil, "", "x")
	_, _ = svc.Approve(ctx, "t1", p.ID, "boss")
	fal := false
	done, err := svc.ReportResult(ctx, testCluster, p.ID, contract.ActionResult{
		Success: false, VerificationPassed: &fal, Error: "rollout did not become healthy",
		StartedAt: time.Now(), FinishedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusFailed {
		t.Fatalf("want failed, got %s", done.Status)
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
	ctx := context.Background()
	p, _ := svc.CreateProposal(ctx, "t1", inc, ActionRolloutRestart, deploymentTarget, nil, "", "x")
	if _, err := svc.Approve(ctx, "t1", p.ID, "boss"); err != nil {
		t.Fatal(err)
	}
	// Controller executes and reports back, which records the execution time.
	now := time.Now()
	if _, err := svc.ReportResult(ctx, testCluster, p.ID, contract.ActionResult{Success: true, StartedAt: now, FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Immediately proposing the same target again must hit the cooldown.
	p2, _ := svc.CreateProposal(ctx, "t1", inc, ActionRolloutRestart, deploymentTarget, nil, "", "x")
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
