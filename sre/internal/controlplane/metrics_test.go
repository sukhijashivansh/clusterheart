package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"clusterheart/sre/internal/contract"
)

func hostSnap(at time.Time, cpu float64, gpuUtil float64) contract.ClusterSnapshot {
	return contract.ClusterSnapshot{CollectedAt: at, Host: &contract.HostSnapshot{
		Hostname: "gpu-01", CPUCores: 8, CPUPercent: cpu, Load1: 2, Load5: 1.5,
		MemTotalMi: 1000, MemUsedMi: 250, NetRxKBps: 100, NetTxKBps: 50,
		Disks: []contract.HostDisk{{Mount: "/", TotalMi: 100, UsedMi: 40}, {Mount: "/data", TotalMi: 100, UsedMi: 70}},
		GPUs: []contract.HostGPU{
			{Index: 0, Vendor: "nvidia", Name: "NVIDIA A10G", UtilPercent: gpuUtil, MemUsedMi: 500, MemTotalMi: 1000, TempC: 60, PowerW: 100},
			{Index: 1, Vendor: "nvidia", Name: "NVIDIA A10G", UtilPercent: 10, MemUsedMi: 0, MemTotalMi: 1000},
		},
	}}
}

func TestServerMetricHistory(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	code, body := do(t, http.MethodPost, ts.URL+"/api/v1/clusters", adminTok, "",
		contract.RegisterRequest{TenantID: "acme", ClusterName: "gpu-01", Kind: contract.KindServer})
	if code != http.StatusCreated {
		t.Fatalf("register: %d %s", code, body)
	}
	var srv contract.RegisterResponse
	_ = json.Unmarshal(body, &srv)

	// Two samples in the same minute, one an hour ago, one in the future (dropped),
	// one with nonsense values (clamped).
	now := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Minute).Add(10 * time.Second)
	snaps := []contract.ClusterSnapshot{
		hostSnap(now.Add(-time.Hour), 90, 100),
		hostSnap(now, 20, 40),
		hostSnap(now.Add(30*time.Second), 40, 80),
		hostSnap(now.Add(time.Hour), 99, 99),
	}
	bad := hostSnap(now.Add(-30*time.Minute), 900, -5)
	bad.Host.MemUsedMi = 5000
	snaps = append(snaps, bad)
	batch := contract.TelemetryBatch{APIVersion: contract.APIVersion, Snapshots: snaps}
	if code, body := do(t, http.MethodPost, ts.URL+"/api/v1/telemetry", srv.AgentToken, srv.ClusterID, batch); code != http.StatusAccepted {
		t.Fatalf("telemetry: %d %s", code, body)
	}

	if code, _ := do(t, http.MethodGet, ts.URL+"/api/v1/clusters/"+srv.ClusterID+"/metrics?tenantId=other&range=1h", adminTok, "", nil); code != http.StatusNotFound {
		t.Fatalf("other tenant got %d", code)
	}

	code, body = do(t, http.MethodGet, ts.URL+"/api/v1/clusters/"+srv.ClusterID+"/metrics?tenantId=acme&range=24h", adminTok, "", nil)
	if code != http.StatusOK {
		t.Fatalf("metrics: %d %s", code, body)
	}
	var h MetricHistory
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatal(err)
	}
	if h.Range != "24h" || h.StepSec != 300 {
		t.Fatalf("range/step = %s/%d", h.Range, h.StepSec)
	}
	if len(h.Points) != 3 {
		t.Fatalf("want 3 buckets (hour ago, clamped, now), got %d: %s", len(h.Points), body)
	}
	last := h.Points[len(h.Points)-1]
	if last.CPU != 30 || last.CPUMax != 40 || last.Mem != 25 || last.Disk != 70 || last.Load != 1.5 || last.LoadMax != 2 || last.Rx != 100 {
		t.Fatalf("last bucket = %+v", last)
	}
	if mid := h.Points[1]; mid.CPU != 100 || mid.Mem != 100 {
		t.Fatalf("values not clamped: %+v", mid)
	}
	if len(h.GPUs) != 2 || h.GPUs[0].Name != "NVIDIA A10G" || h.GPUs[0].Index != 0 {
		t.Fatalf("gpus = %+v", h.GPUs)
	}
	g := h.GPUs[0].Points[len(h.GPUs[0].Points)-1]
	if g.Util != 60 || g.UtilMax != 80 || g.Mem != 50 || g.Temp != 60 || g.Power != 100 {
		t.Fatalf("gpu bucket = %+v", g)
	}
	if h.GPUs[0].Points[1].Util != 0 {
		t.Fatalf("negative GPU util not clamped: %+v", h.GPUs[0].Points[1])
	}

	// 1h uses 30s buckets, so the two samples 30s apart stay separate points.
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/clusters/"+srv.ClusterID+"/metrics?tenantId=acme&range=1h", adminTok, "", nil)
	_ = json.Unmarshal(body, &h)
	if h.StepSec != 30 || len(h.Points) != 3 {
		t.Fatalf("1h: step %d, %d points", h.StepSec, len(h.Points))
	}

	// A zoomed window (from/to in Unix ms) gets the finest step that fits.
	zf, zt := now.Add(-35*time.Minute).UnixMilli(), now.Add(-25*time.Minute).UnixMilli()
	_, body = do(t, http.MethodGet, ts.URL+"/api/v1/clusters/"+srv.ClusterID+"/metrics?tenantId=acme&from="+strconv.FormatInt(zf, 10)+"&to="+strconv.FormatInt(zt, 10), adminTok, "", nil)
	h = MetricHistory{}
	_ = json.Unmarshal(body, &h)
	if h.Range != "custom" || h.StepSec != 30 || len(h.Points) != 1 || h.From.UnixMilli() != zf {
		t.Fatalf("custom: range %s step %d, %d points, from %v", h.Range, h.StepSec, len(h.Points), h.From)
	}

	// Deleting the server drops its history.
	if code, _ := do(t, http.MethodDelete, ts.URL+"/api/v1/clusters/"+srv.ClusterID+"?tenantId=acme", adminTok, "", nil); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
}

