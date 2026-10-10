package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
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
	engine    policyEngine
	log       *slog.Logger
	now       func() time.Time
}

func newRemediationService(rem RemediationStore, pol PolicyStore, audit AuditStore, incidents IncidentStore, clusters Store, log *slog.Logger) *remediationService {
	if log == nil {
		log = slog.Default()
	}
	return &remediationService{
		rem: rem, pol: pol, audit: audit, incidents: incidents, clusters: clusters,
		engine: policyEngine{}, log: log, now: time.Now,
	}
}

func (s *remediationService) appendAudit(ctx context.Context, tenantID, actor, event, proposalID, incidentID, details string) {
	_ = s.audit.Append(ctx, AuditEvent{
		ID: newID("aud"), TenantID: tenantID, At: s.now(), Actor: actor,
		Event: event, ProposalID: proposalID, IncidentID: incidentID, Details: details,
	})
}

// evaluate gathers live state and runs the deterministic policy engine.
func (s *remediationService) evaluate(ctx context.Context, tenantID, clusterID string, action ActionType, target TargetRef) PolicyDecision {
	pol, _ := s.pol.Get(ctx, tenantID)
	env := ""
	if c, err := s.clusters.GetCluster(ctx, clusterID); err == nil {
		env = c.Environment
	}
	active, _ := s.rem.ActiveCount(ctx, clusterID)
	last, _ := s.rem.LastExecutionForTarget(ctx, clusterID, target)
	return s.engine.Evaluate(pol, action, target, policyInput{environment: env, activeCount: active, lastForTarget: last}, s.now())
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

	dec := s.evaluate(ctx, tenantID, inc.ClusterID, action, target)
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
		// slice A: auto resolves to a dry run (no in-cluster executor yet).
		s.execute(p)
		_ = s.rem.Create(ctx, p)
		s.appendAudit(ctx, tenantID, createdBy, "proposal.created", p.ID, incidentID, string(dec.Mode))
		s.appendAudit(ctx, tenantID, createdBy, "remediation.executed", p.ID, incidentID, "dry-run")
	default: // recommend / approval-required
		p.Status = StatusProposed
		_ = s.rem.Create(ctx, p)
		s.appendAudit(ctx, tenantID, createdBy, "proposal.created", p.ID, incidentID, string(dec.Mode))
	}
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
	dec := s.evaluate(ctx, tenantID, p.ClusterID, p.Action, p.Target)
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
	p.Status = StatusApproved
	_ = s.rem.Update(ctx, p)
	s.appendAudit(ctx, tenantID, approver, "proposal.approved", p.ID, p.IncidentID, "")
	s.execute(p)
	_ = s.rem.Update(ctx, p)
	s.appendAudit(ctx, tenantID, approver, "remediation.executed", p.ID, p.IncidentID, "dry-run")
	return p, nil
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

// execute performs the action. SLICE A: dry run only — it validates and records
// the planned change but contacts no cluster. Slice B replaces this with the
// in-cluster controller that captures before-state, applies, verifies, and can
// roll back.
func (s *remediationService) execute(p *RemediationProposal) {
	spec, _ := lookupAction(p.Action)
	start := s.now()
	p.Execution = &ActionExecution{
		StartedAt:         start,
		FinishedAt:        s.now(),
		DryRun:            true,
		Success:           true,
		PlannedChange:     spec.plan(p.Target),
		BeforeState:       "(not captured in dry-run)",
		Result:            "dry-run: validated against policy; no changes applied",
		RollbackAvailable: spec.reversible,
	}
	p.Status = StatusExecuted
}
