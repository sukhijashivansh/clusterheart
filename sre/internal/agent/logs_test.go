package agent

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"clusterheart/sre/internal/contract"
)

func TestLogTargetsPicksCrashesThenFailedPods(t *testing.T) {
	probs := []contract.PodProblem{
		{Namespace: "web", Pod: "api-1", Container: "api", Type: contract.ProblemCrashLoopBackOff, RestartCount: 4},
		{Namespace: "web", Pod: "img-1", Container: "img", Type: contract.ProblemImagePullBackOff},           // never ran: skipped
		{Namespace: "web", Pod: "api-1", Container: "api", Type: contract.ProblemOOMKilled, RestartCount: 4}, // duplicate container
	}
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "jobs", Name: "nightly-x"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "run"}}}, Status: corev1.PodStatus{Phase: corev1.PodFailed}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "web", Name: "ok-1"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "ok"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
	}
	got := logTargets(probs, pods)
	if len(got) != 2 {
		t.Fatalf("targets = %+v", got)
	}
	if got[0].pod != "api-1" || !got[0].previous || got[0].reason != "CrashLoopBackOff" {
		t.Errorf("crash target = %+v", got[0])
	}
	if got[1].pod != "nightly-x" || got[1].previous || got[1].reason != "Failed" {
		t.Errorf("failed-pod target = %+v", got[1])
	}
}

func TestCleanLogLinesTruncatesRedactsAndTails(t *testing.T) {
	var b strings.Builder
	for i := 0; i < logTailLines+10; i++ {
		b.WriteString("line\n")
	}
	b.WriteString("connecting with password=hunter22 now\n")
	b.WriteString(strings.Repeat("x", maxLogLineLen+50) + "\n")
	lines := cleanLogLines([]byte(b.String()))
	if len(lines) != logTailLines {
		t.Fatalf("got %d lines, want %d", len(lines), logTailLines)
	}
	secret := lines[len(lines)-2]
	if strings.Contains(secret, "hunter22") {
		t.Errorf("secret not redacted: %q", secret)
	}
	if long := lines[len(lines)-1]; len(long) > maxLogLineLen+len("…") {
		t.Errorf("long line not truncated: %d bytes", len(long))
	}
}

func TestPodLogsReadsAndCaches(t *testing.T) {
	c := &Collector{clientset: fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "web", Name: "api-1"}})}
	targets := []logTarget{{ns: "web", pod: "api-1", container: "api", reason: "CrashLoopBackOff"}}
	t0 := time.Now()
	got := c.podLogs(context.Background(), targets, t0)
	if len(got) != 1 || len(got[0].Lines) == 0 || got[0].Pod != "api-1" || !got[0].CollectedAt.Equal(t0) {
		t.Fatalf("logs = %+v", got)
	}
	// Within the TTL the cached copy is reused (same CollectedAt).
	again := c.podLogs(context.Background(), targets, t0.Add(30*time.Second))
	if len(again) != 1 || !again[0].CollectedAt.Equal(t0) {
		t.Fatalf("expected cached logs, got %+v", again)
	}
	// A pod that recovered is forgotten.
	c.podLogs(context.Background(), nil, t0.Add(time.Minute))
	if len(c.logs.m) != 0 {
		t.Fatalf("cache not pruned: %v", c.logs.m)
	}
}

func TestBufferKeepsLogsOnlyOnNewestSnapshot(t *testing.T) {
	r := &Runner{Collector: &Collector{clientset: fake.NewSimpleClientset()}, Log: slog.Default()}
	r.buffer = []contract.ClusterSnapshot{{PodLogs: []contract.PodLog{{Pod: "old", Lines: []string{"x"}}}}}
	r.collectOnce(context.Background())
	if len(r.buffer) != 2 {
		t.Fatalf("collectOnce did not buffer a snapshot (len %d)", len(r.buffer))
	}
	if r.buffer[0].PodLogs != nil {
		t.Fatalf("older snapshot kept its logs: %+v", r.buffer[0].PodLogs)
	}
}
