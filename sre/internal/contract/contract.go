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
	// Kind is what's being watched: "kubernetes" (default) or "server" (a
	// single Linux host running the host agent).
	Kind string `json:"kind,omitempty"`
}

// Kinds of monitored targets.
const (
	KindKubernetes = "kubernetes"
	KindServer     = "server"
)

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
	// NodeList is a bounded per-node capacity/utilization inventory so the
	// dashboard can show real node CPU/memory pressure.
	NodeList []NodeInfo `json:"nodeList,omitempty"`
	// JobList is a bounded inventory of batch Jobs with their real status so the
	// dashboard shows failed/complete runs even after their pods are GC'd.
	JobList          []JobInfo `json:"jobList,omitempty"`
	JobListTruncated int       `json:"jobListTruncated,omitempty"`
	// PodLogs are recent log tails from failing pods (crash-looping, OOMKilled or
	// failed), redacted by the agent. Bounded in count and size; only the newest
	// snapshot in a batch carries them.
	PodLogs []PodLog `json:"podLogs,omitempty"`
	// Host is set by the host agent (Kind "server") instead of the Kubernetes
	// inventory above.
	Host *HostSnapshot `json:"host,omitempty"`
	// SecretFindings are places where a credential appears to be exposed: in a
	// container's log output or hard-coded in a pod spec (env/args). They never
	// contain the secret itself, only where it is, what kind, and a redacted
	// line for context.
	SecretFindings []SecretFinding `json:"secretFindings,omitempty"`
}

// SecretFinding is one exposed-credential signal, keyed by workload (not pod) so
// replicas and restarts don't duplicate it.
type SecretFinding struct {
	Source    string    `json:"source"` // "log" | "env" | "args"
	Type      string    `json:"type"`   // detector name, e.g. aws-access-key, password-or-token, hardcoded-env
	Namespace string    `json:"namespace"`
	Owner     string    `json:"owner"` // e.g. Deployment/api, or Pod/<name>
	Pod       string    `json:"pod,omitempty"`
	Container string    `json:"container"`
	Field     string    `json:"field,omitempty"`   // env var name or "args"
	Key       string    `json:"key,omitempty"`     // field the secret was under, e.g. accessToken
	Masked    string    `json:"masked,omitempty"`  // recognisable hint, e.g. "eyJh…x9Qz" (never the value)
	Length    int       `json:"length,omitempty"`  // length of the secret value
	Context   string    `json:"context,omitempty"` // redacted log line / snippet, centred on the leak
	Count     int       `json:"count"`             // log lines seen with it (1 for specs)
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// Ref identifies a finding stably: the same workload, container, field, type
// and key give the same ref.
func (f SecretFinding) Ref() string {
	return f.Source + "|" + f.Namespace + "|" + f.Owner + "|" + f.Container + "|" + f.Field + "|" + f.Type + "|" + f.Key
}

// RevealRequestsResponse answers agents up to 0.2.2 / host-0.2.1, which still
// poll GET /api/v1/agents/reveals. Revealing secret values was removed, so the
// list is always empty; newer agents do not poll.
type RevealRequestsResponse struct {
	Requests []struct{} `json:"requests"`
}

// ProbeTarget is a pod port to time HTTP GET requests against (latency check).
type ProbeTarget struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Port      int    `json:"port,omitempty"` // 0: the pod's first declared TCP port
	Scheme    string `json:"scheme"`         // http | https
	Path      string `json:"path"`
	Samples   int    `json:"samples"`
}

// ProbeRequest is one latency check a person started in the dashboard.
type ProbeRequest struct {
	ID     string      `json:"id"`
	Target ProbeTarget `json:"target"`
}

// ProbeRequestsResponse is the agent's queue of latency checks to run.
type ProbeRequestsResponse struct {
	Requests []ProbeRequest `json:"requests"`
}

