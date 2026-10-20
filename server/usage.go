package main

// Usage history, proxied for the desktop dashboard. The dashboard samples
// per-workload CPU/memory and POSTs batches here (authenticated with the user's
// session token); the server stores them in Atlas tagged by user, runs the
// rightsizing window aggregation, and both continuously (8h TTL) and once a day
// (hard purge) cleans the data out. No database credential ever ships in the app.

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// usageSampleIn is one per-pod-normalised reading sent by the dashboard. The
// server stamps user + ts itself (never trusts the client for those).
type usageSampleIn struct {
	Namespace  string `json:"ns" bson:"ns"`
	Workload   string `json:"workload" bson:"workload"`
	OwnerKind  string `json:"ownerKind" bson:"ownerKind"`
	Replicas   int    `json:"replicas" bson:"replicas"`
	CPUPerPod  int64  `json:"cpuPerPod" bson:"cpuPerPod"`
	MemPerPod  int64  `json:"memPerPod" bson:"memPerPod"`
	CPURequest int64  `json:"cpuReq" bson:"cpuReq"`
	MemRequest int64  `json:"memReq" bson:"memReq"`
}

func (a *app) requireUser(w http.ResponseWriter, r *http.Request) *User {
	u := a.sessionUser(r)
	if u == nil || !u.Approved {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return nil
	}
	return u
}

// handleUsageIngest stores a batch of samples for the authenticated user.
func (a *app) handleUsageIngest(w http.ResponseWriter, r *http.Request) {
	u := a.requireUser(w, r)
	if u == nil {
		return
	}
	var body struct {
		Samples []usageSampleIn `json:"samples"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	if len(body.Samples) == 0 {
		writeJSON(w, 200, map[string]any{"ok": true, "stored": 0})
		return
	}
	now := time.Now().UTC()
	docs := make([]interface{}, 0, len(body.Samples))
	for _, s := range body.Samples {
		docs = append(docs, bson.M{
			"user": u.Email, "ts": now,
			"ns": s.Namespace, "workload": s.Workload, "ownerKind": s.OwnerKind,
			"replicas": s.Replicas, "cpuPerPod": s.CPUPerPod, "memPerPod": s.MemPerPod,
			"cpuReq": s.CPURequest, "memReq": s.MemRequest,
		})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if _, err := a.store.usage.InsertMany(ctx, docs); err != nil {
		writeJSON(w, 500, map[string]any{"error": "store failed"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "stored": len(docs)})
}

// usageAggRow mirrors the grouped shape the dashboard expects back.
type usageAggRow struct {
	NS        string    `bson:"ns" json:"ns"`
	Workload  string    `bson:"workload" json:"workload"`
	CPUP95    []float64 `bson:"cpuP95" json:"cpuP95"`
	MemP95    []float64 `bson:"memP95" json:"memP95"`
	CPUPeak   int64     `bson:"cpuPeak" json:"cpuPeak"`
	MemPeak   int64     `bson:"memPeak" json:"memPeak"`
	CPUReq    int64     `bson:"cpuReq" json:"cpuReq"`
	MemReq    int64     `bson:"memReq" json:"memReq"`
	Replicas  int       `bson:"replicas" json:"replicas"`
	OwnerKind string    `bson:"ownerKind" json:"ownerKind"`
	Samples   int       `bson:"samples" json:"samples"`
	First     time.Time `bson:"first" json:"first"`
	Last      time.Time `bson:"last" json:"last"`
}

// handleUsageAggregate runs the per-workload window aggregation for the user.
// Query: ns (ALL/empty = all namespaces), windowMins (default 360).
func (a *app) handleUsageAggregate(w http.ResponseWriter, r *http.Request) {
	u := a.requireUser(w, r)
	if u == nil {
		return
	}
	windowMins := 360
	if v := r.URL.Query().Get("windowMins"); v != "" {
		if n, err := time.ParseDuration(v + "m"); err == nil && n > 0 {
			windowMins = int(n.Minutes())
		}
	}
	cutoff := time.Now().UTC().Add(-time.Duration(windowMins) * time.Minute)
	match := bson.D{{Key: "user", Value: u.Email}, {Key: "ts", Value: bson.D{{Key: "$gte", Value: cutoff}}}}
	if ns := r.URL.Query().Get("ns"); ns != "" && ns != "ALL" {
		match = append(match, bson.E{Key: "ns", Value: ns})
	}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: match}},
		{{Key: "$sort", Value: bson.D{{Key: "ts", Value: 1}}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{{Key: "ns", Value: "$ns"}, {Key: "workload", Value: "$workload"}}},
			{Key: "ns", Value: bson.D{{Key: "$last", Value: "$ns"}}},
			{Key: "workload", Value: bson.D{{Key: "$last", Value: "$workload"}}},
			{Key: "cpuP95", Value: bson.D{{Key: "$percentile", Value: bson.D{{Key: "input", Value: "$cpuPerPod"}, {Key: "p", Value: bson.A{0.95}}, {Key: "method", Value: "approximate"}}}}},
			{Key: "memP95", Value: bson.D{{Key: "$percentile", Value: bson.D{{Key: "input", Value: "$memPerPod"}, {Key: "p", Value: bson.A{0.95}}, {Key: "method", Value: "approximate"}}}}},
			{Key: "cpuPeak", Value: bson.D{{Key: "$max", Value: "$cpuPerPod"}}},
			{Key: "memPeak", Value: bson.D{{Key: "$max", Value: "$memPerPod"}}},
			{Key: "cpuReq", Value: bson.D{{Key: "$last", Value: "$cpuReq"}}},
			{Key: "memReq", Value: bson.D{{Key: "$last", Value: "$memReq"}}},
			{Key: "replicas", Value: bson.D{{Key: "$last", Value: "$replicas"}}},
			{Key: "ownerKind", Value: bson.D{{Key: "$last", Value: "$ownerKind"}}},
			{Key: "samples", Value: bson.D{{Key: "$sum", Value: 1}}},
			{Key: "first", Value: bson.D{{Key: "$min", Value: "$ts"}}},
			{Key: "last", Value: bson.D{{Key: "$max", Value: "$ts"}}},
		}}},
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cur, err := a.store.usage.Aggregate(ctx, pipeline)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	defer cur.Close(ctx)
	rows := []usageAggRow{}
	if err := cur.All(ctx, &rows); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"rows": rows, "windowMins": windowMins})
}

// usagePurgeLoop hard-deletes all usage samples once a day at midnight UTC, on
// top of the continuous 8h TTL. Belt and suspenders: nothing lingers past a day.
func (s *store) usagePurgeLoop() {
	for {
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
		time.Sleep(time.Until(next))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, _ = s.usage.DeleteMany(ctx, bson.M{})
		cancel()
	}
}
