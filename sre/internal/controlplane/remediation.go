package controlplane

import (
	"errors"
	"fmt"
	"time"
)

// Remediation safety model (spec §7, §8). The AI never executes anything: it may
// only PROPOSE a typed, schema-validated action. Deterministic code in this file
// and policy.go decides; a separate in-cluster controller (Milestone 3 slice B)
// executes. There is no path that turns free-form text into a cluster mutation.

// ActionType enumerates the EXPLICITLY supported remediation actions. Anything
// not in the registry is rejected — no arbitrary patches or shell.
type ActionType string

const (
	// ActionRolloutRestart triggers a rolling restart of a controller-owned
	// workload (Deployment/StatefulSet/DaemonSet). Reversible via rollout undo.
	ActionRolloutRestart ActionType = "RolloutRestart"
)

// RiskClass classifies the blast radius of an action.
type RiskClass string

const (
	RiskLow    RiskClass = "low"
	RiskMedium RiskClass = "medium"
	RiskHigh   RiskClass = "high"
)

// Mode is the execution posture resolved by policy.
type Mode string

const (
	ModeOff              Mode = "off"               // remediation disabled
	ModeDryRun           Mode = "dry-run"           // validate + plan, never apply
	ModeRecommend        Mode = "recommend"         // surface a proposal, wait for a human
	ModeApprovalRequired Mode = "approval-required" // validate, apply only after approval
	ModeAuto             Mode = "auto"              // apply allowlisted actions automatically
)

// ProposalStatus is the lifecycle state of a remediation proposal.
type ProposalStatus string

const (
	StatusBlocked  ProposalStatus = "blocked"  // policy denied it
	StatusProposed ProposalStatus = "proposed" // awaiting approval / recommendation
	StatusApproved ProposalStatus = "approved" // approved, execution attempted
	StatusRejected ProposalStatus = "rejected" // a human rejected it
	StatusExecuted ProposalStatus = "executed" // applied (or dry-run applied)
	StatusFailed   ProposalStatus = "failed"   // execution failed
)

// TargetRef identifies the object an action targets.
type TargetRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (t TargetRef) String() string { return t.Kind + "/" + t.Namespace + "/" + t.Name }

// PolicyDecision is the deterministic verdict for a proposal. It is produced
// without any AI involvement.
type PolicyDecision struct {
	Allowed     bool      `json:"allowed"`
	Reasons     []string  `json:"reasons"`
	Mode        Mode      `json:"mode"`
	EvaluatedAt time.Time `json:"evaluatedAt"`
}

// ActionExecution records what happened when an approved action ran. In slice A
// every execution is a DRY RUN (no cluster contact); slice B adds the real
// in-cluster executor and fills BeforeState from the live object.
type ActionExecution struct {
	StartedAt          time.Time `json:"startedAt"`
	FinishedAt         time.Time `json:"finishedAt"`
	DryRun             bool      `json:"dryRun"`
	Success            bool      `json:"success"`
	PlannedChange      string    `json:"plannedChange"`
	BeforeState        string    `json:"beforeState"`
	Result             string    `json:"result"`
	RollbackAvailable  bool      `json:"rollbackAvailable"`
	VerificationPassed *bool     `json:"verificationPassed,omitempty"`
	Error              string    `json:"error,omitempty"`
}

// RemediationProposal is the full action contract (spec §7). Every field that
// matters for safety and audit is captured.
type RemediationProposal struct {
	ID         string            `json:"id"`
	IncidentID string            `json:"incidentId"`
	TenantID   string            `json:"tenantId"`
	ClusterID  string            `json:"clusterId"`
	Action     ActionType        `json:"action"`
	Target     TargetRef         `json:"target"`
	Params     map[string]string `json:"params,omitempty"`
	Rationale  string            `json:"rationale"` // evidence-linked "why"
	RiskClass  RiskClass         `json:"riskClass"`
	Reversible bool              `json:"reversible"`
	Status     ProposalStatus    `json:"status"`
	Decision   *PolicyDecision   `json:"decision,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
	CreatedBy  string            `json:"createdBy"` // "operator:<id>" or "system:<rule>" — never executes AI
	DecidedBy  string            `json:"decidedBy,omitempty"`
	DecidedAt  *time.Time        `json:"decidedAt,omitempty"`
	Execution  *ActionExecution  `json:"execution,omitempty"`
}

// --- typed action registry ----------------------------------------------

type actionSpec struct {
	risk       RiskClass
	reversible bool
	validate   func(t TargetRef, params map[string]string) error
	plan       func(t TargetRef) string
}

var actionRegistry = map[ActionType]actionSpec{
	ActionRolloutRestart: {
		risk:       RiskMedium,
		reversible: true, // `kubectl rollout undo` restores the previous revision
		validate: func(t TargetRef, params map[string]string) error {
			switch t.Kind {
			case "Deployment", "StatefulSet", "DaemonSet":
			default:
				return fmt.Errorf("RolloutRestart supports Deployment/StatefulSet/DaemonSet, not %q", t.Kind)
			}
			if t.Namespace == "" || t.Name == "" {
				return errors.New("target namespace and name are required")
			}
			if len(params) > 0 {
				return errors.New("RolloutRestart takes no parameters")
			}
			return nil
		},
		plan: func(t TargetRef) string {
			return fmt.Sprintf("patch %s/%s in %s: set spec.template annotation "+
				"kubectl.kubernetes.io/restartedAt to trigger a rolling restart (no spec change)",
				t.Kind, t.Name, t.Namespace)
		},
	},
}

// lookupAction returns the spec for a supported action, or an error if the
// action is unknown (never execute something not in the registry).
func lookupAction(a ActionType) (actionSpec, error) {
	spec, ok := actionRegistry[a]
	if !ok {
		return actionSpec{}, fmt.Errorf("unsupported action %q", a)
	}
	return spec, nil
}
