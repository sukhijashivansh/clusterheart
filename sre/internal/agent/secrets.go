package agent

import (
	"bufio"
	"bytes"
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"clusterheart/sre/internal/contract"
	"clusterheart/sre/internal/redact"
)

// Secret-leak scanning bounds. Logs are scanned a few containers per cycle in
// rotation, reading only what was written since the last visit, so the whole
// cluster is covered every few minutes at a small, steady cost.
const (
	secretScanPerCycle   = 12
	secretScanBudget     = 8 * time.Second
	secretScanFirstTail  = 200
	secretScanLimitBytes = 256 << 10
	secretFindingTTL     = 24 * time.Hour
	maxSecretFindings    = 100
	maxContextLen        = 200
)

// sensitiveEnvName matches env var names that should come from a Secret.
var sensitiveEnvName = regexp.MustCompile(`(?i)(pass(word|wd)?|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential|auth|dsn|conn(ection)?[_-]?str)`)

type secretScanner struct {
	mu       sync.Mutex
	cursor   int
	lastScan map[string]time.Time               // ns/pod/container -> last log read
	logs     map[string]*contract.SecretFinding // log findings, kept secretFindingTTL
	specs    map[string]*contract.SecretFinding // spec findings, recomputed every cycle
}

func newSecretScanner() *secretScanner {
	return &secretScanner{lastScan: map[string]time.Time{}, logs: map[string]*contract.SecretFinding{}, specs: map[string]*contract.SecretFinding{}}
}

func podOwner(p *corev1.Pod) string {
	if o := ownerWorkload(p); o != "" {
		return o
	}
	return "Pod/" + p.Name
}

func clip(s string) string {
	if len(s) > maxContextLen {
		return s[:maxContextLen] + "…"
	}
	return s
}

// scan updates findings from pod specs and a slice of container logs, and
// returns the current set (most recently seen first, capped).
func (s *secretScanner) scan(ctx context.Context, cs kubernetes.Interface, pods []corev1.Pod, now time.Time) []contract.SecretFinding {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scanSpecs(pods, now)
	if cs != nil {
		s.scanLogs(ctx, cs, pods, now)
	}
	for k, f := range s.logs {
		if now.Sub(f.LastSeen) > secretFindingTTL {
			delete(s.logs, k)
		}
	}
	out := make([]contract.SecretFinding, 0, len(s.specs)+len(s.logs))
	for _, f := range s.logs {
		out = append(out, *f)
	}
	for _, f := range s.specs {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].Namespace+out[i].Owner+out[i].Field < out[j].Namespace+out[j].Owner+out[j].Field
	})
	if len(out) > maxSecretFindings {
		out = out[:maxSecretFindings]
	}
	return out
}

// scanSpecs flags credentials written literally into env vars or container
// args. Values from Secrets/ConfigMaps (valueFrom) are not inspected.
func (s *secretScanner) scanSpecs(pods []corev1.Pod, now time.Time) {
	prev := s.specs
	s.specs = map[string]*contract.SecretFinding{}
	add := func(p *corev1.Pod, container, source, field, typ, ctx string) {
		owner := podOwner(p)
		key := source + "|" + p.Namespace + "|" + owner + "|" + container + "|" + field + "|" + typ
		if _, ok := s.specs[key]; ok {
			return
		}
		f := &contract.SecretFinding{
			Source: source, Type: typ, Namespace: p.Namespace, Owner: owner, Pod: p.Name,
			Container: container, Field: field, Context: clip(ctx), Count: 1, FirstSeen: now, LastSeen: now,
		}
		if old, ok := prev[key]; ok {
			f.FirstSeen = old.FirstSeen
		}
		s.specs[key] = f
	}
	for i := range pods {
		p := &pods[i]
		all := append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...)
		for _, c := range all {
			for _, e := range c.Env {
				v := strings.TrimSpace(e.Value)
				if e.ValueFrom != nil || v == "" || strings.HasPrefix(v, "$(") || strings.HasPrefix(v, "${") {
					continue
				}
				if types := redact.Find(v); len(types) > 0 {
					add(p, c.Name, "env", e.Name, types[0], e.Name+"="+redact.String(v))
					continue
				}
				if sensitiveEnvName.MatchString(e.Name) && len(v) >= 6 && !isBoringValue(v) {
					add(p, c.Name, "env", e.Name, "hardcoded-env", e.Name+"=[REDACTED]")
				}
			}
			cmd := strings.Join(append(append([]string{}, c.Command...), c.Args...), " ")
			for _, typ := range redact.Find(cmd) {
				add(p, c.Name, "args", "args", typ, redact.String(cmd))
			}
		}
	}
}

