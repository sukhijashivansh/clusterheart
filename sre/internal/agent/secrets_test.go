package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func ctrl(kind, name string) []metav1.OwnerReference {
	t := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &t}}
}

func podWithEnv(name string, env ...corev1.EnvVar) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "web", Name: name, OwnerReferences: ctrl("ReplicaSet", "api-5d8f")},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Env: env}}},
	}
}

func TestSpecScanFindsHardcodedSecrets(t *testing.T) {
	env := []corev1.EnvVar{
		{Name: "DB_PASSWORD", Value: "hunter22hunter"},                                                  // sensitive name, literal
		{Name: "AWS_ACCESS_KEY_ID", Value: "AKIAIOSFODNN7EXAMPLE"},                                      // real key shape
		{Name: "API_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{}}}, // from a Secret: fine
		{Name: "AUTH_ENABLED", Value: "true"},                                                           // boring value
		{Name: "LOG_LEVEL", Value: "debug"},                                                             // not sensitive
	}
	pods := []corev1.Pod{podWithEnv("api-5d8f-a", env...), podWithEnv("api-5d8f-b", env...)} // two replicas
	pods[0].Spec.Containers[0].Args = []string{"--db=postgres://app:s3cretpw@db:5432/app"}

	s := newSecretScanner()
	got := s.scan(context.Background(), nil, pods, time.Now())
	byField := map[string]string{}
	for _, f := range got {
		if f.Owner != "Deployment/api" {
			t.Errorf("finding not keyed by workload: %+v", f)
		}
		if strings.Contains(f.Context, "hunter22") || strings.Contains(f.Context, "AKIAIOSFODNN7EXAMPLE") || strings.Contains(f.Context, "s3cretpw") {
			t.Errorf("finding leaks the secret: %+v", f)
		}
		byField[f.Field] = f.Type
	}
	want := map[string]string{"DB_PASSWORD": "hardcoded-env", "AWS_ACCESS_KEY_ID": "aws-access-key", "args": "url-credentials"}
	if len(got) != len(want) {
		t.Fatalf("got %d findings (%v), want %d (replicas must not duplicate)", len(got), byField, len(want))
	}
	for f, typ := range want {
		if byField[f] != typ {
			t.Errorf("field %s: type %q, want %q", f, byField[f], typ)
		}
	}

	// Fixing the spec clears its findings on the next cycle.
	if again := s.scan(context.Background(), nil, []corev1.Pod{podWithEnv("api-5d8f-a")}, time.Now()); len(again) != 0 {
		t.Fatalf("findings remain after the spec was fixed: %+v", again)
	}
}

func TestLogLinesRecordedRedactedAndExpire(t *testing.T) {
	s := newSecretScanner()
	p := podWithEnv("api-5d8f-a")
	t0 := time.Now()
	s.recordLogLines(&p, "api", []byte("ok\nconnecting with password=hunter22\nGET /healthz 200\nuser key AKIAIOSFODNN7EXAMPLE\npassword=again99\n"), t0)
	got := s.scan(context.Background(), nil, nil, t0)
	counts := map[string]int{}
	for _, f := range got {
		if f.Source != "log" || strings.Contains(f.Context, "hunter22") || strings.Contains(f.Context, "AKIA") {
			t.Fatalf("bad log finding: %+v", f)
		}
		counts[f.Type] = f.Count
	}
	if counts["password-or-token"] != 2 || counts["aws-access-key"] != 1 {
		t.Fatalf("counts = %v", counts)
	}
	// Log findings drop out after the TTL.
	if later := s.scan(context.Background(), nil, nil, t0.Add(secretFindingTTL+time.Minute)); len(later) != 0 {
		t.Fatalf("findings not expired: %+v", later)
	}
}

func TestLogScanRotatesAndReadsIncrementally(t *testing.T) {
	var pods []corev1.Pod
	for i := 0; i < secretScanPerCycle+3; i++ {
		p := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "web", Name: "p" + string(rune('a'+i))},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
				{Name: "c", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			}},
		}
		pods = append(pods, p)
	}
	cs := fake.NewSimpleClientset()
	for i := range pods {
		_, _ = cs.CoreV1().Pods("web").Create(context.Background(), &pods[i], metav1.CreateOptions{})
	}
	s := newSecretScanner()
	s.scan(context.Background(), cs, pods, time.Now())
	if len(s.lastScan) != secretScanPerCycle {
		t.Fatalf("first cycle scanned %d containers, want %d", len(s.lastScan), secretScanPerCycle)
	}
	s.scan(context.Background(), cs, pods, time.Now())
	if len(s.lastScan) != len(pods) {
		t.Fatalf("rotation did not reach every container: %d of %d", len(s.lastScan), len(pods))
	}
	// Containers that went away are forgotten.
	s.scan(context.Background(), cs, pods[:2], time.Now())
	if len(s.lastScan) > 2 {
		t.Fatalf("stale containers kept: %d", len(s.lastScan))
	}
}
