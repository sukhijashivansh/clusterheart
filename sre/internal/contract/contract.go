// Package contract defines the wire types exchanged between the Kubernetes
// agent and the control plane. These are deliberately small, versioned JSON
// payloads — telemetry is treated as UNTRUSTED input and may never carry an
// authorization decision or a policy override. Anything an agent reports is a
// measured fact about a cluster, nothing more.
package contract

import "time"

// APIVersion is bumped when the wire format changes incompatibly. The control
// plane rejects agents it cannot understand (see spec: version compatibility).
const APIVersion = "v1"

// RegisterRequest is sent once by an operator (admin-authenticated) to enroll a
// cluster. The control plane mints an agent token in response.
type RegisterRequest struct {
	TenantID    string `json:"tenantId"`
	ClusterName string `json:"clusterName"`
	Environment string `json:"environment"` // e.g. production, staging, dev
	Region      string `json:"region,omitempty"`
}

// RegisterResponse returns the cluster identity and the one-time agent token.
// The token is shown exactly once; the control plane stores only its hash.
type RegisterResponse struct {
	ClusterID  string `json:"clusterId"`
	AgentToken string `json:"agentToken"`
	APIVersion string `json:"apiVersion"`
}

// Heartbeat is sent by the agent on a fixed interval so the control plane can
// track connectivity, version compatibility, and agent health.
type Heartbeat struct {
	APIVersion   string    `json:"apiVersion"`
	AgentVersion string    `json:"agentVersion"`
	K8sVersion   string    `json:"k8sVersion,omitempty"`
	SentAt       time.Time `json:"sentAt"`
	// Bounded self-telemetry about the agent process itself (spec §17).
	BufferedSnapshots int `json:"bufferedSnapshots"`
	CollectErrors     int `json:"collectErrors"`
}

// TelemetryBatch carries one or more cluster-health snapshots. The agent buffers
// a bounded number of these during outages and flushes on reconnect.
type TelemetryBatch struct {
	APIVersion string            `json:"apiVersion"`
	Snapshots  []ClusterSnapshot `json:"snapshots"`
}

// ClusterSnapshot is a point-in-time, bounded summary of cluster health. It is
// intentionally aggregate (counts, not full object dumps) so the primary store
// never becomes a raw-telemetry sink. Deep evidence is fetched on demand later.
type ClusterSnapshot struct {
	CollectedAt time.Time       `json:"collectedAt"`
	Nodes       NodeSummary     `json:"nodes"`
	Pods        PodSummary      `json:"pods"`
	Warnings    []WarningEvent  `json:"warnings"`    // recent warning events (bounded)
	Workloads   WorkloadSummary `json:"workloads"`
}

type NodeSummary struct {
	Total    int `json:"total"`
	Ready    int `json:"ready"`
	NotReady int `json:"notReady"`
}

type PodSummary struct {
	Total     int `json:"total"`
	Running   int `json:"running"`
	Pending   int `json:"pending"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	// Restart pressure: pods whose restart count is climbing (symptom only).
	HighRestart int `json:"highRestart"`
}

type WorkloadSummary struct {
	Deployments  int `json:"deployments"`
	StatefulSets int `json:"statefulSets"`
	DaemonSets   int `json:"daemonSets"`
	Jobs         int `json:"jobs"`
}

// WarningEvent is a redacted, bounded copy of a Kubernetes warning event.
type WarningEvent struct {
	Namespace string    `json:"namespace"`
	Reason    string    `json:"reason"`
	Object    string    `json:"object"`  // kind/name
	Message   string    `json:"message"` // redacted upstream in the agent
	Count     int32     `json:"count"`
	LastSeen  time.Time `json:"lastSeen"`
}

// ClusterStatus is the control plane's view of a cluster, returned to operators.
type ClusterStatus struct {
	ClusterID     string           `json:"clusterId"`
	TenantID      string           `json:"tenantId"`
	Name          string           `json:"name"`
	Environment   string           `json:"environment"`
	Region        string           `json:"region,omitempty"`
	Connected     bool             `json:"connected"`
	LastHeartbeat *time.Time       `json:"lastHeartbeat,omitempty"`
	AgentVersion  string           `json:"agentVersion,omitempty"`
	K8sVersion    string           `json:"k8sVersion,omitempty"`
	Latest        *ClusterSnapshot `json:"latest,omitempty"`
}

// ErrorResponse is the uniform error envelope.
type ErrorResponse struct {
	Error string `json:"error"`
}
