package agent

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"clusterheart/sre/internal/contract"
)

func TestReadLog(t *testing.T) {
	old := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "nightly-1-old", Labels: map[string]string{"job-name": "nightly-1"}, CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}}
	newer := old.DeepCopy()
	newer.Name, newer.CreationTimestamp = "nightly-1-new", metav1.NewTime(time.Now())
	newer.Spec.InitContainers = []corev1.Container{{Name: "setup"}}
	c := &Collector{clientset: fake.NewSimpleClientset(old, newer, probePod("api-1", corev1.PodRunning, "10.0.0.1"))}
	ctx := context.Background()

	r := c.ReadLog(ctx, contract.LogTarget{Namespace: "batch", Job: "nightly-1", Tail: 100})
	if r.Error != "" || r.Pod != "nightly-1-new" || r.Container != "main" || len(r.Containers) != 2 || len(r.Lines) == 0 {
		t.Fatalf("job log: %+v", r)
	}
	if r := c.ReadLog(ctx, contract.LogTarget{Namespace: "batch", Job: "gone", Tail: 100}); r.Error == "" {
		t.Fatal("missing job pods should say so")
	}
	if r := c.ReadLog(ctx, contract.LogTarget{Namespace: "web", Pod: "api-1", Tail: 100}); r.Error != "" || r.Container != "app" {
		t.Fatalf("pod log: %+v", r)
	}
	if r := c.ReadLog(ctx, contract.LogTarget{Namespace: "web", Pod: "nope"}); r.Error == "" {
		t.Fatal("missing pod should say so")
	}
}

func TestViewLogLines(t *testing.T) {
	got := viewLogLines([]byte("a\n\nb\nAKIAIOSFODNN7EXAMPLE\nc\n"), 3)
	if len(got) != 3 || got[0] != "b" || got[2] != "c" || got[1] == "AKIAIOSFODNN7EXAMPLE" {
		t.Fatalf("lines: %q", got)
	}
}
