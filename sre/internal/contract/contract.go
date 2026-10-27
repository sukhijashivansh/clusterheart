// Package contract defines the wire types exchanged between the Kubernetes
// agent and the control plane. These are deliberately small, versioned JSON
// payloads - telemetry is treated as UNTRUSTED input and may never carry an
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
//
// Problems are bounded lists of measured, per-object fault signals (not
// diagnoses). The control plane turns these facts into incidents; the agent
// never decides what is an incident.
type ClusterSnapshot struct {
	CollectedAt      time.Time         `json:"collectedAt"`
	Nodes            NodeSummary       `json:"nodes"`
	Pods             PodSummary        `json:"pods"`
	Warnings         []WarningEvent    `json:"warnings"` // recent warning events (bounded)
	Workloads        WorkloadSummary   `json:"workloads"`
	PodProblems      []PodProblem      `json:"podProblems,omitempty"`
	WorkloadProblems []WorkloadProblem `json:"workloadProblems,omitempty"`
	// PodList is a bounded inventory of individual pods (capped by the agent) so
	// the browser dashboard can show a real pod table with live CPU/memory.
	// PodListTruncated is how many pods the cap dropped.
	PodList          []PodInfo `json:"podList,omitempty"`
	PodListTruncated int       `json:"podListTruncated,omitempty"`
}

// PodInfo is one pod's inventory row: identity, status, and (when metrics-server
// is present) live usage. CPUMilli is millicores, MemMi is mebibytes.
type PodInfo struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Ready     string `json:"ready"` // e.g. "2/3"
	Restarts  int32  `json:"restarts"`
	Node      string `json:"node,omitempty"`
	Owner     string `json:"owner,omitempty"` // controller, e.g. "Deployment/x" or "Job/y"
	CPUMilli  int64  `json:"cpuMilli,omitempty"`
	MemMi     int64  `json:"memMi,omitempty"`
}

// ProblemType enumerates the deterministic fault signals the agent reports.
// These are symptoms derived from Kubernetes object status - never root causes.
type ProblemType string

const (
	ProblemCrashLoopBackOff ProblemType = "CrashLoopBackOff"
	ProblemOOMKilled        ProblemType = "OOMKilled"
	ProblemImagePullBackOff ProblemType = "ImagePullBackOff"
	ProblemUnschedulable    ProblemType = "Unschedulable"
	ProblemRolloutStuck     ProblemType = "RolloutStuck"
	// ProblemAgentDown is raised by the CONTROL PLANE (not the agent) when a
	// previously-connected cluster stops sending telemetry - so the platform
	// never silently reports a cluster healthy while it is actually blind.
	ProblemAgentDown ProblemType = "AgentDown"
)

// PodProblem is a measured fault on a pod/container (from pod status).
type PodProblem struct {
	Namespace    string      `json:"namespace"`
	Pod          string      `json:"pod"`
	Owner        string      `json:"owner,omitempty"`     // kind/name of the pod's controller, if any
	Container    string      `json:"container,omitempty"` // for container-level problems
	Node         string      `json:"node,omitempty"`
	Type         ProblemType `json:"type"`
	RestartCount int32       `json:"restartCount,omitempty"`
	Message      string      `json:"message"` // redacted upstream
}

// WorkloadProblem is a measured fault on a controller object (e.g. a Deployment
// rollout that exceeded its progress deadline).
type WorkloadProblem struct {
	Namespace string      `json:"namespace"`
	Kind      string      `json:"kind"`
	Name      string      `json:"name"`
	Type      ProblemType `json:"type"`
	Message   string      `json:"message"` // redacted upstream
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

// --- remediation controller protocol ------------------------------------
//
// The in-cluster remediation controller authenticates with its OWN per-cluster
// token (separate from the observe agent), pulls approved actions for its
// cluster, executes them with the narrow remediate ServiceAccount, and reports
// the outcome. The control plane never contacts a cluster itself.

// RemediationTokenResponse returns the one-time remediation token (operator API).
type RemediationTokenResponse struct {
	ClusterID        string `json:"clusterId"`
	RemediationToken string `json:"remediationToken"`
}

// ActionTarget identifies the object an action operates on.
type ActionTarget struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// PendingAction is an approved action handed to the controller for execution.
// DryRun is resolved by the control plane from policy (mode=dry-run => true).
type PendingAction struct {
	ProposalID     string            `json:"proposalId"`
	Action         string            `json:"action"`
	Target         ActionTarget      `json:"target"`
	Params         map[string]string `json:"params,omitempty"`
	DryRun         bool              `json:"dryRun"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
}

// PendingActionsResponse is the controller's work queue for its cluster.
type PendingActionsResponse struct {
	Actions []PendingAction `json:"actions"`
}

// ActionResult is what the controller reports back after executing (or
// dry-running) an action.
type ActionResult struct {
	Success            bool      `json:"success"`
	DryRun             bool      `json:"dryRun"`
	BeforeState        string    `json:"beforeState"`
	PlannedChange      string    `json:"plannedChange"`
	Result             string    `json:"result"`
	VerificationPassed *bool     `json:"verificationPassed,omitempty"`
	RollbackAvailable  bool      `json:"rollbackAvailable"`
	Error              string    `json:"error,omitempty"`
	StartedAt          time.Time `json:"startedAt"`
	FinishedAt         time.Time `json:"finishedAt"`
}
