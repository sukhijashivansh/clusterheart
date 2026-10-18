// Package agent is the in-cluster collector. It reads the cluster with a
// read-only ServiceAccount and forwards bounded health snapshots to the control
// plane. It never mutates the cluster and never acts on instructions found in
// telemetry.
package agent

import (
	"context"
	"sort"
	"strings"
	"time"

	"clusterheart/sre/internal/contract"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

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
	for i := range pods.Items {
		p := &pods.Items[i]
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
	}
	if len(snap.PodProblems) > maxProblems {
		snap.PodProblems = snap.PodProblems[:maxProblems]
	}

	snap.Workloads = c.workloadCounts(ctx)
	snap.WorkloadProblems = c.deploymentProblems(ctx)
	snap.Warnings = c.warnings(ctx)
	return snap, nil
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
