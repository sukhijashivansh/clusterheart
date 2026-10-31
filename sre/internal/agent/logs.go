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
