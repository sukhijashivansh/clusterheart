package controlplane

// Runbooks (spec §12): operator-authored, versioned playbooks that map an
// incident type to a typed remediation action. They make "the known fix for a
// known problem" explicit and operator-controlled - distinct from AI diagnosis.
//
// Safety: a runbook only PRE-FILLS a proposal's action+params; proposing still
// goes through the deterministic, policy-checked CreateProposal path, so a
// runbook can never bypass policy/approval. A runbook's action must be a known,
// valid action (validated on save). One active runbook per (tenant, incidentType).

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Runbook maps an incident type to a typed, validated remediation action.
type Runbook struct {
	ID           string            `json:"id" bson:"_id"`
	TenantID     string            `json:"tenantId" bson:"tenantId"`
	IncidentType string            `json:"incidentType" bson:"incidentType"`
	Action       ActionType        `json:"action" bson:"action"`
	Params       map[string]string `json:"params,omitempty" bson:"params,omitempty"`
	Enabled      bool              `json:"enabled" bson:"enabled"`
	Version      int               `json:"version" bson:"version"`
	UpdatedAt    time.Time         `json:"updatedAt" bson:"updatedAt"`
	UpdatedBy    string            `json:"updatedBy" bson:"updatedBy"`
}

// RunbookStore persists runbooks, tenant-scoped. One per (tenant, incidentType).
type RunbookStore interface {
	Upsert(ctx context.Context, rb Runbook) (Runbook, error)
	List(ctx context.Context, tenantID string) ([]Runbook, error)
	GetForType(ctx context.Context, tenantID, incidentType string) (Runbook, error)
}

type memRunbookStore struct {
	mu sync.RWMutex
	m  map[string]Runbook // tenant|incidentType -> runbook
}

// NewMemRunbookStore returns an in-memory RunbookStore.
func NewMemRunbookStore() RunbookStore { return &memRunbookStore{m: map[string]Runbook{}} }

func rbKey(tenantID, incidentType string) string { return tenantID + "|" + incidentType }

func (s *memRunbookStore) Upsert(_ context.Context, rb Runbook) (Runbook, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := rbKey(rb.TenantID, rb.IncidentType)
	if prev, ok := s.m[k]; ok {
		rb.ID, rb.Version = prev.ID, prev.Version+1
	} else {
		rb.ID, rb.Version = newID("rb"), 1
	}
	s.m[k] = rb
	return rb, nil
}

func (s *memRunbookStore) List(_ context.Context, tenantID string) ([]Runbook, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Runbook
	for _, rb := range s.m {
		if rb.TenantID == tenantID {
			out = append(out, rb)
		}
	}
	return out, nil
}

func (s *memRunbookStore) GetForType(_ context.Context, tenantID, incidentType string) (Runbook, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rb, ok := s.m[rbKey(tenantID, incidentType)]
	if !ok {
		return Runbook{}, ErrNotFound
	}
	return rb, nil
}

// validateRunbook checks the action is known and its params validate against the
// incident type's typical target shape. (Target is supplied at propose time from
// the incident, so only action+params are checked here.)
func validateRunbook(rb Runbook) error {
	if rb.IncidentType == "" {
		return fmt.Errorf("incidentType is required")
	}
	spec, err := lookupAction(rb.Action)
	if err != nil {
		return err
	}
	// Validate params against a representative target (kind inferred as Deployment,
	// the common controller case) so obviously-bad params are rejected on save.
	if err := spec.validate(TargetRef{Kind: "Deployment", Namespace: "x", Name: "y"}, rb.Params); err != nil {
		return fmt.Errorf("invalid runbook params: %w", err)
	}
	return nil
}
