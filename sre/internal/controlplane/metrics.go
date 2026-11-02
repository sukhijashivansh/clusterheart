package controlplane

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"clusterheart/sre/internal/contract"
)

// Server metric history. Every host snapshot becomes one point in a week-long
// history, stored apart from the 24h raw snapshots: system metrics in one
// MongoDB time-series collection (server_metrics), per-GPU metrics in another
// (gpu_metrics), each bucketed by server (and GPU). Charts read it downsampled.

// metricRetention keeps a full 7-day window plus a day of margin.
const metricRetention = 8 * 24 * time.Hour

// maxGPUsPerHost bounds GPU points per snapshot (8-GPU boxes are common).
const maxGPUsPerHost = 16

// HostPoint is one sample of a server's system metrics. Percentages are 0-100.
type HostPoint struct {
	At    time.Time
	CPU   float64 // % of all cores
	Mem   float64 // % of RAM used
	Swap  float64 // % of swap used
	Disk  float64 // % used of the fullest disk
	Load1 float64
	Load5 float64
	Rx    float64 // KB/s received
	Tx    float64 // KB/s sent
}

// GPUPoint is one sample of one GPU.
type GPUPoint struct {
	At    time.Time
	Index int
	UUID  string
	Name  string
	Util  float64 // %
	Mem   float64 // % of GPU memory used
	Temp  float64 // °C, 0 = not reported
	Power float64 // W, 0 = not reported
}

// HostBucket is the average (and peak) of the points in one time bucket.
type HostBucket struct {
	T       time.Time `json:"t"`
	CPU     float64   `json:"cpu"`
	CPUMax  float64   `json:"cpuMax"`
	Mem     float64   `json:"mem"`
	MemMax  float64   `json:"memMax"`
	Swap    float64   `json:"swap"`
	Disk    float64   `json:"disk"`
	Load    float64   `json:"load"` // 5-minute load average
	LoadMax float64   `json:"loadMax"`
	Rx      float64   `json:"rx"`
	RxMax   float64   `json:"rxMax"`
	Tx      float64   `json:"tx"`
	TxMax   float64   `json:"txMax"`
}

// GPUBucket is one GPU's average (and peak) in one time bucket.
type GPUBucket struct {
	T       time.Time `json:"t"`
	Util    float64   `json:"util"`
	UtilMax float64   `json:"utilMax"`
	Mem     float64   `json:"mem"`
	MemMax  float64   `json:"memMax"`
	Temp    float64   `json:"temp"`
	TempMax float64   `json:"tempMax"`
	Power   float64   `json:"power"`
}

// GPUHistory is one GPU's buckets.
type GPUHistory struct {
	Index  int         `json:"index"`
	Name   string      `json:"name"`
	Points []GPUBucket `json:"points"`
}

// MetricHistory is a server's downsampled history over a range.
type MetricHistory struct {
	Range   string       `json:"range"`
	StepSec int          `json:"stepSec"`
	From    time.Time    `json:"from"`
	To      time.Time    `json:"to"`
	Points  []HostBucket `json:"points"`
	GPUs    []GPUHistory `json:"gpus"`
}

// MetricsStore keeps the per-server history. memStore and mongoStore both
// implement it.
type MetricsStore interface {
	AppendHostMetrics(ctx context.Context, clusterID string, hosts []HostPoint, gpus []GPUPoint) error
	HostHistory(ctx context.Context, clusterID string, from, to time.Time, step time.Duration) ([]HostBucket, []GPUHistory, error)
	DeleteMetrics(ctx context.Context, clusterID string) error
}

// historyRanges maps a chart range to its bucket size (~30-340 points each).
// Short ranges use 30s buckets, the agent's default sample interval, so every
// raw sample is its own point and short spikes are not averaged away.
var historyRanges = map[string]struct{ span, step time.Duration }{
	"15m": {15 * time.Minute, 30 * time.Second},
	"1h":  {time.Hour, 30 * time.Second},
	"6h":  {6 * time.Hour, time.Minute},
	"24h": {24 * time.Hour, 5 * time.Minute},
	"7d":  {7 * 24 * time.Hour, 30 * time.Minute},
}

// customSteps are the bucket sizes a custom (zoomed) window can use; the
// smallest that keeps the window under maxCustomPoints wins.
var customSteps = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute,
	10 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour}

const (
	maxCustomPoints = 400
	minCustomSpan   = 5 * time.Minute
)

// stepFor picks the finest bucket that keeps span under maxCustomPoints.
func stepFor(span time.Duration) time.Duration {
	for _, st := range customSteps {
		if span/st <= maxCustomPoints {
			return st
		}
	}
	return customSteps[len(customSteps)-1]
}

