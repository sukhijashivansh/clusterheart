package agent

import (
	"context"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"clusterheart/sre/internal/contract"
	"clusterheart/sre/internal/probe"
)

const (
	probeMaxTime  = 60 * time.Second // one check, all samples
	probeParallel = 2
)

// defaultProbeEvery is how often the agent checks for latency checks to run.
const defaultProbeEvery = 4 * time.Second

// probePort picks the port to check: the requested one, or the pod's first
// declared TCP port.
func probePort(pod *corev1.Pod, want int) int {
	if want > 0 {
		return want
	}
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.Protocol == "" || p.Protocol == corev1.ProtocolTCP {
				return int(p.ContainerPort)
			}
		}
	}
	return 0
}

// RunProbe times GET requests from the agent straight to a pod's IP.
func (c *Collector) RunProbe(ctx context.Context, t contract.ProbeTarget) contract.ProbeResult {
	pod, err := c.clientset.CoreV1().Pods(t.Namespace).Get(ctx, t.Pod, metav1.GetOptions{})
	if err != nil {
		return contract.ProbeResult{Error: "pod " + t.Namespace + "/" + t.Pod + " not found (it may have been replaced; pick it again)"}
	}
	if pod.Status.PodIP == "" || pod.Status.Phase != corev1.PodRunning {
		return contract.ProbeResult{Error: "the pod is not running (" + string(pod.Status.Phase) + ")"}
	}
	port := probePort(pod, t.Port)
	if port <= 0 || port > 65535 {
		return contract.ProbeResult{Error: "this pod does not declare a port; enter the port it listens on"}
	}
	scheme := "http"
	if t.Scheme == "https" {
		scheme = "https"
	}
	u := url.URL{Scheme: scheme, Host: net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(port)), Path: t.Path}
	if u.Path == "" {
		u.Path = "/"
	}
	res := probe.Run(ctx, u.String(), t.Samples)
	res.Port = port
	return res
}

// probeLoop runs latency checks people start in the dashboard. Only GET
// requests are sent, and only timings, status and sizes are reported back.
func (r *Runner) probeLoop(ctx context.Context) {
	every := r.ProbeEvery
	if every <= 0 {
		every = defaultProbeEvery
	}
	sem := make(chan struct{}, probeParallel)
	for {
		sleep(ctx, every)
		if ctx.Err() != nil {
			return
		}
		var q contract.ProbeRequestsResponse
		if err := r.Client.get(ctx, "/api/v1/agents/probes", &q); err != nil {
			continue // control plane down or older: try again later
		}
		for _, req := range q.Requests {
			req := req
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func() {
				defer func() { <-sem }()
				pctx, cancel := context.WithTimeout(ctx, probeMaxTime)
				res := r.Collector.RunProbe(pctx, req.Target)
				cancel()
				if err := r.Client.post(ctx, "/api/v1/agents/probes/"+url.PathEscape(req.ID), res); err != nil {
					logger(r.Log).Warn("could not send latency check result", "id", req.ID, "err", err)
				}
			}()
		}
	}
}

func logger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}
