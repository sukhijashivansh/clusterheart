package remediation

import (
	"context"
	"testing"
	"time"

	"clusterheart/sre/internal/contract"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newDeployment(ns, name string, replicas int32, healthy bool) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1, ResourceVersion: "1"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}}},
		},
	}
	d.Status.ObservedGeneration = 1
	if healthy {
		d.Status.UpdatedReplicas = replicas
		d.Status.AvailableReplicas = replicas
		d.Status.ReadyReplicas = replicas
	}
	return d
}

func TestRolloutRestartDeploymentApplies(t *testing.T) {
	e := NewExecutorWithClient(fake.NewSimpleClientset(newDeployment("web", "api", 2, true)), nil)
	e.verifyInterval = 10 * time.Millisecond
	res := e.Execute(context.Background(), contract.PendingAction{
		Action: "RolloutRestart", Target: contract.ActionTarget{Kind: "Deployment", Namespace: "web", Name: "api"},
		DryRun: false, TimeoutSeconds: 5,
	})
	if !res.Success {
		t.Fatalf("apply failed: %q", res.Error)
	}
	if res.VerificationPassed == nil || !*res.VerificationPassed {
		t.Fatalf("expected verification to pass, got %v", res.VerificationPassed)
	}
	// The restart annotation must now be set on the live object.
	d, _ := e.client.AppsV1().Deployments("web").Get(context.Background(), "api", metav1.GetOptions{})
	if d.Spec.Template.Annotations[restartAnnotation] == "" {
		t.Fatal("restart annotation was not applied")
	}
}

func TestRolloutRestartDryRunReportsNoVerification(t *testing.T) {
	e := NewExecutorWithClient(fake.NewSimpleClientset(newDeployment("web", "api", 2, true)), nil)
	res := e.Execute(context.Background(), contract.PendingAction{
		Action: "RolloutRestart", Target: contract.ActionTarget{Kind: "Deployment", Namespace: "web", Name: "api"},
		DryRun: true, TimeoutSeconds: 5,
	})
	if !res.Success || !res.DryRun {
		t.Fatalf("dry-run should succeed: %+v", res)
	}
	if res.VerificationPassed != nil {
		t.Fatal("dry-run must not verify a live rollout")
	}
}

func TestRolloutRestartNotFound(t *testing.T) {
	e := NewExecutorWithClient(fake.NewSimpleClientset(), nil)
	res := e.Execute(context.Background(), contract.PendingAction{
		Action: "RolloutRestart", Target: contract.ActionTarget{Kind: "Deployment", Namespace: "web", Name: "api"},
	})
	if res.Success {
		t.Fatal("missing deployment should fail")
	}
}

func TestRolloutRestartVerificationFails(t *testing.T) {
	e := NewExecutorWithClient(fake.NewSimpleClientset(newDeployment("web", "api", 2, false)), nil)
	e.verifyInterval = 10 * time.Millisecond
	res := e.Execute(context.Background(), contract.PendingAction{
		Action: "RolloutRestart", Target: contract.ActionTarget{Kind: "Deployment", Namespace: "web", Name: "api"},
		DryRun: false, TimeoutSeconds: 1,
	})
	if res.Success {
		t.Fatal("unhealthy rollout should report failure")
	}
	if res.VerificationPassed == nil || *res.VerificationPassed {
		t.Fatalf("verification should have failed, got %v", res.VerificationPassed)
	}
}

func TestUnsupportedKindRejected(t *testing.T) {
	e := NewExecutorWithClient(fake.NewSimpleClientset(), nil)
	res := e.Execute(context.Background(), contract.PendingAction{
		Action: "RolloutRestart", Target: contract.ActionTarget{Kind: "Pod", Namespace: "web", Name: "api-1"},
	})
	if res.Success {
		t.Fatal("RolloutRestart on a Pod must fail")
	}
}

func TestStatefulSetApplies(t *testing.T) {
	ss := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg", ResourceVersion: "1"}}
	e := NewExecutorWithClient(fake.NewSimpleClientset(ss), nil)
	res := e.Execute(context.Background(), contract.PendingAction{
		Action: "RolloutRestart", Target: contract.ActionTarget{Kind: "StatefulSet", Namespace: "db", Name: "pg"},
		DryRun: false, TimeoutSeconds: 5,
	})
	if !res.Success {
		t.Fatalf("statefulset apply failed: %q", res.Error)
	}
	got, _ := e.client.AppsV1().StatefulSets("db").Get(context.Background(), "pg", metav1.GetOptions{})
	if got.Spec.Template.Annotations[restartAnnotation] == "" {
		t.Fatal("restart annotation not applied to statefulset")
	}
}
