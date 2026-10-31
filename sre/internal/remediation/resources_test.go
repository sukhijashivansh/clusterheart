package remediation

import (
	"context"
	"strings"
	"testing"
	"time"

	"clusterheart/sre/internal/contract"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func withContainers(tmpl *corev1.PodTemplateSpec, cs ...corev1.Container) {
	tmpl.Spec.Containers = cs
}

func rsAction(params map[string]string, dry bool) contract.PendingAction {
	return contract.PendingAction{
		Action: "SetResources", Target: contract.ActionTarget{Kind: "Deployment", Namespace: "web", Name: "api"},
		Params: params, DryRun: dry, TimeoutSeconds: 5,
	}
}

func TestSetResourcesApplies(t *testing.T) {
	d := newDeployment("web", "api", 2, true)
	withContainers(&d.Spec.Template, corev1.Container{Name: "api", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
	}})
	e := NewExecutorWithClient(fake.NewSimpleClientset(d), nil)
	e.verifyInterval = 10 * time.Millisecond
	res := e.Execute(context.Background(), rsAction(map[string]string{"cpu": "84", "memory": "144"}, false))
	if !res.Success {
		t.Fatalf("apply failed: %q", res.Error)
	}
	if !strings.Contains(res.BeforeState, "cpu request=500m") || !strings.Contains(res.BeforeState, "memory request=512Mi") {
		t.Fatalf("before-state = %q", res.BeforeState)
	}
	got, _ := e.client.AppsV1().Deployments("web").Get(context.Background(), "api", metav1.GetOptions{})
	req := got.Spec.Template.Spec.Containers[0].Resources.Requests
	if req.Cpu().MilliValue() != 84 || req.Memory().Value() != 144*1024*1024 {
		t.Fatalf("requests not applied: %v", req)
	}
}

func TestSetResourcesOnlyCPUKeepsMemory(t *testing.T) {
	d := newDeployment("web", "api", 1, true)
	withContainers(&d.Spec.Template, corev1.Container{Name: "api", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
	}})
	e := NewExecutorWithClient(fake.NewSimpleClientset(d), nil)
	e.verifyInterval = 10 * time.Millisecond
	if res := e.Execute(context.Background(), rsAction(map[string]string{"cpu": "200"}, false)); !res.Success {
		t.Fatalf("apply failed: %q", res.Error)
	}
	got, _ := e.client.AppsV1().Deployments("web").Get(context.Background(), "api", metav1.GetOptions{})
	req := got.Spec.Template.Spec.Containers[0].Resources.Requests
	if req.Cpu().MilliValue() != 200 || req.Memory().String() != "256Mi" {
		t.Fatalf("requests = %v", req)
	}
}

func TestSetResourcesRefusesMultiContainer(t *testing.T) {
	d := newDeployment("web", "api", 1, true)
	withContainers(&d.Spec.Template, corev1.Container{Name: "api"}, corev1.Container{Name: "sidecar"})
	e := NewExecutorWithClient(fake.NewSimpleClientset(d), nil)
	res := e.Execute(context.Background(), rsAction(map[string]string{"cpu": "200"}, false))
	if res.Success || !strings.Contains(res.Error, "single-container") {
		t.Fatalf("expected single-container refusal, got %+v", res)
	}
}

func TestSetResourcesRefusesAboveLimit(t *testing.T) {
	d := newDeployment("web", "api", 1, true)
	withContainers(&d.Spec.Template, corev1.Container{Name: "api", Resources: corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
	}})
	e := NewExecutorWithClient(fake.NewSimpleClientset(d), nil)
	res := e.Execute(context.Background(), rsAction(map[string]string{"cpu": "800"}, false))
	if res.Success || !strings.Contains(res.Error, "above the container's limit") {
		t.Fatalf("expected limit refusal, got %+v", res)
	}
}

func TestSetResourcesDryRunDoesNotPersist(t *testing.T) {
	d := newDeployment("web", "api", 1, true)
	withContainers(&d.Spec.Template, corev1.Container{Name: "api"})
	e := NewExecutorWithClient(fake.NewSimpleClientset(d), nil)
	res := e.Execute(context.Background(), rsAction(map[string]string{"memory": "128"}, true))
	if !res.Success || !res.DryRun || res.VerificationPassed != nil {
		t.Fatalf("dry run result = %+v", res)
	}
}
