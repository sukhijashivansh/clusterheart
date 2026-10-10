// Command agent is the ClusterHeart in-cluster collector. It reads the cluster
// with a read-only ServiceAccount and forwards bounded health snapshots to the
// control plane. Milestone 1: observe-only. It has no mutation code path.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"clusterheart/sre/internal/agent"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cpURL := os.Getenv("CONTROLPLANE_URL")
	clusterID := os.Getenv("CLUSTER_ID")
	token := os.Getenv("AGENT_TOKEN")
	kubeconfig := os.Getenv("KUBECONFIG") // empty => in-cluster config
	interval := parseInterval(os.Getenv("COLLECT_INTERVAL"), 30*time.Second)

	if cpURL == "" || clusterID == "" || token == "" {
		log.Error("CONTROLPLANE_URL, CLUSTER_ID and AGENT_TOKEN are required")
		os.Exit(1)
	}

	collector, err := agent.NewCollector(kubeconfig)
	if err != nil {
		log.Error("could not build kubernetes client", "err", err)
		os.Exit(1)
	}
	log.Info("agent starting", "version", agent.Version, "clusterId", clusterID,
		"k8sVersion", collector.K8sVersion(), "interval", interval.String())

	runner := &agent.Runner{
		Client:    agent.NewClient(cpURL, clusterID, token),
		Collector: collector,
		Interval:  interval,
		Log:       log,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-stop; log.Info("shutting down agent"); cancel() }()

	runner.Run(ctx)
}

func parseInterval(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 5*time.Second {
		return d
	}
	return def
}