// customWindow parses from/to (Unix milliseconds) for a zoomed-in chart. It
// clamps the window into the retention period and widens it to minCustomSpan.
func customWindow(fromS, toS string, now time.Time) (from, to time.Time, ok bool) {
	f, err1 := strconv.ParseInt(fromS, 10, 64)
	t, err2 := strconv.ParseInt(toS, 10, 64)
	if err1 != nil || err2 != nil || t <= f {
		return time.Time{}, time.Time{}, false
	}
	from, to = time.UnixMilli(f).UTC(), time.UnixMilli(t).UTC()
	if to.After(now) {
		to = now
	}
	if earliest := now.Add(-metricRetention); from.Before(earliest) {
		from = earliest
	}
	if to.Sub(from) < minCustomSpan {
		mid := from.Add(to.Sub(from) / 2)
		from, to = mid.Add(-minCustomSpan/2), mid.Add(minCustomSpan/2)
		if to.After(now) {
			from, to = now.Add(-minCustomSpan), now
		}
	}
	return from, to, true
}

func clampPct(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	return math.Min(v, 100)
}

func nonNeg(v float64, ceiling float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	return math.Min(v, ceiling)
}

func pctOf(used, total int64) float64 {
	if total <= 0 || used <= 0 {
		return 0
	}
	return clampPct(float64(used) / float64(total) * 100)
}

// metricPoints turns host snapshots into history points. Values are clamped so
// a buggy agent cannot skew charts; samples from the future or older than the
// retention window are dropped.
func metricPoints(snaps []contract.ClusterSnapshot, now time.Time) ([]HostPoint, []GPUPoint) {
	var hosts []HostPoint
	var gpus []GPUPoint
	for _, sn := range snaps {
		h := sn.Host
		if h == nil {
			continue
		}
		at := sn.CollectedAt.UTC()
		if at.IsZero() {
			at = now
		}
		if at.After(now.Add(5*time.Minute)) || now.Sub(at) > metricRetention {
			continue
		}
		p := HostPoint{At: at, CPU: clampPct(h.CPUPercent), Mem: pctOf(h.MemUsedMi, h.MemTotalMi), Swap: pctOf(h.SwapUsedMi, h.SwapTotalMi),
			Load1: nonNeg(h.Load1, 1e4), Load5: nonNeg(h.Load5, 1e4), Rx: nonNeg(h.NetRxKBps, 1e8), Tx: nonNeg(h.NetTxKBps, 1e8)}
		for _, d := range h.Disks {
			p.Disk = math.Max(p.Disk, pctOf(d.UsedMi, d.TotalMi))
		}
		hosts = append(hosts, p)
		for i, g := range h.GPUs {
			if i >= maxGPUsPerHost {
				break
			}
			name := strings.TrimSpace(g.Name)
			if len(name) > 80 {
				name = name[:80]
			}
			uuid := g.UUID
			if len(uuid) > 80 {
				uuid = uuid[:80]
			}
			gpus = append(gpus, GPUPoint{At: at, Index: g.Index, UUID: uuid, Name: name, Util: clampPct(g.UtilPercent),
				Mem: pctOf(g.MemUsedMi, g.MemTotalMi), Temp: nonNeg(g.TempC, 200), Power: nonNeg(g.PowerW, 5000)})
		}
	}
	return hosts, gpus
}

// handleServerMetrics serves a server's history for charts:
// GET /api/v1/clusters/{id}/metrics?tenantId=...&range=15m|1h|6h|24h|7d
// or, for a zoomed-in window, &from=<unix ms>&to=<unix ms> (range "custom").
func (s *Server) handleServerMetrics(w http.ResponseWriter, r *http.Request) {
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
	if s.metrics == nil {
		writeErr(w, http.StatusServiceUnavailable, "metric history is not available")
		return
	}
	now := s.now().UTC()
	q := r.URL.Query()
	name := q.Get("range")
	var from, to time.Time
	var step time.Duration
	if f, t, ok := customWindow(q.Get("from"), q.Get("to"), now); ok {
		name, from, to, step = "custom", f, t, stepFor(t.Sub(f))
	} else {
		rg, ok := historyRanges[name]
		if !ok {
			name, rg = "24h", historyRanges["24h"]
		}
		from, to, step = now.Add(-rg.span), now, rg.step
	}
	hosts, gpus, err := s.metrics.HostHistory(r.Context(), c.ID, from, to, step)
	if err != nil {
		s.log.Error("metric history query failed", "clusterId", c.ID, "err", err)
		writeErr(w, http.StatusInternalServerError, "could not load metric history")
		return
	}
	if hosts == nil {
		hosts = []HostBucket{}
	}
	if gpus == nil {
		gpus = []GPUHistory{}
	}
	writeJSON(w, http.StatusOK, MetricHistory{Range: name, StepSec: int(step.Seconds()), From: from, To: to, Points: hosts, GPUs: gpus})
}

// --- in-memory implementation and the shared bucketing ---------------------

