package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"clusterheart/sre/internal/contract"
)

func probePod(name string, phase corev1.PodPhase, ip string, ports ...int32) *corev1.Pod {
	var cp []corev1.ContainerPort
	for _, p := range ports {
		cp = append(cp, corev1.ContainerPort{ContainerPort: p})
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "web", Name: name},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Ports: cp}}},
		Status: corev1.PodStatus{Phase: phase, PodIP: ip}}
}

func TestRunProbeAgainstPodIP(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte("ok"))
	}))
	defer ts.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	c := &Collector{clientset: fake.NewSimpleClientset(
		probePod("api-1", corev1.PodRunning, "127.0.0.1", int32(port)),
		probePod("api-2", corev1.PodPending, ""),
		probePod("noport", corev1.PodRunning, "127.0.0.1"),
	)}
	// Port 0: the pod's declared port is used.
	res := c.RunProbe(context.Background(), contract.ProbeTarget{Namespace: "web", Pod: "api-1", Path: "/healthz", Samples: 2})
	if res.Error != "" || res.OKCount != 2 || res.Port != port || gotPath != "/healthz" || !strings.HasPrefix(res.URL, "http://127.0.0.1:") {
		t.Fatalf("result: %+v path=%q", res, gotPath)
	}
	for name, want := range map[string]string{"api-2": "not running", "noport": "does not declare a port", "gone": "not found"} {
		if r := c.RunProbe(context.Background(), contract.ProbeTarget{Namespace: "web", Pod: name, Samples: 1}); !strings.Contains(r.Error, want) {
			t.Errorf("%s: error %q, want %q", name, r.Error, want)
		}
	}
	// An explicit port wins over the declared one.
	if r := c.RunProbe(context.Background(), contract.ProbeTarget{Namespace: "web", Pod: "noport", Port: port, Samples: 1}); r.OKCount != 1 {
		t.Fatalf("explicit port: %+v", r)
	}
}

// TestProbeLoopRunsQueuedChecks: the agent picks up a check from the control
// plane, times the pod, and posts the result for that check.
func TestProbeLoopRunsQueuedChecks(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer app.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(app.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	results := make(chan contract.ProbeResult, 1)
	served := false
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agents/probes":
			out := contract.ProbeRequestsResponse{Requests: []contract.ProbeRequest{}}
			if !served {
				served = true
				out.Requests = append(out.Requests, contract.ProbeRequest{ID: "prb_1", Target: contract.ProbeTarget{Namespace: "web", Pod: "api-1", Port: port, Scheme: "http", Path: "/", Samples: 3}})
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/probes/prb_1":
			var res contract.ProbeResult
			json.NewDecoder(r.Body).Decode(&res)
			results <- res
		default:
			w.WriteHeader(404)
		}
	}))
	defer cp.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &Runner{ProbeEvery: 20 * time.Millisecond, Client: NewClient(cp.URL, "clu_1", "tok"), Collector: &Collector{clientset: fake.NewSimpleClientset(probePod("api-1", corev1.PodRunning, "127.0.0.1"))}}
	go r.probeLoop(ctx)
	select {
	case res := <-results:
		if res.OKCount != 3 || res.Samples[0].Status != 204 || res.Port != port {
			t.Fatalf("result: %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no result posted")
	}
}
