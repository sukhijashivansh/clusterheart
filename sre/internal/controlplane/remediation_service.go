package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"clusterheart/sre/internal/contract"
)

// errConflict signals a lifecycle violation (e.g. approving a non-proposed item).
var errConflict = errors.New("conflict")

// remediationService orchestrates the proposal → policy → approval → execute →
// audit flow. In slice A every execution is a DRY RUN: no cluster is contacted.
// The real in-cluster executor (with the narrow remediate ServiceAccount) is
// slice B. This service NEVER calls an LLM; policy is purely deterministic.
type remediationService struct {
	rem       RemediationStore
	pol       PolicyStore
	audit     AuditStore
	incidents IncidentStore
	clusters  Store
	notifier  Notifier
	engine    policyEngine
	log       *slog.Logger
	now       func() time.Time
}

func newRemediationService(rem RemediationStore, pol PolicyStore, audit AuditStore, incidents IncidentStore, clusters Store, notifier Notifier, log *slog.Logger) *remediationService {
	if log == nil {
		log = slog.Default()
	}
	if notifier == nil {
		notifier = noopNotifier{}
	}
	return &remediationService{
		rem: rem, pol: pol, audit: audit, incidents: incidents, clusters: clusters,
		notifier: notifier, engine: policyEngine{}, log: log, now: time.Now,
	}
}

func (s *remediationService) appendAudit(ctx context.Context, tenantID, actor, event, proposalID, incidentID, details string) {
	_ = s.audit.Append(ctx, AuditEvent{
		ID: newID("aud"), TenantID: tenantID, At: s.now(), Actor: actor,
		Event: event, ProposalID: proposalID, IncidentID: incidentID, Details: details,
	})
}

// evaluate gathers live state and runs the deterministic policy engine.
func (s *remediationService) evaluate(ctx context.Context, tenantID, clusterID string, action ActionType, target TargetRef, params map[string]string) PolicyDecision {
	pol, _ := s.pol.Get(ctx, tenantID)
	env := ""
	if c, err := s.clusters.GetCluster(ctx, clusterID); err == nil {
		env = c.Environment
	}
	active, _ := s.rem.ActiveCount(ctx, clusterID)
	last, _ := s.rem.LastExecutionForTarget(ctx, clusterID, target)
	now := s.now()
	failures := 0
	if pol.CircuitBreakerFailures > 0 && pol.CircuitBreakerWindow > 0 {
		failures, _ = s.rem.RecentFailuresForTarget(ctx, clusterID, target, now.Add(-pol.CircuitBreakerWindow))
	}
	replicas, hasReplicas := parseReplicas(params)
	return s.engine.Evaluate(pol, action, target, policyInput{
		environment: env, activeCount: active, lastForTarget: last, recentFailures: failures,
		requestedReplicas: replicas, hasReplicas: hasReplicas,
	}, now)
}

// CreateProposal validates and records a proposal, evaluates policy, and either
// blocks it, parks it for approval, or (in dry-run/auto mode) executes it as a
// dry run.
func (s *remediationService) CreateProposal(ctx context.Context, tenantID, incidentID string, action ActionType, target TargetRef, params map[string]string, rationale, createdBy string) (*RemediationProposal, error) {
	spec, err := lookupAction(action)
	if err != nil {
		return nil, err
	}
	if err := spec.validate(target, params); err != nil {
		return nil, fmt.Errorf("invalid action: %w", err)
	}
	inc, err := s.incidents.GetIncident(ctx, tenantID, incidentID)
	if err != nil {
		return nil, fmt.Errorf("incident: %w", err)
	}

	dec := s.evaluate(ctx, tenantID, inc.ClusterID, action, target, params)
	p := &RemediationProposal{
		ID: newID("rem"), IncidentID: incidentID, TenantID: tenantID, ClusterID: inc.ClusterID,
		Action: action, Target: target, Params: params, Rationale: rationale,
		RiskClass: spec.risk, Reversible: spec.reversible, Decision: &dec,
		CreatedAt: s.now(), CreatedBy: createdBy,
	}

	if !dec.Allowed {
		p.Status = StatusBlocked
		_ = s.rem.Create(ctx, p)
		s.appendAudit(ctx, tenantID, createdBy, "proposal.blocked", p.ID, incidentID, strings.Join(dec.Reasons, "; "))
		return p, nil
	}

	switch dec.Mode {
	case ModeDryRun, ModeAuto:
		// Ready for the in-cluster controller to pick up. dry-run vs real apply
		// is resolved from the mode when the action is handed to the controller.
		p.Status = StatusApproved
	default: // recommend / approval-required → wait for a human
		p.Status = StatusProposed
	}
	_ = s.rem.Create(ctx, p)
	s.appendAudit(ctx, tenantID, createdBy, "proposal.created", p.ID, incidentID, string(dec.Mode))
	return p, nil
}

// Approve re-evaluates policy at execution time, then executes (dry run).
func (s *remediationService) Approve(ctx context.Context, tenantID, id, approver string) (*RemediationProposal, error) {
	p, err := s.rem.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if p.Status != StatusProposed {
		return nil, fmt.Errorf("%w: proposal is %s, not awaiting approval", errConflict, p.Status)
	}
	// Preconditions can change between proposal and approval (pause, cooldown,
	// concurrency) — re-evaluate deterministically.
	dec := s.evaluate(ctx, tenantID, p.ClusterID, p.Action, p.Target, p.Params)
	now := s.now()
	p.DecidedBy = approver
	p.DecidedAt = &now
	p.Decision = &dec
	if !dec.Allowed {
		p.Status = StatusBlocked
		_ = s.rem.Update(ctx, p)
		s.appendAudit(ctx, tenantID, approver, "proposal.blocked", p.ID, p.IncidentID, strings.Join(dec.Reasons, "; "))
		return p, nil
	}
	// Approved and ready for the in-cluster controller to execute. The control
	// plane never applies the change itself.
	p.Status = StatusApproved
	_ = s.rem.Update(ctx, p)
	s.appendAudit(ctx, tenantID, approver, "proposal.approved", p.ID, p.IncidentID, "")
	return p, nil
}

