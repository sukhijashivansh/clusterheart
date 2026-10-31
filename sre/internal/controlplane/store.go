package controlplane

import (
	"context"
	"errors"
	"sync"
	"time"

	"clusterheart/sre/internal/contract"
)

// ErrNotFound is returned by Store implementations when a record is absent.
var ErrNotFound = errors.New("not found")

// maxSnapshotsPerCluster bounds the in-memory/Mongo snapshot history so a chatty
// or hostile agent cannot grow storage without limit (spec: bounded telemetry).
const maxSnapshotsPerCluster = 120

// Cluster is the control plane's record of a registered cluster. AgentTokenHash
// is the only credential material stored; the plaintext token is shown once at
// registration and never persisted.
type Cluster struct {
	ID             string
	TenantID       string
	Name           string
	Environment    string
	Region         string
	AgentTokenHash string
	// RemediationTokenHash is set only when an operator opts the cluster into
	// remediation (separate identity from the observe agent). Empty => no
	// in-cluster executor is authorized.
	RemediationTokenHash string
	CreatedAt            time.Time
	LastHeartbeat        *time.Time
	AgentVersion         string
	K8sVersion           string
	Kind                 string // contract.KindKubernetes (default) or contract.KindServer
}

// Store is the persistence boundary. It is an interface so the control plane can
// run against MongoDB in production and an in-memory fake in tests, and so no
// handler logic depends on the database directly.
type Store interface {
	CreateCluster(ctx context.Context, c Cluster) error
	GetCluster(ctx context.Context, clusterID string) (Cluster, error)
	ListClusters(ctx context.Context, tenantID string) ([]Cluster, error)
	// AllClusters returns every registered cluster across tenants (for the
	// control plane's own background sweeps, e.g. agent-down detection).
	AllClusters(ctx context.Context) ([]Cluster, error)
	SetRemediationTokenHash(ctx context.Context, clusterID, hash string) error
	SetAgentTokenHash(ctx context.Context, clusterID, hash string) error
	RenameCluster(ctx context.Context, clusterID, name string) error
	SetClusterEnvironment(ctx context.Context, clusterID, environment string) error
	DeleteCluster(ctx context.Context, clusterID string) error
	RecordHeartbeat(ctx context.Context, clusterID string, hb contract.Heartbeat, at time.Time) error
	SaveSnapshots(ctx context.Context, clusterID string, snaps []contract.ClusterSnapshot) error
	LatestSnapshot(ctx context.Context, clusterID string) (*contract.ClusterSnapshot, error)
	// WorkloadUsage aggregates per-workload peak usage from snapshots since a time.
	WorkloadUsage(ctx context.Context, clusterID string, since time.Time) ([]WorkloadUsage, error)
}

// --- in-memory implementation (tests, local dev) -------------------------

type memStore struct {
	mu        sync.RWMutex
	clusters  map[string]Cluster
	snapshots map[string][]contract.ClusterSnapshot // clusterID -> bounded ring
	hostPts   map[string][]HostPoint                // clusterID -> metric history
	gpuPts    map[string][]GPUPoint
}

// NewMemStore returns an in-memory Store. Not durable; for tests and local runs.
func NewMemStore() Store {
	return &memStore{
		clusters:  map[string]Cluster{},
		snapshots: map[string][]contract.ClusterSnapshot{},
	}
}

func (m *memStore) CreateCluster(_ context.Context, c Cluster) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.clusters[c.ID]; ok {
		return errors.New("cluster already exists")
	}
	m.clusters[c.ID] = c
	return nil
}

func (m *memStore) SetRemediationTokenHash(_ context.Context, clusterID, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clusters[clusterID]
	if !ok {
		return ErrNotFound
	}
	c.RemediationTokenHash = hash
	m.clusters[clusterID] = c
	return nil
}

func (m *memStore) SetAgentTokenHash(_ context.Context, clusterID, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clusters[clusterID]
	if !ok {
		return ErrNotFound
	}
	c.AgentTokenHash = hash
	m.clusters[clusterID] = c
	return nil
}

func (m *memStore) RenameCluster(_ context.Context, clusterID, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clusters[clusterID]
	if !ok {
		return ErrNotFound
	}
	c.Name = name
	m.clusters[clusterID] = c
	return nil
}

func (m *memStore) SetClusterEnvironment(_ context.Context, clusterID, environment string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clusters[clusterID]
	if !ok {
		return ErrNotFound
	}
	c.Environment = environment
	m.clusters[clusterID] = c
	return nil
}

func (m *memStore) DeleteCluster(_ context.Context, clusterID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.clusters[clusterID]; !ok {
		return ErrNotFound
	}
	delete(m.clusters, clusterID)
	delete(m.snapshots, clusterID)
	return nil
}

func (m *memStore) GetCluster(_ context.Context, clusterID string) (Cluster, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.clusters[clusterID]
	if !ok {
		return Cluster{}, ErrNotFound
	}
	return c, nil
}

func (m *memStore) ListClusters(_ context.Context, tenantID string) ([]Cluster, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Cluster
	for _, c := range m.clusters {
		if c.TenantID == tenantID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memStore) AllClusters(_ context.Context) ([]Cluster, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Cluster, 0, len(m.clusters))
	for _, c := range m.clusters {
		out = append(out, c)
	}
	return out, nil
}

func (m *memStore) RecordHeartbeat(_ context.Context, clusterID string, hb contract.Heartbeat, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clusters[clusterID]
	if !ok {
		return ErrNotFound
	}
	t := at
	c.LastHeartbeat = &t
	c.AgentVersion = hb.AgentVersion
	if hb.K8sVersion != "" {
		c.K8sVersion = hb.K8sVersion
	}
	m.clusters[clusterID] = c
	return nil
}

func (m *memStore) SaveSnapshots(_ context.Context, clusterID string, snaps []contract.ClusterSnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.clusters[clusterID]; !ok {
		return ErrNotFound
	}
	ring := append(m.snapshots[clusterID], snaps...)
	if len(ring) > maxSnapshotsPerCluster {
		ring = ring[len(ring)-maxSnapshotsPerCluster:]
	}
	m.snapshots[clusterID] = ring
	return nil
}

func (m *memStore) LatestSnapshot(_ context.Context, clusterID string) (*contract.ClusterSnapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ring := m.snapshots[clusterID]
	if len(ring) == 0 {
		return nil, nil
	}
	s := ring[len(ring)-1]
	return &s, nil
}
