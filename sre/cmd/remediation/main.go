// Command remediation is the in-cluster Remediation Controller. It runs under a
// narrow `remediate` ServiceAccount, polls the control plane for approved
// actions targeting its cluster, executes them (dry-run or real, per policy),
// and reports the outcome. It performs no detection and runs no AI; it only
// applies the small set of typed, pre-approved actions it is handed.
package main

import (
	"context"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"clusterheart/sre/internal/remediation"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cpURL := os.Getenv("CONTROLPLANE_URL")
	clusterID := os.Getenv("CLUSTER_ID")
	token := os.Getenv("REMEDIATION_TOKEN")
	kubeconfig := os.Getenv("KUBECONFIG") // empty => in-cluster
	interval := parseDur(os.Getenv("POLL_INTERVAL"), 15*time.Second)

	if cpURL == "" || clusterID == "" || token == "" {
		log.Error("CONTROLPLANE_URL, CLUSTER_ID and REMEDIATION_TOKEN are required")
		os.Exit(1)
	}

	exec, err := remediation.NewExecutor(kubeconfig, log)
	if err != nil {
		log.Error("could not build kubernetes client", "err", err)
		os.Exit(1)
	}
	client := remediation.NewControlPlaneClient(cpURL, clusterID, token)
	log.Info("remediation controller starting", "clusterId", clusterID, "pollInterval", interval.String())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-stop; log.Info("shutting down controller"); cancel() }()

	failures := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if processOnce(ctx, log, client, exec) {
			failures = 0
			sleep(ctx, jitter(interval))
		} else {
			failures++
			sleep(ctx, backoff(failures))
		}
	}
}

// processOnce pulls pending actions and executes each, reporting results.
// Returns false if it could not reach the control plane (triggers backoff).
func processOnce(ctx context.Context, log *slog.Logger, client *remediation.ControlPlaneClient, exec *remediation.Executor) bool {
	actions, err := client.Pending(ctx)
	if err != nil {
		log.Warn("could not fetch pending actions", "err", err)
		return false
	}
	for _, a := range actions {
		log.Info("executing action", "proposalId", a.ProposalID, "action", a.Action, "target", a.Target, "dryRun", a.DryRun)
		res := exec.Execute(ctx, a)
		if err := client.Report(ctx, a.ProposalID, res); err != nil {
			log.Error("could not report result", "proposalId", a.ProposalID, "err", err)
			// Keep going; the action remains approved and will be retried next poll.
			continue
		}
		log.Info("reported result", "proposalId", a.ProposalID, "success", res.Success, "dryRun", res.DryRun)
	}
	return true
}

func backoff(failures int) time.Duration {
	const base, cap = 2 * time.Second, 60 * time.Second
	d := base << min(failures-1, 5)
	if d > cap {
		d = cap
	}
	return jitter(d)
}

func jitter(d time.Duration) time.Duration {
	delta := float64(d) * 0.2
	return time.Duration(float64(d) - delta + rand.Float64()*2*delta)
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func parseDur(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d >= 5*time.Second {
		return d
	}
	return def
}