// isBoringValue skips values that are clearly not credentials (flags, numbers,
// paths, hostnames-with-ports, URLs without userinfo).
func isBoringValue(v string) bool {
	l := strings.ToLower(v)
	switch l {
	case "true", "false", "yes", "no", "enabled", "disabled", "none", "null", "changeme":
		return true
	}
	if strings.HasPrefix(v, "/") || strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") {
		return true
	}
	allDigits := true
	for _, r := range v {
		if r < '0' || r > '9' {
			allDigits = false
			break
		}
	}
	return allDigits
}

// scanLogs reads new log output from the next few running containers in
// rotation and records lines that contain a credential.
func (s *secretScanner) scanLogs(ctx context.Context, cs kubernetes.Interface, pods []corev1.Pod, now time.Time) {
	type target struct {
		pod       *corev1.Pod
		container string
	}
	var targets []target
	live := map[string]bool{}
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, st := range p.Status.ContainerStatuses {
			if st.State.Running == nil {
				continue
			}
			targets = append(targets, target{p, st.Name})
			live[p.Namespace+"/"+p.Name+"/"+st.Name] = true
		}
	}
	for k := range s.lastScan {
		if !live[k] {
			delete(s.lastScan, k)
		}
	}
	if len(targets) == 0 {
		return
	}
	sort.Slice(targets, func(i, j int) bool {
		a, b := targets[i], targets[j]
		return a.pod.Namespace+"/"+a.pod.Name+"/"+a.container < b.pod.Namespace+"/"+b.pod.Name+"/"+b.container
	})
	deadline := time.Now().Add(secretScanBudget)
	n := min(secretScanPerCycle, len(targets))
	for i := 0; i < n && time.Now().Before(deadline); i++ {
		t := targets[(s.cursor+i)%len(targets)]
		key := t.pod.Namespace + "/" + t.pod.Name + "/" + t.container
		opts := &corev1.PodLogOptions{Container: t.container, LimitBytes: ptr(int64(secretScanLimitBytes))}
		if last, ok := s.lastScan[key]; ok {
			opts.SinceTime = &metav1.Time{Time: last}
		} else {
			opts.TailLines = ptr(int64(secretScanFirstTail))
		}
		cctx, cancel := context.WithTimeout(ctx, logCallTimeout)
		raw, err := cs.CoreV1().Pods(t.pod.Namespace).GetLogs(t.pod.Name, opts).DoRaw(cctx)
		cancel()
		if err != nil {
			continue
		}
		s.lastScan[key] = now
		s.recordLogLines(t.pod, t.container, raw, now)
	}
	s.cursor = (s.cursor + n) % len(targets)
}

func (s *secretScanner) recordLogLines(p *corev1.Pod, container string, raw []byte, now time.Time) {
	owner := podOwner(p)
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		types := redact.Find(line)
		if len(types) == 0 {
			continue
		}
		for _, typ := range types {
			key := "log|" + p.Namespace + "|" + owner + "|" + container + "|" + typ
			f := s.logs[key]
			if f == nil {
				f = &contract.SecretFinding{Source: "log", Type: typ, Namespace: p.Namespace, Owner: owner, Container: container, FirstSeen: now}
				s.logs[key] = f
			}
			f.Pod = p.Name
			f.Count++
			f.LastSeen = now
			f.Context = clip(redact.String(strings.TrimSpace(line)))
		}
	}
}

func ptr[T any](v T) *T { return &v }
