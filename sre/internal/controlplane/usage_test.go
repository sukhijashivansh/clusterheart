package controlplane

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"clusterheart/sre/internal/contract"
)

func TestWorkloadUsageGroupsByOwnerAcrossPods(t *testing.T) {
	ctx := context.Background()
	st := NewMemStore()
	if err := st.CreateCluster(ctx, Cluster{ID: "c1", TenantID: "t1"}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	snap := func(at time.Time, pods ...contract.PodInfo) contract.ClusterSnapshot {
		return contract.ClusterSnapshot{CollectedAt: at, PodList: pods}
	}
	// The api pod is replaced mid-window by a new pod of the same Deployment:
	// its peak must carry over. The bare pod has no owner.
	_ = st.SaveSnapshots(ctx, "c1", []contract.ClusterSnapshot{
		snap(t0, contract.PodInfo{Namespace: "prod", Name: "api-1", Owner: "Deployment/api", CPUMilli: 300, MemMi: 100}),
		snap(t0.Add(time.Hour), contract.PodInfo{Namespace: "prod", Name: "api-2", Owner: "Deployment/api", CPUMilli: 120, MemMi: 250}),
		snap(t0.Add(2*time.Hour),
			contract.PodInfo{Namespace: "prod", Name: "api-2", Owner: "Deployment/api"}, // no metrics: ignored
			contract.PodInfo{Namespace: "prod", Name: "debug", CPUMilli: 5, MemMi: 10}),
	})

	got, err := st.WorkloadUsage(ctx, "c1", t0.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]WorkloadUsage{}
	for _, u := range got {
		by[u.Owner] = u
	}
	api := by["Deployment/api"]
	if api.CPUPeakMilli != 300 || api.MemPeakMi != 250 || api.Samples != 2 || !api.First.Equal(t0) || !api.Last.Equal(t0.Add(time.Hour)) {
		t.Fatalf("api usage = %+v", api)
	}
	if by["Pod/debug"].Samples != 1 {
		t.Fatalf("bare pod not keyed by name: %+v", got)
	}

	// The window excludes older snapshots.
	got, _ = st.WorkloadUsage(ctx, "c1", t0.Add(30*time.Minute))
	for _, u := range got {
		if u.Owner == "Deployment/api" && u.CPUPeakMilli != 120 {
			t.Fatalf("window not applied: %+v", u)
		}
	}
}

// The Mongo pipeline in WorkloadUsage addresses snapshot fields by the names the
// driver gives untagged struct fields. Guard those names.
func TestSnapshotBSONFieldNames(t *testing.T) {
	raw, err := bson.Marshal(snapshotDoc{ClusterID: "c1", CollectedAt: time.Now(), Snapshot: contract.ClusterSnapshot{
		PodList: []contract.PodInfo{{Namespace: "ns", Name: "p", Owner: "Deployment/x", CPUMilli: 1, MemMi: 2}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pod, err := bson.Raw(raw).LookupErr("snapshot", "podlist", "0")
	if err != nil {
		t.Fatalf("snapshot.podlist missing: %v", err)
	}
	for _, k := range []string{"namespace", "name", "owner", "cpumilli", "memmi"} {
		if _, err := pod.Document().LookupErr(k); err != nil {
			t.Errorf("pod field %q missing: %v", k, err)
		}
	}
}
