// Package agent is the in-cluster collector. It reads the cluster with a
// read-only ServiceAccount and forwards bounded health snapshots to the control
// plane. It never mutates the cluster and never acts on instructions found in
// telemetry.
package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"clusterheart/sre/internal/contract"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

// maxPodInventory bounds how many individual pods a snapshot carries, so the
// control-plane store stays small even on large clusters. Extra pods are
// dropped and counted in PodListTruncated.
const maxPodInventory = 300

// maxNodeInventory bounds how many node rows a snapshot carries.
const maxNodeInventory = 100

// maxJobInventory bounds how many Job rows a snapshot carries.
const maxJobInventory = 200

// maxWarnings bounds how many warning events a snapshot carries.
const maxWarnings = 50

// highRestartThreshold is the per-pod restart count above which a pod is counted
// as "restart pressure". This is a symptom signal only, not a diagnosis.
const highRestartThreshold = 5

// maxProblems bounds how many fault signals a snapshot carries.
const maxProblems = 200

// Collector reads cluster state.
type Collector struct {
	clientset kubernetes.Interface
	metrics   metricsv.Interface // nil if metrics-server is not installed
	k8sVer    string
}

// NewCollector builds a collector. If kubeconfigPath is non-empty it is used;
// otherwise in-cluster config is used (the normal in-agent path).
func NewCollector(kubeconfigPath string) (*Collector, error) {
	var cfg *rest.Config
	var err error
	if kubeconfigPath != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	c := &Collector{clientset: cs}
	// Metrics are best-effort: if metrics-server isn't installed, CPU/mem are
	// simply omitted - the agent must never fail because metrics are missing.
	if mc, err := metricsv.NewForConfig(cfg); err == nil {
		c.metrics = mc
	}
	if v, err := cs.Discovery().ServerVersion(); err == nil {
		c.k8sVer = v.GitVersion
	}
	return c, nil
}

// K8sVersion returns the discovered server version (may be empty).
func (c *Collector) K8sVersion() string { return c.k8sVer }

// Snapshot collects one bounded cluster-health summary.
func (c *Collector) Snapshot(ctx context.Context) (contract.ClusterSnapshot, error) {
	snap := contract.ClusterSnapshot{CollectedAt: time.Now().UTC()}

	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return snap, err
	}
	snap.Nodes.Total = len(nodes.Items)
	for i := range nodes.Items {
		if nodeReady(&nodes.Items[i]) {
			snap.Nodes.Ready++
		} else {
			snap.Nodes.NotReady++
		}
	}

	pods, err := c.clientset.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return snap, err
	}
	snap.Pods.Total = len(pods.Items)
	podsPerNode := make(map[string]int, len(nodes.Items))
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != "" && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			podsPerNode[p.Spec.NodeName]++
		}
		switch p.Status.Phase {
		case corev1.PodRunning:
			snap.Pods.Running++
		case corev1.PodPending:
			snap.Pods.Pending++
		case corev1.PodSucceeded:
			snap.Pods.Succeeded++
		case corev1.PodFailed:
			snap.Pods.Failed++
		}
		if podRestarts(p) >= highRestartThreshold {
			snap.Pods.HighRestart++
		}
		if len(snap.PodProblems) < maxProblems {
			snap.PodProblems = append(snap.PodProblems, detectPodProblems(p)...)
		}
		if len(snap.PodList) < maxPodInventory {
			snap.PodList = append(snap.PodList, podInfo(p))
		} else {
			snap.PodListTruncated++
		}
	}
	if len(snap.PodProblems) > maxProblems {
		snap.PodProblems = snap.PodProblems[:maxProblems]
	}

	for i := range nodes.Items {
		if len(snap.NodeList) >= maxNodeInventory {
			break
		}
		snap.NodeList = append(snap.NodeList, nodeInfo(&nodes.Items[i], podsPerNode[nodes.Items[i].Name]))
	}

	snap.Workloads = c.workloadCounts(ctx)
	snap.WorkloadProblems = c.deploymentProblems(ctx)
	snap.Warnings = c.warnings(ctx)
	snap.JobList, snap.JobListTruncated = c.jobList(ctx)
	c.mergePodMetrics(ctx, snap.PodList)
	c.mergeNodeMetrics(ctx, snap.NodeList)
	return snap, nil
}

// jobList reads batch Jobs and their status directly from the Job objects (not
// their pods, which get garbage-collected), so failed and complete runs stay
// visible. Most recent first, so the latest runs survive the cap.
func (c *Collector) jobList(ctx context.Context) ([]contract.JobInfo, int) {
	jl, err := c.clientset.BatchV1().Jobs(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, 0
	}
	items := jl.Items
	sort.Slice(items, func(i, j int) bool { return jobStart(&items[i]).After(jobStart(&items[j])) })
	var out []contract.JobInfo
	trunc := 0
	for i := range items {
		if len(out) >= maxJobInventory {
			trunc++
			continue
		}
		out = append(out, jobInfo(&items[i]))
	}
	return out, trunc
}