// PendingForCluster returns approved, not-yet-executed proposals for a cluster.
// The in-cluster controller pulls these, executes them, and reports back.
func (s *remediationService) PendingForCluster(ctx context.Context, tenantID, clusterID string) ([]RemediationProposal, error) {
	all, err := s.rem.List(ctx, tenantID, string(StatusApproved))
	if err != nil {
		return nil, err
	}
	var out []RemediationProposal
	for _, p := range all {
		if p.ClusterID == clusterID && p.Execution == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// ReportResult records the outcome the in-cluster controller reports for an
// approved action. The result may only be reported by the cluster that owns the
// proposal, and only while it is still approved (prevents replay/overwrite).
func (s *remediationService) ReportResult(ctx context.Context, cluster Cluster, proposalID string, res contract.ActionResult) (*RemediationProposal, error) {
	p, err := s.rem.Get(ctx, cluster.TenantID, proposalID)
	if err != nil {
		return nil, err
	}
	if p.ClusterID != cluster.ID {
		return nil, ErrNotFound // a cluster may only report on its own proposals
	}
	if p.Status != StatusApproved {
		return nil, fmt.Errorf("%w: proposal is %s, not awaiting execution", errConflict, p.Status)
	}
	p.Execution = &ActionExecution{
		StartedAt: res.StartedAt, FinishedAt: res.FinishedAt, DryRun: res.DryRun,
		Success: res.Success, PlannedChange: res.PlannedChange, BeforeState: res.BeforeState,
		Result: res.Result, RollbackAvailable: res.RollbackAvailable,
		VerificationPassed: res.VerificationPassed, Error: res.Error,
	}
	if res.Success {
		p.Status = StatusExecuted
	} else {
		p.Status = StatusFailed
	}
	_ = s.rem.Update(ctx, p)
	ev := "remediation.executed"
	if !res.Success {
		ev = "remediation.failed"
	}
	detail := "dry-run"
	if !res.DryRun {
		detail = "applied"
	}
	s.appendAudit(ctx, cluster.TenantID, "controller:"+cluster.ID, ev, p.ID, p.IncidentID, detail)
	s.notifier.Notify(Notification{
		Kind:  "remediation." + map[bool]string{true: "executed", false: "failed"}[res.Success],
		Title: fmt.Sprintf("%s on %s", p.Action, p.Target), Detail: detailLine(res),
		TenantID: cluster.TenantID, ClusterID: cluster.ID,
	})
	return p, nil
}

func detailLine(res contract.ActionResult) string {
	if !res.Success {
		return "failed: " + res.Error
	}
	if res.DryRun {
		return "dry-run validated (no changes applied)"
	}
	return res.Result
}

// Reject marks a proposal rejected (a human declined it).
func (s *remediationService) Reject(ctx context.Context, tenantID, id, approver, reason string) (*RemediationProposal, error) {
	p, err := s.rem.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if p.Status != StatusProposed {
		return nil, fmt.Errorf("%w: proposal is %s, cannot reject", errConflict, p.Status)
	}
	now := s.now()
	p.Status = StatusRejected
	p.DecidedBy = approver
	p.DecidedAt = &now
	_ = s.rem.Update(ctx, p)
	s.appendAudit(ctx, tenantID, approver, "proposal.rejected", p.ID, p.IncidentID, reason)
	return p, nil
}

// GetProposal / ListProposals / ListAudit are tenant-scoped read helpers.
func (s *remediationService) GetProposal(ctx context.Context, tenantID, id string) (*RemediationProposal, error) {
	return s.rem.Get(ctx, tenantID, id)
}
func (s *remediationService) ListProposals(ctx context.Context, tenantID, status string) ([]RemediationProposal, error) {
	return s.rem.List(ctx, tenantID, status)
}
func (s *remediationService) ListAudit(ctx context.Context, tenantID string) ([]AuditEvent, error) {
	return s.audit.List(ctx, tenantID)
}
func (s *remediationService) GetPolicy(ctx context.Context, tenantID string) (Policy, error) {
	return s.pol.Get(ctx, tenantID)
}
func (s *remediationService) PutPolicy(ctx context.Context, p Policy, actor string) error {
	if err := s.pol.Put(ctx, p); err != nil {
		return err
	}
	s.appendAudit(ctx, p.TenantID, actor, "policy.updated", "", "", string(p.Mode))
	return nil
}

// SetPaused flips the emergency kill switch for a tenant (spec §8, §20).
func (s *remediationService) SetPaused(ctx context.Context, tenantID string, paused bool, actor string) (Policy, error) {
	p, err := s.pol.Get(ctx, tenantID)
	if err != nil {
		return Policy{}, err
	}
	p.Paused = paused
	if err := s.pol.Put(ctx, p); err != nil {
		return Policy{}, err
	}
	ev := "policy.resumed"
	if paused {
		ev = "policy.paused"
	}
	s.appendAudit(ctx, tenantID, actor, ev, "", "", "")
	return p, nil
}
