package controlplane

import (
	"context"
	"sync"
	"time"

	"clusterheart/sre/internal/contract"
)

// maxEvidence bounds how much evidence an incident retains (most recent wins).
const maxEvidence = 20

// Incident is a detected, evidence-backed operational problem. It is a SYMPTOM
// with supporting facts — not a root-cause claim. Root-cause analysis (AI) is a
// separate, later stage that attaches hypotheses to an incident.
type Incident struct {
	ID          string               `json:"id"`
	Fingerprint string               `json:"fingerprint"`
	TenantID    string               `json:"tenantId"`
	ClusterID   string               `json:"clusterId"`
	Type        contract.ProblemType `json:"type"`
	Severity    string               `json:"severity"` // high | medium (critical/low reserved)
	Status      string               `json:"status"`   // open | resolved
	Title       string               `json:"title"`
	Namespace   string               `json:"namespace,omitempty"`
	Workload    string               `json:"workload,omitempty"` // kind/name
	Node        string               `json:"node,omitempty"`
	FirstSeen   time.Time            `json:"firstSeen"`
	LastSeen    time.Time            `json:"lastSeen"`
	Occurrences int                  `json:"occurrences"`
	Evidence    []EvidenceItem       `json:"evidence"`
	ResolvedAt  *time.Time           `json:"resolvedAt,omitempty"`
}

// EvidenceItem is one measured fact supporting an incident. Messages are
// redacted upstream in the agent.
type EvidenceItem struct {
	ObservedAt   time.Time `json:"observedAt"`
	Ref          string    `json:"ref"`    // ns/pod[/container] or kind/name
	Reason       string    `json:"reason"` // problem type
	Message      string    `json:"message"`
	RestartCount int32     `json:"restartCount,omitempty"`
}

// observation is a single detector output for one snapshot cycle.
type observation struct {
	fingerprint string
	typ         contract.ProblemType
	severity    string
	title       string
	namespace   string
	workload    string
	node        string
	evidence    EvidenceItem
}

// IncidentStore persists incidents and their lifecycle. Reads are tenant-scoped.
type IncidentStore interface {
	// UpsertObservation creates a new open incident for (clusterID, fingerprint)
	// or updates the existing one (lastSeen, occurrences++, bounded evidence).
	// Returns created=true only when a NEW open incident was opened (so callers
	// can notify once per incident, not once per occurrence).
	UpsertObservation(ctx context.Context, tenantID, clusterID string, at time.Time, obs observation) (created bool, err error)
	// ResolveMissing marks open incidents for clusterID whose fingerprint is not
	// in active as resolved. Returns the number resolved.
	ResolveMissing(ctx context.Context, clusterID string, active map[string]bool, at time.Time) (int, error)
	ListIncidents(ctx context.Context, tenantID, status string) ([]Incident, error)
	GetIncident(ctx context.Context, tenantID, id string) (Incident, error)
}

// --- in-memory implementation -------------------------------------------

type memIncidentStore struct {
	mu    sync.RWMutex
	byKey map[string]*Incident // clusterID|fingerprint -> incident
	byID  map[string]*Incident
}

// NewMemIncidentStore returns an in-memory IncidentStore (tests/local dev).
func NewMemIncidentStore() IncidentStore {
	return &memIncidentStore{byKey: map[string]*Incident{}, byID: map[string]*Incident{}}
}

func incKey(clusterID, fingerprint string) string { return clusterID + "|" + fingerprint }

func (m *memIncidentStore) UpsertObservation(_ context.Context, tenantID, clusterID string, at time.Time, obs observation) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := incKey(clusterID, obs.fingerprint)
	inc, ok := m.byKey[k]
	if ok && inc.Status == "open" {
		inc.LastSeen = at
		inc.Occurrences++
		inc.Evidence = appendEvidence(inc.Evidence, obs.evidence)
		return false, nil
	}
	// New incident, or re-open a previously resolved one with a fresh identity.
	id := newID("inc")
	ni := &Incident{
		ID: id, Fingerprint: obs.fingerprint, TenantID: tenantID, ClusterID: clusterID,
		Type: obs.typ, Severity: obs.severity, Status: "open", Title: obs.title,
		Namespace: obs.namespace, Workload: obs.workload, Node: obs.node,
		FirstSeen: at, LastSeen: at, Occurrences: 1,
		Evidence: appendEvidence(nil, obs.evidence),
	}
	m.byKey[k] = ni
	m.byID[id] = ni
	return true, nil
}

func (m *memIncidentStore) ResolveMissing(_ context.Context, clusterID string, active map[string]bool, at time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, inc := range m.byKey {
		if inc.ClusterID != clusterID || inc.Status != "open" {
			continue
		}
		if !active[inc.Fingerprint] {
			t := at
			inc.Status = "resolved"
			inc.ResolvedAt = &t
			n++
			_ = k
		}
	}
	return n, nil
}

func (m *memIncidentStore) ListIncidents(_ context.Context, tenantID, status string) ([]Incident, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Incident
	for _, inc := range m.byID {
		if inc.TenantID != tenantID {
			continue
		}
		if status != "" && inc.Status != status {
			continue
		}
		out = append(out, *inc)
	}
	return out, nil
}

func (m *memIncidentStore) GetIncident(_ context.Context, tenantID, id string) (Incident, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inc, ok := m.byID[id]
	if !ok || inc.TenantID != tenantID { // tenant scoping: don't leak across tenants
		return Incident{}, ErrNotFound
	}
	return *inc, nil
}

func appendEvidence(ev []EvidenceItem, item EvidenceItem) []EvidenceItem {
	ev = append(ev, item)
	if len(ev) > maxEvidence {
		ev = ev[len(ev)-maxEvidence:]
	}
	return ev
}