func jobInfo(j *batchv1.Job) contract.JobInfo {
	ji := contract.JobInfo{
		Namespace: j.Namespace,
		Name:      j.Name,
		Owner:     controllerRef(j.OwnerReferences),
		Failed:    j.Status.Failed,
		Status:    "Pending",
	}
	comp := int32(1)
	if j.Spec.Completions != nil {
		comp = *j.Spec.Completions
	}
	ji.Completions = fmt.Sprintf("%d/%d", j.Status.Succeeded, comp)
	for _, cond := range j.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete:
			ji.Status = "Complete"
		case batchv1.JobFailed:
			ji.Status = "Failed"
		}
	}
	if ji.Status == "Pending" && j.Status.Active > 0 {
		ji.Status = "Running"
	}
	if j.Status.StartTime != nil {
		t := j.Status.StartTime.Time.UTC()
		ji.StartedAt = &t
	}
	if j.Status.CompletionTime != nil {
		t := j.Status.CompletionTime.Time.UTC()
		ji.FinishedAt = &t
	}
	return ji
}

func jobStart(j *batchv1.Job) time.Time {
	if j.Status.StartTime != nil {
		return j.Status.StartTime.Time
	}
	return j.CreationTimestamp.Time
}

// controllerRef returns the owning controller as "Kind/Name" (e.g. the CronJob).
func controllerRef(refs []metav1.OwnerReference) string {
	for _, r := range refs {
		if r.Controller != nil && *r.Controller {
			return r.Kind + "/" + r.Name
		}
	}
	if len(refs) > 0 {
		return refs[0].Kind + "/" + refs[0].Name
	}
	return ""
}

// nodeInfo builds one node row from its object (capacity/allocatable); usage is
// filled later by mergeNodeMetrics when metrics-server is present.
func nodeInfo(n *corev1.Node, pods int) contract.NodeInfo {
	ni := contract.NodeInfo{
		Name:           n.Name,
		Ready:          nodeReady(n),
		Roles:          nodeRoles(n),
		KubeletVersion: n.Status.NodeInfo.KubeletVersion,
		Pods:           pods,
	}
	alloc := n.Status.Allocatable
	if q, ok := alloc[corev1.ResourceCPU]; ok {
		ni.CPUAllocMilli = q.MilliValue()
	}
	if q, ok := alloc[corev1.ResourceMemory]; ok {
		ni.MemAllocMi = q.Value() / (1024 * 1024)
	}
	if q, ok := alloc[corev1.ResourcePods]; ok {
		ni.PodsCapacity = int(q.Value())
	}
	return ni
}

// nodeRoles reads the node-role.kubernetes.io/* labels; defaults to "worker".
func nodeRoles(n *corev1.Node) string {
	var roles []string
	for k := range n.Labels {
		if r := strings.TrimPrefix(k, "node-role.kubernetes.io/"); r != k && r != "" {
			roles = append(roles, r)
		}
	}
	sort.Strings(roles)
	if len(roles) == 0 {
		return "worker"
	}
	return strings.Join(roles, ",")
}

// mergeNodeMetrics fills live CPU/memory usage from metrics-server, by node
// name. Best-effort: if metrics are unavailable the fields stay zero.
func (c *Collector) mergeNodeMetrics(ctx context.Context, list []contract.NodeInfo) {
	if c.metrics == nil || len(list) == 0 {
		return
	}
	nm, err := c.metrics.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	type usage struct{ cpu, mem int64 }
	byName := make(map[string]usage, len(nm.Items))
	for i := range nm.Items {
		m := &nm.Items[i]
		cpu := m.Usage.Cpu().MilliValue()
		mem := m.Usage.Memory().Value() / (1024 * 1024)
		byName[m.Name] = usage{cpu: cpu, mem: mem}
	}
	for i := range list {
		if u, ok := byName[list[i].Name]; ok {
			list[i].CPUUsageMilli = u.cpu
			list[i].MemUsageMi = u.mem
		}
	}
}

// podInfo builds one inventory row from a pod's status (no metrics yet).
func podInfo(p *corev1.Pod) contract.PodInfo {
	total := len(p.Spec.Containers)
	ready := 0
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
	}
	return contract.PodInfo{
		Namespace: p.Namespace,
		Name:      p.Name,
		Phase:     string(p.Status.Phase),
		Ready:     fmt.Sprintf("%d/%d", ready, total),
		Restarts:  int32(podRestarts(p)),
		Node:      p.Spec.NodeName,
		Owner:     ownerWorkload(p),
	}
}

