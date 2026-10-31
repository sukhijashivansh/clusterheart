package remediation

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"clusterheart/sre/internal/contract"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// setResources sets the CPU (millicores) and/or memory (MiB) request of a
// single-container workload. Multi-container workloads are refused: one
// pod-level number can't be split between containers safely. The previous
// requests are reported as before-state so the change can be reverted.
func (e *Executor) setResources(ctx context.Context, a contract.PendingAction, start time.Time) contract.ActionResult {
	ns, name, kind := a.Target.Namespace, a.Target.Name, a.Target.Kind
	var want []string
	if v := a.Params["cpu"]; v != "" {
		want = append(want, "cpu="+v+"m")
	}
	if v := a.Params["memory"]; v != "" {
		want = append(want, "memory="+v+"Mi")
	}
	planned := fmt.Sprintf("set requests %s on %s/%s in %s", strings.Join(want, ","), kind, name, ns)
	fail := func(msg string) contract.ActionResult {
		return contract.ActionResult{
			StartedAt: start, FinishedAt: e.now(), DryRun: a.DryRun, Success: false,
			PlannedChange: planned, RollbackAvailable: true, Error: msg,
		}
	}
	cpu, mem, err := resourceParams(a.Params)
	if err != nil {
		return fail(err.Error())
	}
	opts := metav1.UpdateOptions{}
	if a.DryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}

	var before string
	var verify *bool
	apps := e.client.AppsV1()
	switch kind {
	case "Deployment":
		d, gerr := apps.Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return fail(getErr(gerr, "deployment"))
		}
		if before, err = applyRequests(&d.Spec.Template, cpu, mem); err != nil {
			return fail(err.Error())
		}
		if _, err = apps.Deployments(ns).Update(ctx, d, opts); err != nil {
			return fail(updateErr(err))
		}
		if !a.DryRun {
			v := e.verifyDeployment(ctx, ns, name, timeout(a))
			verify = &v
			if !v {
				r := fail("new pods did not become healthy within timeout")
				r.BeforeState, r.VerificationPassed = before, &v
				return r
			}
		}
	case "StatefulSet":
		ss, gerr := apps.StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return fail(getErr(gerr, "statefulset"))
		}
		if before, err = applyRequests(&ss.Spec.Template, cpu, mem); err != nil {
			return fail(err.Error())
		}
		if _, err = apps.StatefulSets(ns).Update(ctx, ss, opts); err != nil {
			return fail(updateErr(err))
		}
	case "DaemonSet":
		ds, gerr := apps.DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return fail(getErr(gerr, "daemonset"))
		}
		if before, err = applyRequests(&ds.Spec.Template, cpu, mem); err != nil {
			return fail(err.Error())
		}
		if _, err = apps.DaemonSets(ns).Update(ctx, ds, opts); err != nil {
			return fail(updateErr(err))
		}
	default:
		return fail("SetResources supports Deployment/StatefulSet/DaemonSet, not " + kind)
	}

	result := "applied: requests " + strings.Join(want, ",")
	if a.DryRun {
		result = "dry-run: validated against the API server; no changes persisted"
	}
	return contract.ActionResult{
		StartedAt: start, FinishedAt: e.now(), DryRun: a.DryRun, Success: true,
		PlannedChange: planned, BeforeState: before, Result: result,
		RollbackAvailable: true, VerificationPassed: verify,
	}
}

// resourceParams parses the cpu (millicores) and memory (MiB) params; a missing
// param is returned as nil and left unchanged.
func resourceParams(p map[string]string) (cpu, mem *resource.Quantity, err error) {
	if v := p["cpu"]; v != "" {
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || n <= 0 {
			return nil, nil, fmt.Errorf("invalid 'cpu' parameter")
		}
		cpu = resource.NewMilliQuantity(n, resource.DecimalSI)
	}
	if v := p["memory"]; v != "" {
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || n <= 0 {
			return nil, nil, fmt.Errorf("invalid 'memory' parameter")
		}
		mem = resource.NewQuantity(n*1024*1024, resource.BinarySI)
	}
	if cpu == nil && mem == nil {
		return nil, nil, fmt.Errorf("nothing to set: 'cpu' and/or 'memory' required")
	}
	return cpu, mem, nil
}

// applyRequests sets the requests on the template's only container and returns
// the previous values. It refuses multi-container pods and requests above an
// existing limit (the API server would reject those).
func applyRequests(t *corev1.PodTemplateSpec, cpu, mem *resource.Quantity) (string, error) {
	if n := len(t.Spec.Containers); n != 1 {
		return "", fmt.Errorf("Apply supports single-container workloads only; this one has %d containers. Use the kubectl command with -c <container> instead", n)
	}
	c := &t.Spec.Containers[0]
	if c.Resources.Requests == nil {
		c.Resources.Requests = corev1.ResourceList{}
	}
	var before []string
	set := func(name corev1.ResourceName, q *resource.Quantity) error {
		if q == nil {
			return nil
		}
		old := "unset"
		if cur, ok := c.Resources.Requests[name]; ok {
			old = cur.String()
		}
		if lim, ok := c.Resources.Limits[name]; ok && q.Cmp(lim) > 0 {
			return fmt.Errorf("new %s request %s is above the container's limit %s; raise the limit first", name, q.String(), lim.String())
		}
		before = append(before, fmt.Sprintf("%s request=%s", name, old))
		c.Resources.Requests[name] = *q
		return nil
	}
	if err := set(corev1.ResourceCPU, cpu); err != nil {
		return "", err
	}
	if err := set(corev1.ResourceMemory, mem); err != nil {
		return "", err
	}
	return "container=" + c.Name + ", " + strings.Join(before, ", "), nil
}

func getErr(err error, kind string) string {
	if k8serrors.IsNotFound(err) {
		return kind + " not found"
	}
	return err.Error()
}

func updateErr(err error) string {
	if k8serrors.IsConflict(err) {
		return "resource changed since read (optimistic concurrency); retry"
	}
	return err.Error()
}
