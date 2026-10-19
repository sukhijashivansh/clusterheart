package controlplane

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Policy is the per-tenant remediation policy. Defaults are deliberately SAFE:
// approval-required, no namespaces actionable until explicitly allowlisted, and
// production treated as protected. This realises "read-only by default".
type Policy struct {
	TenantID              string              `json:"tenantId" bson:"_id"`
	Mode                  Mode                `json:"mode" bson:"mode"`
	AllowedActions        map[ActionType]bool `json:"allowedActions" bson:"allowedActions"`
	NamespaceAllowlist    []string            `json:"namespaceAllowlist" bson:"namespaceAllowlist"` // empty => nothing actionable; "*" => all
	ProtectedEnvironments []string            `json:"protectedEnvironments" bson:"protectedEnvironments"`
	MaxConcurrent         int                 `json:"maxConcurrent" bson:"maxConcurrent"`
	PerWorkloadCooldown   time.Duration       `json:"perWorkloadCooldown" bson:"perWorkloadCooldown"`
	Paused                bool                `json:"paused" bson:"paused"` // tenant-wide emergency kill switch
	// Scoped kill switches (spec §8): pause automation for specific clusters or
	// workloads without pausing the whole tenant. Workloads are keyed by
	// TargetRef.String() ("Kind/Namespace/Name").
	PausedClusters  []string `json:"pausedClusters,omitempty" bson:"pausedClusters,omitempty"`
	PausedWorkloads []string `json:"pausedWorkloads,omitempty" bson:"pausedWorkloads,omitempty"`
	// Circuit breaker / loop detection (spec §8): once a target has accumulated
	// CircuitBreakerFailures failed remediations within CircuitBreakerWindow, new
	// proposals for that target are denied until the window passes - so a
	// recurring incident cannot drive an endless restart loop. Zero disables it.
	CircuitBreakerFailures int           `json:"circuitBreakerFailures" bson:"circuitBreakerFailures"`
	CircuitBreakerWindow   time.Duration `json:"circuitBreakerWindow" bson:"circuitBreakerWindow"`
	// MaxReplicas bounds the Scale action (spec §8: bounded replica adjustments).
	// Zero disables the bound (structural hardMaxReplicas still applies).
	MaxReplicas int `json:"maxReplicas" bson:"maxReplicas"`
}

// DefaultPolicy returns the safe default for a new tenant.
func DefaultPolicy(tenantID string) Policy {
	return Policy{
		TenantID:               tenantID,
		Mode:                   ModeApprovalRequired,
		AllowedActions:         map[ActionType]bool{ActionRolloutRestart: true},
		NamespaceAllowlist:     nil, // nothing actionable until an operator opts in
		ProtectedEnvironments:  []string{"production", "prod"},
		MaxConcurrent:          1,
		PerWorkloadCooldown:    10 * time.Minute,
		Paused:                 false,
		CircuitBreakerFailures: 3,
		CircuitBreakerWindow:   30 * time.Minute,
		MaxReplicas:            10,
	}
}

func (p Policy) nsAllowed(ns string) bool {
	for _, a := range p.NamespaceAllowlist {
		if a == "*" || a == ns {
			return true
		}
	}
	return false
}

func strSliceContains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func (p Policy) isProtected(env string) bool {
	for _, e := range p.ProtectedEnvironments {
		if e == env {
			return true
		}
	}
	return false
}

// policyInput carries the live state the engine needs for rate/concurrency/
// cooldown checks (supplied by the service from the store).
type policyInput struct {
	environment       string
	clusterID         string
	activeCount       int
	lastForTarget     *time.Time
	recentFailures    int  // failed remediations for this target within the breaker window
	requestedReplicas int  // for Scale: the desired replica count
	hasReplicas       bool // whether requestedReplicas is meaningful
}

// policyEngine evaluates proposals deterministically. It never calls an LLM.
// (Named to avoid colliding with the detection Engine in detect.go.)
type policyEngine struct{}

// Evaluate returns the policy verdict for an action against a target.
func (policyEngine) Evaluate(pol Policy, action ActionType, target TargetRef, in policyInput, now time.Time) PolicyDecision {
	var reasons []string
	deny := func(r string) { reasons = append(reasons, r) }

	if pol.Paused {
		deny("emergency pause is active for this tenant")
	}
	if in.clusterID != "" && strSliceContains(pol.PausedClusters, in.clusterID) {
		deny("emergency pause is active for this cluster")
	}
	if strSliceContains(pol.PausedWorkloads, target.String()) {
		deny("emergency pause is active for this workload")
	}
	if _, err := lookupAction(action); err != nil {
		deny(err.Error())
	}
	if !pol.AllowedActions[action] {
		deny("action is not allowlisted by policy")
	}
	if !pol.nsAllowed(target.Namespace) {
		deny("namespace is not in the policy allowlist")
	}
	if pol.MaxConcurrent > 0 && in.activeCount >= pol.MaxConcurrent {
		deny("maximum concurrent remediations reached")
	}
	if pol.PerWorkloadCooldown > 0 && in.lastForTarget != nil && now.Sub(*in.lastForTarget) < pol.PerWorkloadCooldown {
		deny("cooldown is active for this target")
	}
	if pol.CircuitBreakerFailures > 0 && in.recentFailures >= pol.CircuitBreakerFailures {
		deny("circuit breaker is open: too many recent failed remediations for this target")
	}
	if action == ActionScale && in.hasReplicas && pol.MaxReplicas > 0 && in.requestedReplicas > pol.MaxReplicas {
		deny(fmt.Sprintf("requested replicas %d exceeds policy maximum %d", in.requestedReplicas, pol.MaxReplicas))
	}

	mode := pol.Mode
	if mode == ModeOff {
		deny("remediation is disabled (mode=off)")
	}
	// Production protection: auto never applies in a protected environment; it is
	// downgraded to approval-required.
	note := ""
	if mode == ModeAuto && pol.isProtected(in.environment) {
		mode = ModeApprovalRequired
		note = "auto downgraded to approval-required in a protected environment"
	}

	allowed := len(reasons) == 0
	if allowed && note != "" {
		reasons = append(reasons, note) // informational, not a denial
	}
	return PolicyDecision{Allowed: allowed, Reasons: reasons, Mode: mode, EvaluatedAt: now}
}

// --- per-tenant policy store (in-memory) --------------------------------

// PolicyStore persists per-tenant policies.
type PolicyStore interface {
	Get(ctx context.Context, tenantID string) (Policy, error)
	Put(ctx context.Context, p Policy) error
}

type memPolicyStore struct {
	mu sync.RWMutex
	m  map[string]Policy
}

// NewMemPolicyStore returns an in-memory PolicyStore seeded with safe defaults.
func NewMemPolicyStore() PolicyStore {
	return &memPolicyStore{m: map[string]Policy{}}
}

func (s *memPolicyStore) Get(_ context.Context, tenantID string) (Policy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.m[tenantID]; ok {
		return p, nil
	}
	return DefaultPolicy(tenantID), nil
}

func (s *memPolicyStore) Put(_ context.Context, p Policy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[p.TenantID] = p
	return nil
}
