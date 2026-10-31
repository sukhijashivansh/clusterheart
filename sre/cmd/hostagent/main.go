// Command hostagent is the Clusterheart agent for a plain Linux server. It
// reads the machine's health as an unprivileged user and reports it to the
// control plane over outbound HTTPS. It never changes the machine.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"clusterheart/sre/internal/hostagent"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cpURL, id, token := os.Getenv("CONTROLPLANE_URL"), os.Getenv("CLUSTER_ID"), os.Getenv("AGENT_TOKEN")
	if cpURL == "" || id == "" || token == "" {
		log.Error("CONTROLPLANE_URL, CLUSTER_ID and AGENT_TOKEN are required")
		os.Exit(1)
	}
	interval := 30 * time.Second
	if d, err := time.ParseDuration(os.Getenv("COLLECT_INTERVAL")); err == nil && d >= 10*time.Second {
		interval = d
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("host agent starting", "version", hostagent.Version, "serverId", id, "interval", interval.String())
	hostagent.New(hostagent.NewClient(cpURL, id, token), interval, log).Run(ctx)
}
