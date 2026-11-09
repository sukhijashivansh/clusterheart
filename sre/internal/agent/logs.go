package agent

import (
	"bufio"
	"bytes"
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"clusterheart/sre/internal/contract"
)

// Log collection bounds: only failing pods, a short tail, redacted, and cached
// so the agent doesn't re-read the same logs every collection cycle.
const (
	maxLogPods     = 8
	logTailLines   = 50
	maxLogLineLen  = 300
	maxLogBytes    = 64 << 10 // across all pods in one snapshot
	logCacheTTL    = 2 * time.Minute
	logCallTimeout = 5 * time.Second
)

type logTarget struct {
	ns, pod, container, reason string
	previous                   bool
}

type cachedLog struct {
	log contract.PodLog
	at  time.Time
}

type logCache struct {
	mu sync.Mutex
	m  map[string]cachedLog
}

func newLogCache() *logCache { return &logCache{m: map[string]cachedLog{}} }

// logTargets picks which containers to read: crash-looping or OOMKilled
// containers first (their previous instance shows the crash), then containers
// of Failed pods (e.g. failed Jobs). ImagePullBackOff/Unschedulable pods have
// never run, so there is nothing to read.
func logTargets(probs []contract.PodProblem, pods []corev1.Pod) []logTarget {
	seen := map[string]bool{}
	var out []logTarget
	add := func(t logTarget) {
		k := t.ns + "/" + t.pod + "/" + t.container
		if seen[k] || t.container == "" || len(out) >= maxLogPods {
			return
		}
		seen[k] = true
		out = append(out, t)
	}
	for _, p := range probs {
		if p.Type == contract.ProblemCrashLoopBackOff || p.Type == contract.ProblemOOMKilled {
			add(logTarget{ns: p.Namespace, pod: p.Pod, container: p.Container, reason: string(p.Type), previous: p.RestartCount > 0})
		}
	}
	// Most recently started failed pods first.
	failed := make([]*corev1.Pod, 0)
	for i := range pods {
		if pods[i].Status.Phase == corev1.PodFailed {
			failed = append(failed, &pods[i])
		}
	}
	sort.Slice(failed, func(i, j int) bool {
		return failed[i].CreationTimestamp.After(failed[j].CreationTimestamp.Time)
	})
	for _, p := range failed {
		for _, c := range p.Spec.Containers {
			add(logTarget{ns: p.Namespace, pod: p.Name, container: c.Name, reason: "Failed"})
		}
	}
	return out
}

// podLogs reads (or reuses cached) log tails for the targets. Best effort: a
// container whose logs can't be read is skipped.
func (c *Collector) podLogs(ctx context.Context, targets []logTarget, now time.Time) []contract.PodLog {
	if c.logs == nil {
		c.logs = newLogCache()
	}
	c.logs.mu.Lock()
	defer c.logs.mu.Unlock()
	live := map[string]bool{}
	var out []contract.PodLog
	budget := maxLogBytes
	for _, t := range targets {
		key := t.ns + "/" + t.pod + "/" + t.container
		live[key] = true
		entry, ok := c.logs.m[key]
		if !ok || now.Sub(entry.at) > logCacheTTL {
			pl, err := c.readLog(ctx, t)
			if err != nil {
				continue
			}
			pl.CollectedAt = now
			entry = cachedLog{log: pl, at: now}
			c.logs.m[key] = entry
		}
		pl := entry.log
		size := 0
		for _, l := range pl.Lines {
			size += len(l)
		}
		if size > budget {
			continue
		}
		budget -= size
		out = append(out, pl)
	}
	for k := range c.logs.m { // forget pods that recovered or went away
		if !live[k] {
			delete(c.logs.m, k)
		}
	}
	return out
}

func (c *Collector) readLog(ctx context.Context, t logTarget) (contract.PodLog, error) {
	tail := int64(logTailLines)
	limit := int64(logTailLines * maxLogLineLen * 2)
	get := func(previous bool) ([]byte, error) {
		cctx, cancel := context.WithTimeout(ctx, logCallTimeout)
		defer cancel()
		return c.clientset.CoreV1().Pods(t.ns).GetLogs(t.pod, &corev1.PodLogOptions{
			Container: t.container, Previous: previous, TailLines: &tail, LimitBytes: &limit,
		}).DoRaw(cctx)
	}
	previous := t.previous
	raw, err := get(previous)
	if err != nil && previous { // no previous instance kept: fall back to the current one
		previous = false
		raw, err = get(false)
	}
	if err != nil {
		return contract.PodLog{}, err
	}
	return contract.PodLog{
		Namespace: t.ns, Pod: t.pod, Container: t.container, Previous: previous,
		Reason: t.reason, Lines: cleanLogLines(raw),
	}, nil
}