// TestMongoMetricHistory runs the same bucketing against a real MongoDB
// (MONGO_TEST_URI), so the aggregation pipeline matches the in-memory one.
func TestMongoMetricHistory(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("MONGO_TEST_URI not set; skipping Mongo integration test")
	}
	ctx := context.Background()
	db, err := ConnectMongo(ctx, uri, "chtest_metrics_"+time.Now().Format("150405_000000"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Drop(context.Background()) })
	store, err := NewMongoStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	ms, ok := store.(MetricsStore)
	if !ok {
		t.Fatal("history collections were not set up")
	}
	specs, err := db.ListCollectionSpecifications(ctx, map[string]any{"name": serverMetricsColl})
	if err != nil || len(specs) != 1 || specs[0].Type != "timeseries" {
		t.Fatalf("server_metrics is not a time-series collection: %+v %v", specs, err)
	}
	// Second start must be a no-op.
	if _, err := NewMongoStore(ctx, db); err != nil {
		t.Fatalf("restart: %v", err)
	}

	now := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Minute).Add(10 * time.Second)
	snaps := []contract.ClusterSnapshot{hostSnap(now.Add(-time.Hour), 90, 100), hostSnap(now, 20, 40), hostSnap(now.Add(30*time.Second), 40, 80)}
	hosts, gpus := metricPoints(snaps, now.Add(time.Minute))
	if err := ms.AppendHostMetrics(ctx, "clu_a", hosts, gpus); err != nil {
		t.Fatal(err)
	}
	other, _ := metricPoints([]contract.ClusterSnapshot{hostSnap(now, 99, 99)}, now.Add(time.Minute))
	if err := ms.AppendHostMetrics(ctx, "clu_b", other, nil); err != nil {
		t.Fatal(err)
	}

	from, to := now.Add(-24*time.Hour), now.Add(time.Minute)
	gotH, gotG, err := ms.HostHistory(ctx, "clu_a", from, to, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	wantH, wantG := bucketHosts(hosts, from, to, 5*time.Minute), bucketGPUs(gpus, from, to, 5*time.Minute)
	if len(gotH) != len(wantH) || len(gotG) != len(wantG) {
		t.Fatalf("mongo %d/%d buckets, memory %d/%d", len(gotH), len(gotG), len(wantH), len(wantG))
	}
	for i := range wantH {
		if !gotH[i].T.Equal(wantH[i].T) || gotH[i].CPU != wantH[i].CPU || gotH[i].CPUMax != wantH[i].CPUMax || gotH[i].Disk != wantH[i].Disk || gotH[i].LoadMax != wantH[i].LoadMax {
			t.Fatalf("bucket %d: mongo %+v memory %+v", i, gotH[i], wantH[i])
		}
	}
	for i := range wantG {
		if gotG[i].Index != wantG[i].Index || gotG[i].Name != wantG[i].Name || len(gotG[i].Points) != len(wantG[i].Points) {
			t.Fatalf("gpu %d: mongo %+v memory %+v", i, gotG[i], wantG[i])
		}
		for j := range wantG[i].Points {
			if gotG[i].Points[j] != wantG[i].Points[j] {
				t.Fatalf("gpu %d bucket %d: mongo %+v memory %+v", i, j, gotG[i].Points[j], wantG[i].Points[j])
			}
		}
	}
	if err := ms.DeleteMetrics(ctx, "clu_a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if h, _, _ := ms.HostHistory(ctx, "clu_a", from, to, 5*time.Minute); len(h) != 0 {
		t.Fatalf("history left after delete: %d", len(h))
	}
	if h, _, _ := ms.HostHistory(ctx, "clu_b", from, to, 5*time.Minute); len(h) != 1 {
		t.Fatalf("other server's history touched: %d", len(h))
	}
}

func TestCustomWindowAndStep(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ms := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	if _, _, ok := customWindow("x", "1", now); ok {
		t.Fatal("bad input accepted")
	}
	if _, _, ok := customWindow(ms(now), ms(now.Add(-time.Minute)), now); ok {
		t.Fatal("to before from accepted")
	}
	// A 1-minute window widens to 5 minutes, kept inside "now".
	f, to, ok := customWindow(ms(now.Add(-time.Minute)), ms(now), now)
	if !ok || to != now || to.Sub(f) != minCustomSpan {
		t.Fatalf("widen: %v %v %v", f, to, ok)
	}
	// Windows reaching past retention or into the future are clamped.
	f, to, _ = customWindow(ms(now.Add(-30*24*time.Hour)), ms(now.Add(time.Hour)), now)
	if to != now || f != now.Add(-metricRetention) {
		t.Fatalf("clamp: %v %v", f, to)
	}
	for span, want := range map[time.Duration]time.Duration{
		10 * time.Minute: 30 * time.Second, 3 * time.Hour: 30 * time.Second, 4 * time.Hour: time.Minute,
		24 * time.Hour: 5 * time.Minute, 7 * 24 * time.Hour: 30 * time.Minute,
	} {
		if got := stepFor(span); got != want {
			t.Errorf("stepFor(%v) = %v, want %v", span, got, want)
		}
	}
}
