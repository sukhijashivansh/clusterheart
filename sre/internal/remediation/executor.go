// Package remediation is the in-cluster executor. It applies ONLY the typed,
// already-policy-approved actions handed to it by the control plane, using the
// narrow remediate ServiceAccount. It contains no detection, no AI, and no
// ability to run arbitrary commands - just a small set of registered actions.
package remediation

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"clusterheart/sre/internal/contract"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// restartAnnotation is the standard annotation `kubectl rollout restart` sets to
// trigger a rolling restart without changing the workload spec.
const restartAnnotation = "kubectl.kubernetes.io/restartedAt"

// Executor applies approved actions with client-go.
type Executor struct {
	client         kubernetes.Interface
	log            *slog.Logger
	now            func() time.Time
	verifyInterval time.Duration
}

// NewExecutor builds an executor. Empty kubeconfigPath => in-cluster config.
func NewExecutor(kubeconfigPath string, log *slog.Logger) (*Executor, error) {
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
	return NewExecutorWithClient(cs, log), nil
}

// NewExecutorWithClient is used in tests (with a fake clientset).
func NewExecutorWithClient(c kubernetes.Interface, log *slog.Logger) *Executor {
	if log == nil {
		log = slog.Default()
	}
	return &Executor{client: c, log: log, now: time.Now, verifyInterval: 2 * time.Second}
}

// Execute runs one approved action and returns a structured result.
func (e *Executor) Execute(ctx context.Context, a contract.PendingAction) contract.ActionResult {
	start := e.now()
	switch a.Action {
	case "RolloutRestart":
		return e.rolloutRestart(ctx, a, start)
	case "Scale":
		return e.scale(ctx, a, start)
	case "SetResources":
		return e.setResources(ctx, a, start)
	default:
		// Defensive: the control plane only ever sends registered actions.
		return contract.ActionResult{
			StartedAt: start, FinishedAt: e.now(), DryRun: a.DryRun, Success: false,
			Error: "unsupported action: " + a.Action,
		}
	}
}

func (e *Executor) rolloutRestart(ctx context.Context, a contract.PendingAction, start time.Time) contract.ActionResult {
	ns, name := a.Target.Namespace, a.Target.Name
	ts := e.now().UTC().Format(time.RFC3339)
	planned := fmt.Sprintf("set annotation %s=%s on %s/%s in %s", restartAnnotation, ts, a.Target.Kind, name, ns)
	fail := func(msg string) contract.ActionResult {
		return contract.ActionResult{
			StartedAt: start, FinishedAt: e.now(), DryRun: a.DryRun, Success: false,
			PlannedChange: planned, RollbackAvailable: true, Error: msg,
		}
	}
	opts := metav1.UpdateOptions{}
	if a.DryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}

	var before string
	var verify *bool

	switch a.Target.Kind {
	case "Deployment":
		d, err := e.client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			return fail("deployment not found")
		}
		if err != nil {
			return fail(err.Error())
		}
		before = fmt.Sprintf("resourceVersion=%s, replicas=%d, restartedAt=%q",
			d.ResourceVersion, deref(d.Spec.Replicas), d.Spec.Template.Annotations[restartAnnotation])
		setRestart(&d.Spec.Template, ts)
		// Update carries the resourceVersion, so a concurrent change => Conflict
		// (optimistic concurrency).
		if _, err := e.client.AppsV1().Deployments(ns).Update(ctx, d, opts); err != nil {
			if k8serrors.IsConflict(err) {
				return fail("resource changed since read (optimistic concurrency); retry")
			}
			return fail(err.Error())
		}
		if !a.DryRun {
			v := e.verifyDeployment(ctx, ns, name, timeout(a))
			verify = &v
			if !v {
				r := fail("rollout did not become healthy within timeout")
				r.BeforeState, r.VerificationPassed = before, &v
				return r
			}
		}
	case "StatefulSet":
		ss, err := e.client.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			return fail("statefulset not found")
		}
		if err != nil {
			return fail(err.Error())
		}
		before = fmt.Sprintf("resourceVersion=%s", ss.ResourceVersion)
		setRestart(&ss.Spec.Template, ts)
		if _, err := e.client.AppsV1().StatefulSets(ns).Update(ctx, ss, opts); err != nil {
			if k8serrors.IsConflict(err) {
				return fail("resource changed since read; retry")
			}
			return fail(err.Error())
		}
	case "DaemonSet":
		ds, err := e.client.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			return fail("daemonset not found")
		}
		if err != nil {
			return fail(err.Error())
		}
		before = fmt.Sprintf("resourceVersion=%s", ds.ResourceVersion)
		setRestart(&ds.Spec.Template, ts)
		if _, err := e.client.AppsV1().DaemonSets(ns).Update(ctx, ds, opts); err != nil {
			if k8serrors.IsConflict(err) {
				return fail("resource changed since read; retry")
			}
			return fail(err.Error())
		}
	default:
		return fail("RolloutRestart supports Deployment/StatefulSet/DaemonSet, not " + a.Target.Kind)
	}

	result := "applied: rolling restart triggered"
	if a.DryRun {
		result = "dry-run: validated against the API server; no changes persisted"
	}
	return contract.ActionResult{
		StartedAt: start, FinishedAt: e.now(), DryRun: a.DryRun, Success: true,
		PlannedChange: planned, BeforeState: before, Result: result,
		RollbackAvailable: true, VerificationPassed: verify,
	}
}