// cleanLogLines splits, trims, truncates and redacts log output.
func cleanLogLines(raw []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := strings.TrimRight(sc.Text(), "\r ")
		if l == "" {
			continue
		}
		if len(l) > maxLogLineLen {
			l = l[:maxLogLineLen] + "…"
		}
		out = append(out, Redact(l))
	}
	if len(out) > logTailLines {
		out = out[len(out)-logTailLines:]
	}
	return out
}

// On-demand log reads (a person clicked a pod or job in the dashboard): a
// longer tail and longer lines than the failing-pod tails, still redacted.
const (
	viewMaxTail    = 500
	viewMaxLineLen = 2000
	viewTimeout    = 15 * time.Second
)

// jobPod finds a Job's newest pod. Kubernetes may already have cleaned them up.
func (c *Collector) jobPod(ctx context.Context, ns, job string) (*corev1.Pod, error) {
	for _, sel := range []string{"batch.kubernetes.io/job-name=" + job, "job-name=" + job} {
		list, err := c.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return nil, err
		}
		if len(list.Items) == 0 {
			continue
		}
		newest := &list.Items[0]
		for i := range list.Items {
			if list.Items[i].CreationTimestamp.After(newest.CreationTimestamp.Time) {
				newest = &list.Items[i]
			}
		}
		return newest, nil
	}
	return nil, nil
}

// ReadLog answers one on-demand log read.
func (c *Collector) ReadLog(ctx context.Context, t contract.LogTarget) contract.LogResult {
	var pod *corev1.Pod
	if t.Job != "" {
		p, err := c.jobPod(ctx, t.Namespace, t.Job)
		if err != nil {
			return contract.LogResult{Error: "could not list the job's pods"}
		}
		if p == nil {
			return contract.LogResult{Error: "This job's pods are gone (Kubernetes cleaned them up), so there is no log left to read."}
		}
		pod = p
	} else {
		p, err := c.clientset.CoreV1().Pods(t.Namespace).Get(ctx, t.Pod, metav1.GetOptions{})
		if err != nil {
			return contract.LogResult{Error: "Pod " + t.Namespace + "/" + t.Pod + " not found. It may have been replaced; refresh and pick it again."}
		}
		pod = p
	}
	res := contract.LogResult{Pod: pod.Name, Previous: t.Previous}
	for _, ct := range pod.Spec.InitContainers {
		res.Containers = append(res.Containers, ct.Name)
	}
	for _, ct := range pod.Spec.Containers {
		res.Containers = append(res.Containers, ct.Name)
	}
	res.Container = t.Container
	if res.Container == "" && len(pod.Spec.Containers) > 0 {
		res.Container = pod.Spec.Containers[0].Name
	}
	tail := int64(min(max(t.Tail, 1), viewMaxTail))
	limit := int64(viewMaxTail * viewMaxLineLen)
	raw, err := c.clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: res.Container, Previous: t.Previous, TailLines: &tail, LimitBytes: &limit,
	}).DoRaw(ctx)
	if err != nil {
		msg := "could not read the log"
		if t.Previous {
			msg = "No earlier instance of this container is kept, so there is no previous log."
		} else if pod.Status.Phase == corev1.PodPending {
			msg = "The pod hasn't started yet, so there is no log."
		}
		res.Error = msg
		return res
	}
	res.Lines = viewLogLines(raw, int(tail))
	return res
}

// viewLogLines splits, truncates and redacts log output for the log viewer.
func viewLogLines(raw []byte, tail int) []string {
	out := []string{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := strings.TrimRight(sc.Text(), "\r ")
		if len(l) > viewMaxLineLen {
			l = l[:viewMaxLineLen] + "…"
		}
		out = append(out, Redact(l))
	}
	if len(out) > tail {
		out = out[len(out)-tail:]
	}
	return out
}