// maxMemPoints bounds the in-memory history per server (a week at 30s is ~20k).
const maxMemPoints = 25000

func bucketOf(t time.Time, step time.Duration) time.Time {
	ms := step.Milliseconds()
	return time.UnixMilli(t.UnixMilli() - t.UnixMilli()%ms).UTC()
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// bucketHosts averages points per bucket, oldest first.
func bucketHosts(pts []HostPoint, from, to time.Time, step time.Duration) []HostBucket {
	type acc struct {
		b HostBucket
		n float64
	}
	byT := map[int64]*acc{}
	for _, p := range pts {
		if p.At.Before(from) || !p.At.Before(to) {
			continue
		}
		t := bucketOf(p.At, step)
		a := byT[t.UnixMilli()]
		if a == nil {
			a = &acc{b: HostBucket{T: t}}
			byT[t.UnixMilli()] = a
		}
		a.n++
		b := &a.b
		b.CPU += p.CPU
		b.Mem += p.Mem
		b.Swap += p.Swap
		b.Disk += p.Disk
		b.Load += p.Load5
		b.Rx += p.Rx
		b.Tx += p.Tx
		b.CPUMax = math.Max(b.CPUMax, p.CPU)
		b.MemMax = math.Max(b.MemMax, p.Mem)
		b.LoadMax = math.Max(b.LoadMax, p.Load1)
		b.RxMax = math.Max(b.RxMax, p.Rx)
		b.TxMax = math.Max(b.TxMax, p.Tx)
	}
	out := make([]HostBucket, 0, len(byT))
	for _, a := range byT {
		b := a.b
		b.CPU, b.Mem, b.Swap, b.Disk = round2(b.CPU/a.n), round2(b.Mem/a.n), round2(b.Swap/a.n), round2(b.Disk/a.n)
		b.Load, b.Rx, b.Tx = round2(b.Load/a.n), round2(b.Rx/a.n), round2(b.Tx/a.n)
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	return out
}

// bucketGPUs averages GPU points per GPU and bucket.
func bucketGPUs(pts []GPUPoint, from, to time.Time, step time.Duration) []GPUHistory {
	type acc struct {
		b GPUBucket
		n float64
	}
	type gpu struct {
		name string
		byT  map[int64]*acc
	}
	byIdx := map[int]*gpu{}
	for _, p := range pts {
		if p.At.Before(from) || !p.At.Before(to) {
			continue
		}
		g := byIdx[p.Index]
		if g == nil {
			g = &gpu{byT: map[int64]*acc{}}
			byIdx[p.Index] = g
		}
		g.name = p.Name
		t := bucketOf(p.At, step)
		a := g.byT[t.UnixMilli()]
		if a == nil {
			a = &acc{b: GPUBucket{T: t}}
			g.byT[t.UnixMilli()] = a
		}
		a.n++
		b := &a.b
		b.Util += p.Util
		b.Mem += p.Mem
		b.Temp += p.Temp
		b.Power += p.Power
		b.UtilMax = math.Max(b.UtilMax, p.Util)
		b.MemMax = math.Max(b.MemMax, p.Mem)
		b.TempMax = math.Max(b.TempMax, p.Temp)
	}
	out := make([]GPUHistory, 0, len(byIdx))
	for idx, g := range byIdx {
		h := GPUHistory{Index: idx, Name: g.name}
		for _, a := range g.byT {
			b := a.b
			b.Util, b.Mem, b.Temp, b.Power = round2(b.Util/a.n), round2(b.Mem/a.n), round2(b.Temp/a.n), round2(b.Power/a.n)
			h.Points = append(h.Points, b)
		}
		sort.Slice(h.Points, func(i, j int) bool { return h.Points[i].T.Before(h.Points[j].T) })
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

func (m *memStore) AppendHostMetrics(_ context.Context, clusterID string, hosts []HostPoint, gpus []GPUPoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hostPts == nil {
		m.hostPts, m.gpuPts = map[string][]HostPoint{}, map[string][]GPUPoint{}
	}
	h := append(m.hostPts[clusterID], hosts...)
	if len(h) > maxMemPoints {
		h = h[len(h)-maxMemPoints:]
	}
	m.hostPts[clusterID] = h
	g := append(m.gpuPts[clusterID], gpus...)
	if len(g) > maxMemPoints*4 {
		g = g[len(g)-maxMemPoints*4:]
	}
	m.gpuPts[clusterID] = g
	return nil
}

func (m *memStore) HostHistory(_ context.Context, clusterID string, from, to time.Time, step time.Duration) ([]HostBucket, []GPUHistory, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return bucketHosts(m.hostPts[clusterID], from, to, step), bucketGPUs(m.gpuPts[clusterID], from, to, step), nil
}

func (m *memStore) DeleteMetrics(_ context.Context, clusterID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.hostPts, clusterID)
	delete(m.gpuPts, clusterID)
	return nil
}