// mergePodMetrics fills CPU/memory from metrics-server into the inventory, by
// ns/name. Best-effort: if metrics are unavailable the fields stay zero.
func (c *Collector) mergePodMetrics(ctx context.Context, list []contract.PodInfo) {
	if c.metrics == nil || len(list) == 0 {
		return
	}
	pm, err := c.metrics.MetricsV1beta1().PodMetricses(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	type usage struct{ cpu, mem int64 }
	byKey := make(map[string]usage, len(pm.Items))
	for i := range pm.Items {
		m := &pm.Items[i]
		var cpu, mem int64
		for j := range m.Containers {
			cpu += m.Containers[j].Usage.Cpu().MilliValue()
			mem += m.Containers[j].Usage.Memory().Value()
		}
		byKey[m.Namespace+"/"+m.Name] = usage{cpu: cpu, mem: mem / (1024 * 1024)}
	}
	for i := range list {
		if u, ok := byKey[list[i].Namespace+"/"+list[i].Name]; ok {
			list[i].CPUMilli = u.cpu
			list[i].MemMi = u.mem
		}
	}
}

// detectPodProblems derives measured fault signals from a pod's status. These
// are symptoms (CrashLoopBackOff, OOMKilled, ImagePullBackOff, Unschedulable),
// not diagnoses.
func detectPodProblems(p *corev1.Pod) []contract.PodProblem {
	owner := ownerWorkload(p)
	node := p.Spec.NodeName
	var out []contract.PodProblem
	add := func(t contract.ProblemType, container, msg string, restarts int32) {
		out = append(out, contract.PodProblem{
			Namespace: p.Namespace, Pod: p.Name, Owner: owner, Container: container,
			Node: node, Type: t, RestartCount: restarts, Message: Redact(msg),
		})
	}

	for _, cs := range p.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "CrashLoopBackOff":
				add(contract.ProblemCrashLoopBackOff, cs.Name, w.Message, cs.RestartCount)
			case "ImagePullBackOff", "ErrImagePull":
				add(contract.ProblemImagePullBackOff, cs.Name, w.Message, cs.RestartCount)
			}
		}
		if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
			add(contract.ProblemOOMKilled, cs.Name, t.Message, cs.RestartCount)
		}
	}

	if p.Status.Phase == corev1.PodPending {
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == "Unschedulable" {
				add(contract.ProblemUnschedulable, "", cond.Message, 0)
			}
		}
	}
	return out
}

// ownerWorkload returns a stable "Kind/Name" for the pod's controlling workload.
// ReplicaSet owners are collapsed to their Deployment (RS names are
// "<deployment>-<hash>") so incidents stay stable across rollouts.
func ownerWorkload(p *corev1.Pod) string {
	for _, o := range p.OwnerReferences {
		if o.Controller == nil || !*o.Controller {
			continue
		}
		if o.Kind == "ReplicaSet" {
			if i := strings.LastIndex(o.Name, "-"); i > 0 {
				return "Deployment/" + o.Name[:i]
			}
		}
		return o.Kind + "/" + o.Name
	}
	return ""
}

// deploymentProblems reports deployments whose rollout exceeded its progress
// deadline (Progressing=False, reason=ProgressDeadlineExceeded).
func (c *Collector) deploymentProblems(ctx context.Context) []contract.WorkloadProblem {
	ds, err := c.clientset.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var out []contract.WorkloadProblem
	for i := range ds.Items {
		d := &ds.Items[i]
		for _, cond := range d.Status.Conditions {
			if cond.Type == appsv1.DeploymentProgressing && cond.Status == corev1.ConditionFalse && cond.Reason == "ProgressDeadlineExceeded" {
				out = append(out, contract.WorkloadProblem{
					Namespace: d.Namespace, Kind: "Deployment", Name: d.Name,
					Type: contract.ProblemRolloutStuck, Message: Redact(cond.Message),
				})
			}
		}
	}
	return out
}

func (c *Collector) workloadCounts(ctx context.Context) contract.WorkloadSummary {
	var w contract.WorkloadSummary
	if d, err := c.clientset.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{}); err == nil {
		w.Deployments = len(d.Items)
	}
	if ss, err := c.clientset.AppsV1().StatefulSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{}); err == nil {
		w.StatefulSets = len(ss.Items)
	}
	if ds, err := c.clientset.AppsV1().DaemonSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{}); err == nil {
		w.DaemonSets = len(ds.Items)
	}
	if j, err := c.clientset.BatchV1().Jobs(metav1.NamespaceAll).List(ctx, metav1.ListOptions{}); err == nil {
		w.Jobs = len(j.Items)
	}
	return w
}

func (c *Collector) warnings(ctx context.Context) []contract.WarningEvent {
	evs, err := c.clientset.CoreV1().Events(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "type=Warning",
	})
	if err != nil {
		return nil
	}
	items := evs.Items
	// Most recent first.
	sort.Slice(items, func(i, j int) bool {
		return eventTime(&items[i]).After(eventTime(&items[j]))
	})
	if len(items) > maxWarnings {
		items = items[:maxWarnings]
	}
	out := make([]contract.WarningEvent, 0, len(items))
	for i := range items {
		e := &items[i]
		out = append(out, contract.WarningEvent{
			Namespace: e.Namespace,
			Reason:    e.Reason,
			Object:    e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			Message:   Redact(e.Message), // telemetry is untrusted + may hold secrets
			Count:     e.Count,
			LastSeen:  eventTime(e),
		})
	}
	return out
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podRestarts(p *corev1.Pod) int {
	var total int
	for _, cs := range p.Status.ContainerStatuses {
		total += int(cs.RestartCount)
	}
	return total
}

func eventTime(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}