// scale sets spec.replicas on a Deployment/StatefulSet to the requested count,
// capturing the previous count as before-state (so the change is reversible),
// with optimistic concurrency and (for Deployments) post-scale verification.
func (e *Executor) scale(ctx context.Context, a contract.PendingAction, start time.Time) contract.ActionResult {
	ns, name := a.Target.Namespace, a.Target.Name
	want, err := strconv.Atoi(a.Params["replicas"])
	fail := func(msg string) contract.ActionResult {
		return contract.ActionResult{
			StartedAt: start, FinishedAt: e.now(), DryRun: a.DryRun, Success: false,
			PlannedChange:     fmt.Sprintf("scale %s/%s in %s to %s replicas", a.Target.Kind, name, ns, a.Params["replicas"]),
			RollbackAvailable: true, Error: msg,
		}
	}
	if err != nil || want < 0 {
		return fail("invalid 'replicas' parameter")
	}
	want32 := int32(want)
	planned := fmt.Sprintf("scale %s/%s in %s to %d replicas", a.Target.Kind, name, ns, want32)
	opts := metav1.UpdateOptions{}
	if a.DryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	var before string
	var verify *bool

	switch a.Target.Kind {
	case "Deployment":
		d, gerr := e.client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if k8serrors.IsNotFound(gerr) {
			return fail("deployment not found")
		}
		if gerr != nil {
			return fail(gerr.Error())
		}
		before = fmt.Sprintf("resourceVersion=%s, replicas=%d", d.ResourceVersion, deref(d.Spec.Replicas))
		d.Spec.Replicas = &want32
		if _, uerr := e.client.AppsV1().Deployments(ns).Update(ctx, d, opts); uerr != nil {
			if k8serrors.IsConflict(uerr) {
				return fail("resource changed since read (optimistic concurrency); retry")
			}
			return fail(uerr.Error())
		}
		if !a.DryRun {
			v := e.verifyDeployment(ctx, ns, name, timeout(a))
			verify = &v
			if !v {
				r := fail("scaled workload did not become healthy within timeout")
				r.BeforeState, r.VerificationPassed = before, &v
				return r
			}
		}
	case "StatefulSet":
		ss, gerr := e.client.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if k8serrors.IsNotFound(gerr) {
			return fail("statefulset not found")
		}
		if gerr != nil {
			return fail(gerr.Error())
		}
		before = fmt.Sprintf("resourceVersion=%s, replicas=%d", ss.ResourceVersion, deref(ss.Spec.Replicas))
		ss.Spec.Replicas = &want32
		if _, uerr := e.client.AppsV1().StatefulSets(ns).Update(ctx, ss, opts); uerr != nil {
			if k8serrors.IsConflict(uerr) {
				return fail("resource changed since read; retry")
			}
			return fail(uerr.Error())
		}
	default:
		return fail("Scale supports Deployment/StatefulSet, not " + a.Target.Kind)
	}

	result := fmt.Sprintf("applied: scaled to %d replicas", want32)
	if a.DryRun {
		result = "dry-run: validated against the API server; no changes persisted"
	}
	return contract.ActionResult{
		StartedAt: start, FinishedAt: e.now(), DryRun: a.DryRun, Success: true,
		PlannedChange: planned, BeforeState: before, Result: result,
		RollbackAvailable: true, VerificationPassed: verify,
	}
}

// verifyDeployment polls until the rollout is complete (observedGeneration caught
// up, all replicas updated and available) or the timeout elapses.
func (e *Executor) verifyDeployment(ctx context.Context, ns, name string, to time.Duration) bool {
	deadline := e.now().Add(to)
	for {
		d, err := e.client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil && deploymentHealthy(d) {
			return true
		}
		if e.now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(e.verifyInterval):
		}
	}
}

func deploymentHealthy(d *appsv1.Deployment) bool {
	desired := deref(d.Spec.Replicas)
	return d.Status.ObservedGeneration >= d.Generation &&
		d.Status.UpdatedReplicas == desired &&
		d.Status.AvailableReplicas == desired
}

func setRestart(t *corev1.PodTemplateSpec, ts string) {
	if t.Annotations == nil {
		t.Annotations = map[string]string{}
	}
	t.Annotations[restartAnnotation] = ts
}

func deref(p *int32) int32 {
	if p == nil {
		return 1
	}
	return *p
}

func timeout(a contract.PendingAction) time.Duration {
	if a.TimeoutSeconds <= 0 {
		return 120 * time.Second
	}
	return time.Duration(a.TimeoutSeconds) * time.Second
}
