package controlplane

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"clusterheart/sre/internal/contract"
)

// WorkloadUsage is a workload's peak per-pod usage over a window, computed from
// the stored snapshots. It is keyed by namespace + owning workload (e.g.
// "Deployment/api"), not pod name, so history carries across pod restarts,
// rollouts and agent restarts. Pods without a controller use "Pod/<name>".
type WorkloadUsage struct {
	Namespace    string    `json:"namespace" bson:"namespace"`
	Owner        string    `json:"owner" bson:"owner"`
	CPUPeakMilli int64     `json:"cpuPeakMilli" bson:"cpuPeakMilli"`
	MemPeakMi    int64     `json:"memPeakMi" bson:"memPeakMi"`
	Samples      int       `json:"samples" bson:"samples"`
	First        time.Time `json:"first" bson:"first"`
	Last         time.Time `json:"last" bson:"last"`
}

func workloadKey(p contract.PodInfo) string {
	if p.Owner != "" {
		return p.Owner
	}
	return "Pod/" + p.Name
}

func (m *memStore) WorkloadUsage(_ context.Context, clusterID string, since time.Time) ([]WorkloadUsage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agg := map[string]*WorkloadUsage{}
	var order []string
	for _, sn := range m.snapshots[clusterID] {
		if sn.CollectedAt.Before(since) {
			continue
		}
		for _, p := range sn.PodList {
			if p.CPUMilli == 0 && p.MemMi == 0 {
				continue // no metrics for this sample
			}
			k := p.Namespace + "|" + workloadKey(p)
			u := agg[k]
			if u == nil {
				u = &WorkloadUsage{Namespace: p.Namespace, Owner: workloadKey(p), First: sn.CollectedAt}
				agg[k] = u
				order = append(order, k)
			}
			u.CPUPeakMilli = max(u.CPUPeakMilli, p.CPUMilli)
			u.MemPeakMi = max(u.MemPeakMi, p.MemMi)
			u.Samples++
			if sn.CollectedAt.Before(u.First) {
				u.First = sn.CollectedAt
			}
			if sn.CollectedAt.After(u.Last) {
				u.Last = sn.CollectedAt
			}
		}
	}
	out := make([]WorkloadUsage, 0, len(order))
	for _, k := range order {
		out = append(out, *agg[k])
	}
	return out, nil
}

// WorkloadUsage aggregates the snapshot pod inventories in Mongo. Snapshot
// fields carry no bson tags, so the driver stores them lowercased
// (snapshot.podlist[].cpumilli, ...).
func (s *mongoStore) WorkloadUsage(ctx context.Context, clusterID string, since time.Time) ([]WorkloadUsage, error) {
	ownerExpr := bson.M{"$cond": bson.A{
		bson.M{"$gt": bson.A{bson.M{"$strLenCP": bson.M{"$ifNull": bson.A{"$p.owner", ""}}}, 0}},
		"$p.owner",
		bson.M{"$concat": bson.A{"Pod/", "$p.name"}},
	}}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"clusterId": clusterID, "collectedAt": bson.M{"$gte": since}}}},
		{{Key: "$project", Value: bson.M{"collectedAt": 1, "p": "$snapshot.podlist"}}},
		{{Key: "$unwind", Value: "$p"}},
		{{Key: "$match", Value: bson.M{"$or": bson.A{bson.M{"p.cpumilli": bson.M{"$gt": 0}}, bson.M{"p.memmi": bson.M{"$gt": 0}}}}}},
		{{Key: "$group", Value: bson.M{
			"_id":          bson.M{"ns": "$p.namespace", "owner": ownerExpr},
			"cpuPeakMilli": bson.M{"$max": "$p.cpumilli"},
			"memPeakMi":    bson.M{"$max": "$p.memmi"},
			"samples":      bson.M{"$sum": 1},
			"first":        bson.M{"$min": "$collectedAt"},
			"last":         bson.M{"$max": "$collectedAt"},
		}}},
		{{Key: "$project", Value: bson.M{
			"_id": 0, "namespace": "$_id.ns", "owner": "$_id.owner",
			"cpuPeakMilli": 1, "memPeakMi": 1, "samples": 1, "first": 1, "last": 1,
		}}},
	}
	cur, err := s.snapshots.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []WorkloadUsage{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// handleWorkloadUsage returns per-workload peak usage for one cluster:
// GET /api/v1/clusters/{id}/usage?tenantId=...&windowMins=360 (max 24h, the
// snapshot retention).
func (s *Server) handleWorkloadUsage(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	c, err := s.store.GetCluster(r.Context(), r.PathValue("id"))
	if err != nil || c.TenantID != tenantID {
		writeErr(w, http.StatusNotFound, "cluster not found")
		return
	}
	mins := 360
	if v, err := strconv.Atoi(r.URL.Query().Get("windowMins")); err == nil && v > 0 {
		mins = min(v, 24*60)
	}
	usage, err := s.store.WorkloadUsage(r.Context(), c.ID, s.now().Add(-time.Duration(mins)*time.Minute))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not load usage history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"windowMins": mins, "workloads": usage})
}