// ProbeSample is one timed request, split into the phases curl -w reports.
// The agent reaches the pod IP directly, so there is no DNS phase.
type ProbeSample struct {
	ConnectMs  float64 `json:"connectMs"`  // TCP handshake
	TLSms      float64 `json:"tlsMs"`      // TLS handshake (0 for http)
	WaitMs     float64 `json:"waitMs"`     // request sent -> first byte
	TransferMs float64 `json:"transferMs"` // first byte -> last byte
	FirstByte  float64 `json:"firstByteMs"`
	TotalMs    float64 `json:"totalMs"`
	Status     int     `json:"status,omitempty"`
	Bytes      int64   `json:"bytes"`
	Err        string  `json:"err,omitempty"`
}

// ProbeStat is min/avg/max of one phase across the successful samples.
type ProbeStat struct {
	Min float64 `json:"min"`
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

// ProbeResult is the agent's answer to a latency check. Response bodies are
// never returned, only their size.
type ProbeResult struct {
	URL       string        `json:"url,omitempty"` // what was requested, e.g. https://10.0.3.7:8443/health
	Port      int           `json:"port,omitempty"`
	OKCount   int           `json:"okCount"`
	Count     int           `json:"count"`
	Samples   []ProbeSample `json:"samples,omitempty"`
	Connect   ProbeStat     `json:"connect"`
	TLS       ProbeStat     `json:"tls"`
	Wait      ProbeStat     `json:"wait"`
	Transfer  ProbeStat     `json:"transfer"`
	FirstByte ProbeStat     `json:"firstByte"`
	Total     ProbeStat     `json:"total"`
	Error     string        `json:"error,omitempty"` // the check could not run at all
}

// PodLog is the tail of one container's log. Previous is true when the lines
// come from the last terminated instance (what a crash looked like).
type PodLog struct {
	Namespace   string    `json:"namespace"`
	Pod         string    `json:"pod"`
	Container   string    `json:"container"`
	Previous    bool      `json:"previous,omitempty"`
	Reason      string    `json:"reason,omitempty"` // why it was collected, e.g. CrashLoopBackOff
	Lines       []string  `json:"lines"`
	CollectedAt time.Time `json:"collectedAt"`
}

// JobInfo is one batch Job's status, read from the Job object (not its pods, which
// Kubernetes garbage-collects). Status is Complete, Failed, Running, or Pending.
type JobInfo struct {
	Namespace   string     `json:"namespace"`
	Name        string     `json:"name"`
	Owner       string     `json:"owner,omitempty"`       // e.g. "CronJob/nightly"
	Status      string     `json:"status"`                // Complete | Failed | Running | Pending
	Completions string     `json:"completions,omitempty"` // "1/1"
	Failed      int32      `json:"failed,omitempty"`      // failed pod attempts
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
}

// NodeInfo is one node's capacity and (when metrics-server is present) live
// utilization. Usage fields are omitted when metrics are unavailable;
// allocatable/capacity always come from the node object. CPU is millicores,
// memory is mebibytes.
type NodeInfo struct {
	Name           string `json:"name"`
	Ready          bool   `json:"ready"`
	Roles          string `json:"roles,omitempty"`          // e.g. "control-plane" or "worker"
	KubeletVersion string `json:"kubeletVersion,omitempty"` // e.g. "v1.29.4"
	Pods           int    `json:"pods"`                     // pods scheduled on this node
	PodsCapacity   int    `json:"podsCapacity,omitempty"`   // max schedulable pods
	CPUAllocMilli  int64  `json:"cpuAllocMilli,omitempty"`  // allocatable CPU (millicores)
	CPUUsageMilli  int64  `json:"cpuUsageMilli,omitempty"`  // live CPU usage (metrics-server)
	MemAllocMi     int64  `json:"memAllocMi,omitempty"`     // allocatable memory (Mi)
	MemUsageMi     int64  `json:"memUsageMi,omitempty"`     // live memory usage (metrics-server)
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
	// Requests/limits summed over the pod's containers (omitted when unset), for
	// rightsizing. CPU in millicores, memory in mebibytes.
	CPUReqMilli int64 `json:"cpuReqMilli,omitempty"`
	MemReqMi    int64 `json:"memReqMi,omitempty"`
	CPULimMilli int64 `json:"cpuLimMilli,omitempty"`
	MemLimMi    int64 `json:"memLimMi,omitempty"`
	// Peak usage the agent has observed for this pod over the last PeakWindowMins
	// (rolling, up to 6h; resets when the agent restarts).
	CPUPeakMilli   int64 `json:"cpuPeakMilli,omitempty"`
	MemPeakMi      int64 `json:"memPeakMi,omitempty"`
	PeakWindowMins int   `json:"peakWindowMins,omitempty"`
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
	Kind          string           `json:"kind,omitempty"` // "kubernetes" or "server"
	// RemediationEnabled reports that a remediation token has been issued, i.e.
	// the in-cluster remediation controller can apply approved actions.
	RemediationEnabled bool `json:"remediationEnabled,omitempty"`
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

// HostSnapshot is one Linux server's health, collected by the host agent.
// Sizes are MiB, CPU and memory percentages are 0-100.
type HostSnapshot struct {
	Hostname    string        `json:"hostname"`
	OS          string        `json:"os,omitempty"`     // e.g. "Ubuntu 24.04 LTS"
	Kernel      string        `json:"kernel,omitempty"` // e.g. "6.8.0-45-generic"
	Arch        string        `json:"arch,omitempty"`
	CPUCores    int           `json:"cpuCores"`
	CPUPercent  float64       `json:"cpuPercent"`
	Load1       float64       `json:"load1"`
	Load5       float64       `json:"load5"`
	Load15      float64       `json:"load15"`
	MemTotalMi  int64         `json:"memTotalMi"`
	MemUsedMi   int64         `json:"memUsedMi"`
	SwapTotalMi int64         `json:"swapTotalMi,omitempty"`
	SwapUsedMi  int64         `json:"swapUsedMi,omitempty"`
	UptimeSec   int64         `json:"uptimeSec"`
	NetRxKBps   float64       `json:"netRxKBps,omitempty"` // all physical interfaces, since the previous sample
	NetTxKBps   float64       `json:"netTxKBps,omitempty"`
	Disks       []HostDisk    `json:"disks,omitempty"`
	GPUs        []HostGPU     `json:"gpus,omitempty"`
	Processes   []HostProcess `json:"processes,omitempty"`   // top by CPU, then memory
	FailedUnits []HostUnit    `json:"failedUnits,omitempty"` // systemd units in failed state
	ErrorLogs   []HostLogLine `json:"errorLogs,omitempty"`   // recent error-level journal lines, redacted
	Problems    []HostProblem `json:"problems,omitempty"`
}

// HostGPU is one GPU, read from nvidia-smi (NVIDIA) or the amdgpu driver (AMD).
// Zero means "not reported" for temperature and power.
type HostGPU struct {
	Index       int     `json:"index"`
	Vendor      string  `json:"vendor"` // nvidia | amd
	Name        string  `json:"name"`
	UUID        string  `json:"uuid,omitempty"`
	UtilPercent float64 `json:"utilPercent"`
	MemUsedMi   int64   `json:"memUsedMi"`
	MemTotalMi  int64   `json:"memTotalMi"`
	TempC       float64 `json:"tempC,omitempty"`
	PowerW      float64 `json:"powerW,omitempty"`
	PowerLimitW float64 `json:"powerLimitW,omitempty"`
}

// HostDisk is one mounted filesystem.
type HostDisk struct {
	Mount   string `json:"mount"`
	FS      string `json:"fs"`
	TotalMi int64  `json:"totalMi"`
	UsedMi  int64  `json:"usedMi"`
}

// HostProcess is one process with its CPU (of one core) and resident memory.
type HostProcess struct {
	PID        int     `json:"pid"`
	Name       string  `json:"name"`
	User       string  `json:"user,omitempty"`
	CPUPercent float64 `json:"cpuPercent"`
	MemMi      int64   `json:"memMi"`
}

// HostUnit is a systemd unit.
type HostUnit struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// HostLogLine is one journal entry.
type HostLogLine struct {
	Time    time.Time `json:"time"`
	Unit    string    `json:"unit,omitempty"`
	Message string    `json:"message"`
}

// HostProblem is a measured issue on a host. Severity is "high" or "medium".
type HostProblem struct {
	Type     string `json:"type"` // DiskFull, MemoryPressure, HighLoad, FailedService, RecentReboot, SwapPressure, GPUHot, GPUUnavailable
	Severity string `json:"severity"`
	Message  string `json:"message"`
}
