package controlplane

import (
	"context"
	"os"
	"testing"
	"time"
)

// These exercise the Mongo remediation stores against a REAL MongoDB. They run
// only when MONGO_TEST_URI is set (e.g. a throwaway docker container), so the
// default `go test ./...` stays hermetic. Example:
//
//	MONGO_TEST_URI=mongodb://127.0.0.1:27019 go test ./internal/controlplane -run Mongo
func mongoStores(t *testing.T) (context.Context, RemediationStore, PolicyStore, AuditStore) {
	t.Helper()
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("MONGO_TEST_URI not set; skipping Mongo integration test")
	}
	ctx := context.Background()
	// A unique db per run so repeated runs don't collide.
	dbName := "chtest_" + time.Now().Format("150405_000000")
	db, err := ConnectMongo(ctx, uri, dbName)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Drop(context.Background()) })
	rem, err := NewMongoRemediationStore(ctx, db)
	if err != nil {
		t.Fatalf("rem store: %v", err)
	}
	audit, err := NewMongoAuditStore(ctx, db)
	if err != nil {
		t.Fatalf("audit store: %v", err)
	}
	return ctx, rem, NewMongoPolicyStore(ctx, db), audit
}

func TestMongoProposalRoundTripAndQueries(t *testing.T) {
	ctx, rem, _, _ := mongoStores(t)
	tgt := TargetRef{Kind: "Deployment", Namespace: "web", Name: "api"}
	p := &RemediationProposal{
		ID: "rem_1", IncidentID: "inc_1", TenantID: "t1", ClusterID: "cluA",
		Action: ActionRolloutRestart, Target: tgt, Rationale: "crashloop",
		RiskClass: RiskMedium, Reversible: true, Status: StatusApproved,
		Decision:  &PolicyDecision{Allowed: true, Mode: ModeApprovalRequired, EvaluatedAt: time.Now()},
		CreatedAt: time.Now(),
	}
	if err := rem.Create(ctx, p); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Tenant scoping: wrong tenant must not read it.
	if _, err := rem.Get(ctx, "t2", "rem_1"); err != ErrNotFound {
		t.Fatalf("cross-tenant read should be ErrNotFound, got %v", err)
	}
	got, err := rem.Get(ctx, "t1", "rem_1")
	if err != nil || got.Decision == nil || got.Decision.Mode != ModeApprovalRequired {
		t.Fatalf("get round-trip lost data: %+v (err %v)", got, err)
	}

	// Approved + no execution => counts as active/in-flight.
	if n, _ := rem.ActiveCount(ctx, "cluA"); n != 1 {
		t.Fatalf("want ActiveCount 1, got %d", n)
	}
	// No execution yet => no last-execution time for the target.
	if last, _ := rem.LastExecutionForTarget(ctx, "cluA", tgt); last != nil {
		t.Fatalf("want nil last-execution, got %v", last)
	}

	// Record an execution; it must drop out of ActiveCount and surface in
	// LastExecutionForTarget (exercises the execution.finishedAt sort).
	fin := time.Now().Truncate(time.Millisecond)
	got.Status = StatusExecuted
	got.Execution = &ActionExecution{StartedAt: fin, FinishedAt: fin, Success: true, DryRun: true}
	if err := rem.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	if n, _ := rem.ActiveCount(ctx, "cluA"); n != 0 {
		t.Fatalf("executed proposal must not be active, got %d", n)
	}
	last, _ := rem.LastExecutionForTarget(ctx, "cluA", tgt)
	if last == nil || !last.Equal(fin) {
		t.Fatalf("want last-execution %v, got %v", fin, last)
	}

	// List by status, tenant-scoped.
	ps, err := rem.List(ctx, "t1", string(StatusExecuted))
	if err != nil || len(ps) != 1 {
		t.Fatalf("want 1 executed proposal, got %d (err %v)", len(ps), err)
	}
}

func TestMongoPolicyDefaultAndPersist(t *testing.T) {
	ctx, _, pol, _ := mongoStores(t)
	// Unset tenant returns the SAFE default (not an error).
	def, err := pol.Get(ctx, "t1")
	if err != nil || def.Mode != ModeApprovalRequired || !def.AllowedActions[ActionRolloutRestart] {
		t.Fatalf("default policy wrong: %+v (err %v)", def, err)
	}
	// map[ActionType]bool must survive a bson round-trip.
	def.NamespaceAllowlist = []string{"web"}
	def.Paused = true
	if err := pol.Put(ctx, def); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := pol.Get(ctx, "t1")
	if err != nil || !got.Paused || len(got.NamespaceAllowlist) != 1 || !got.AllowedActions[ActionRolloutRestart] {
		t.Fatalf("policy round-trip lost data: %+v (err %v)", got, err)
	}
}

func TestMongoAuditAppendAndScope(t *testing.T) {
	ctx, _, _, audit := mongoStores(t)
	_ = audit.Append(ctx, AuditEvent{ID: "a1", TenantID: "t1", At: time.Now(), Actor: "x", Event: "proposal.created"})
	_ = audit.Append(ctx, AuditEvent{ID: "a2", TenantID: "t2", At: time.Now(), Actor: "y", Event: "proposal.created"})
	evs, err := audit.List(ctx, "t1")
	if err != nil || len(evs) != 1 || evs[0].ID != "a1" {
		t.Fatalf("audit scoping wrong: %+v (err %v)", evs, err)
	}
}
