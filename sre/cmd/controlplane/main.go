// Command controlplane is the ClusterHeart Autonomous SRE control plane.
// Milestone 1: cluster registration, agent heartbeats, telemetry ingestion,
// and operator read-back. No detection / AI / remediation yet.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"clusterheart/sre/internal/controlplane"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	addr := envOr("ADDR", defaultAddr())
	adminToken := os.Getenv("CONTROLPLANE_ADMIN_TOKEN")
	mongoURI := os.Getenv("MONGO_URI")
	mongoDB := envOr("MONGO_DB", "clusterheart")

	if adminToken == "" {
		log.Error("CONTROLPLANE_ADMIN_TOKEN is empty — operator endpoints are unreachable until it is set")
	}

	var store controlplane.Store
	var incidents controlplane.IncidentStore
	if mongoURI == "" {
		log.Warn("MONGO_URI not set — using in-memory stores (NOT durable; local dev only)")
		store = controlplane.NewMemStore()
		incidents = controlplane.NewMemIncidentStore()
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		db, err := controlplane.ConnectMongo(ctx, mongoURI, mongoDB)
		if err != nil {
			cancel()
			log.Error("mongo connect failed", "err", err)
			os.Exit(1)
		}
		if store, err = controlplane.NewMongoStore(ctx, db); err != nil {
			cancel()
			log.Error("mongo store init failed", "err", err)
			os.Exit(1)
		}
		if incidents, err = controlplane.NewMongoIncidentStore(ctx, db); err != nil {
			cancel()
			log.Error("mongo incident store init failed", "err", err)
			os.Exit(1)
		}
		cancel()
		log.Info("connected to MongoDB", "db", mongoDB)
	}

	srv := controlplane.NewServer(store, incidents, adminToken, log)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Info("control plane listening", "addr", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown on SIGINT/SIGTERM.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

func defaultAddr() string {
	if p := os.Getenv("PORT"); p != "" {
		return "0.0.0.0:" + p
	}
	return "127.0.0.1:9100"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
