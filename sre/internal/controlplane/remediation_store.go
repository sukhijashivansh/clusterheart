package controlplane

import (
	"context"
	"sort"
	"sync"
	"time"
)

// AuditEvent is an append-only record of a decision or mutation (spec §8: full
// audit trail). Every proposal/approval/execution writes one.
type AuditEvent struct {
	ID         string    `json:"id" bson:"_id"`
	TenantID   string    `json:"tenantId" bson:"tenantId"`
	At         time.Time `json:"at" bson:"at"`
	Actor      string    `json:"actor" bson:"actor"` // who/what triggered it
	Event      string    `json:"event" bson:"event"` // e.g. proposal.created, proposal.approved, remediation.executed
	ProposalID string    `json:"proposalId,omitempty" bson:"proposalId,omitempty"`
	IncidentID string    `json:"incidentId,omitempty" bson:"incidentId,omitempty"`
	Details    string    `json:"details,omitempty" bson:"details,omitempty"`
}

// AuditStore is an append-only audit log, tenant-scoped on read.
type AuditStore interface {
	Append(ctx context.Context, ev AuditEvent) error
	List(ctx context.Context, tenantID string) ([]AuditEvent, error)
}

// RemediationStore persists proposals and answers the state queries the policy
// engine needs (concurrency, cooldown).
type RemediationStore interface {
	Create(ctx context.Context, p *RemediationProposal) error
	Get(ctx context.Context, tenantID, id string) (*RemediationProposal, error)
	Update(ctx context.Context, p *RemediationProposal) error
	List(ctx context.Context, tenantID, status string) ([]RemediationProposal, error)
	ActiveCount(ctx context.Context, clusterID string) (int, error)
	LastExecutionForTarget(ctx context.Context, clusterID string, t TargetRef) (*time.Time, error)
	// RecentFailuresForTarget counts failed remediations for a target whose
	// execution finished at or after `since` (circuit-breaker input).
	RecentFailuresForTarget(ctx context.Context, clusterID string, t TargetRef, since time.Time) (int, error)
}

// --- in-memory implementations ------------------------------------------

type memRemediationStore struct {
	mu sync.RWMutex
	m  map[string]*RemediationProposal // id -> proposal
}

// NewMemRemediationStore returns an in-memory RemediationStore.
func NewMemRemediationStore() RemediationStore {
	return &memRemediationStore{m: map[string]*RemediationProposal{}}
}

func (s *memRemediationStore) Create(_ context.Context, p *RemediationProposal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *p
	s.m[p.ID] = &cp
	return nil
}

func (s *memRemediationStore) Get(_ context.Context, tenantID, id string) (*RemediationProposal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.m[id]
	if !ok || p.TenantID != tenantID { // tenant scoping: no cross-tenant access
		return nil, ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (s *memRemediationStore) Update(_ context.Context, p *RemediationProposal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[p.ID]; !ok {
		return ErrNotFound
	}
	cp := *p
	s.m[p.ID] = &cp
	return nil
}

func (s *memRemediationStore) List(_ context.Context, tenantID, status string) ([]RemediationProposal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []RemediationProposal
	for _, p := range s.m {
		if p.TenantID != tenantID {
			continue
		}
		if status != "" && string(p.Status) != status {
			continue
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *memRemediationStore) ActiveCount(_ context.Context, clusterID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, p := range s.m {
		if p.ClusterID == clusterID && p.Status == StatusApproved && p.Execution == nil {
			n++ // approved but not yet finished == in flight
		}
	}
	return n, nil
}

func (s *memRemediationStore) RecentFailuresForTarget(_ context.Context, clusterID string, t TargetRef, since time.Time) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, p := range s.m {
		if p.ClusterID != clusterID || p.Target != t || p.Status != StatusFailed || p.Execution == nil {
			continue
		}
		if !p.Execution.FinishedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

func (s *memRemediationStore) LastExecutionForTarget(_ context.Context, clusterID string, t TargetRef) (*time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var latest *time.Time
	for _, p := range s.m {
		if p.ClusterID != clusterID || p.Target != t || p.Execution == nil {
			continue
		}
		fin := p.Execution.FinishedAt
		if latest == nil || fin.After(*latest) {
			f := fin
			latest = &f
		}
	}
	return latest, nil
}

type memAuditStore struct {
	mu     sync.RWMutex
	events []AuditEvent
}

// NewMemAuditStore returns an in-memory AuditStore.
func NewMemAuditStore() AuditStore { return &memAuditStore{} }

func (s *memAuditStore) Append(_ context.Context, ev AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

func (s *memAuditStore) List(_ context.Context, tenantID string) ([]AuditEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []AuditEvent
	for _, e := range s.events {
		if e.TenantID == tenantID {
			out = append(out, e)
		}
	}
	return out, nil
}
