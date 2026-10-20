// history.go - MongoDB-backed usage history and the rightsizing analysis built
// on top of it. A background sampler records per-workload CPU/memory usage
// every sampleInterval; a TTL index prunes anything older than historyRetention
// so we keep a rolling window a bit larger than the analysis window. The
// rightsizing endpoint aggregates the last minWindow of samples and recommends
// downsize/upsize based on the p95 and peak per-pod usage vs the request -
// which, unlike a single snapshot, doesn't get fooled by spiky workloads.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// sampleInterval is how often usage is recorded. Overridable via SAMPLE_INTERVAL
// (a Go duration) - mainly to exercise the feature quickly in tests.
var sampleInterval = 30 * time.Second

// minWindow is how much history must exist before the analysis emits an
// actionable verdict ("6 hours at least"). Overridable via RIGHTSIZE_MIN_WINDOW
// (a Go duration, e.g. "10m") so the feature can be exercised without waiting.
var minWindow = 6 * time.Hour

// usageSample is one per-workload reading. Usage is normalised per pod so that
// replica changes (e.g. an HPA scaling out) don't distort the per-pod request
// recommendation. Sent to the account server as JSON; the server stamps the
// user and timestamp itself.
type usageSample struct {
	Namespace  string `json:"ns"`
	Workload   string `json:"workload"`
	OwnerKind  string `json:"ownerKind"`
	Replicas   int    `json:"replicas"`
	CPUPerPod  int64  `json:"cpuPerPod"` // milli-cores
	MemPerPod  int64  `json:"memPerPod"` // bytes
	CPURequest int64  `json:"cpuReq"`    // milli-cores, per pod
	MemRequest int64  `json:"memReq"`    // bytes, per pod
}

// history talks to the account server's usage API using the signed-in user's
// session token. The dashboard never holds a database credential.
type history struct {
	server string
	token  string
	client *http.Client
	ready  atomic.Bool
}

// initHistory connects to MongoDB, sets up indexes, and starts the sampler.
// A failed connection is non-fatal: the dashboard still works, the rightsizing
// tab just reports that history is unavailable.
func (s *server) initHistory() {
	if d := os.Getenv("RIGHTSIZE_MIN_WINDOW"); d != "" {
		if parsed, err := time.ParseDuration(d); err == nil && parsed > 0 {
			minWindow = parsed
		}
	}
	if d := os.Getenv("SAMPLE_INTERVAL"); d != "" {
		if parsed, err := time.ParseDuration(d); err == nil && parsed > 0 {
			sampleInterval = parsed
		}
	}

	// Usage history now goes through the account server (authenticated with the
	// user's session token), so no database credential lives in the app. Electron
	// passes the server URL and token in the environment when it launches us.
	server := strings.TrimRight(os.Getenv("CLUSTERHEART_SERVER"), "/")
	token := os.Getenv("CLUSTERHEART_TOKEN")
	s.hist = &history{server: server, token: token, client: &http.Client{Timeout: 20 * time.Second}}
	if server == "" || token == "" {
		fmt.Println("history: no account session - rightsizing history disabled (sign in to enable)")
		return
	}
	s.hist.ready.Store(true)
	fmt.Printf("history: using %s (sampling every %s, %s window)\n", server, sampleInterval, minWindow)
	go s.sampleLoop()
}

// postSamples ships a batch of readings to the account server.
func (h *history) postSamples(ctx context.Context, samples []usageSample) {
	body, _ := json.Marshal(map[string]any{"samples": samples})
	req, err := http.NewRequestWithContext(ctx, "POST", h.server+"/api/usage/ingest", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

func (s *server) sampleLoop() {
	s.sampleOnce() // first sample right away
	t := time.NewTicker(sampleInterval)
	defer t.Stop()
	for range t.C {
		s.sampleOnce()
	}
}

// sampleOnce records one per-workload snapshot of usage into MongoDB.
func (s *server) sampleOnce() {
	if s.hist == nil || !s.hist.ready.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	k8s, mc, _ := s.clients()
	pl, err := k8s.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}

	// pod usage keyed by ns/pod (summed across containers)
	type use struct{ cpu, mem int64 }
	usage := map[string]use{}
	if pm, err := mc.MetricsV1beta1().PodMetricses("").List(ctx, metav1.ListOptions{}); err == nil {
		for _, m := range pm.Items {
			var u use
			for _, c := range m.Containers {
				u.cpu += c.Usage.Cpu().MilliValue()
				u.mem += c.Usage.Memory().Value()
			}
			usage[m.Namespace+"/"+m.Name] = u
		}
	}

	// Map each CronJob-created Job back to its parent CronJob, so every run of a
	// CronJob rolls up to a single workload instead of one row per scheduled Job
	// (e.g. dms-tata-report-worker-29873285 / -29873290 -> dms-tata-report-worker).
	jobToCron := map[string]string{}
	if jl, err := k8s.BatchV1().Jobs("").List(ctx, metav1.ListOptions{}); err == nil {
		for i := range jl.Items {
			j := &jl.Items[i]
			for _, o := range j.OwnerReferences {
				if o.Controller != nil && *o.Controller && o.Kind == "CronJob" {
					jobToCron[j.Namespace+"/"+j.Name] = o.Name
				}
			}
		}
	}

	// aggregate active pods per workload
	type agg struct {
		ownerKind          string
		count              int
		cpuUse, memUse     int64
		cpuReq, memReq     int64 // per-pod request (from representative pod)
		haveReq            bool
	}
	groups := map[string]*agg{}
	for i := range pl.Items {
		p := &pl.Items[i]
		if p.Status.Phase != corev1.PodRunning && p.Status.Phase != corev1.PodPending {
			continue
		}
		kind, owner := workloadOwner(p)
		// Collapse a CronJob's Job onto the CronJob itself.
		if kind == "Job" {
			if cron, ok := jobToCron[p.Namespace+"/"+owner]; ok {
				kind, owner = "CronJob", cron
			}
		}
		name := goWorkloadName(kind, owner, p.Name)
		key := p.Namespace + "|" + name
		g := groups[key]
		if g == nil {
			g = &agg{ownerKind: displayKind(kind)}
			groups[key] = g
		}
		g.count++
		u := usage[p.Namespace+"/"+p.Name]
		g.cpuUse += u.cpu
		g.memUse += u.mem
		if !g.haveReq {
			cpuReq, memReq := podRequests(p)
			g.cpuReq, g.memReq = cpuReq, memReq
			g.haveReq = true
		}
	}

	docs := make([]usageSample, 0, len(groups))
	for key, g := range groups {
		if g.count == 0 {
			continue
		}
		parts := strings.SplitN(key, "|", 2)
		docs = append(docs, usageSample{
			Namespace:  parts[0],
			Workload:   parts[1],
			OwnerKind:  g.ownerKind,
			Replicas:   g.count,
			CPUPerPod:  g.cpuUse / int64(g.count),
			MemPerPod:  g.memUse / int64(g.count),
			CPURequest: g.cpuReq,
			MemRequest: g.memReq,
		})
	}
	if len(docs) == 0 {
		return
	}
	s.hist.postSamples(ctx, docs)
}

// podRequests sums a pod's container CPU (milli) and memory (bytes) requests.
func podRequests(p *corev1.Pod) (cpu, mem int64) {
	for _, c := range p.Spec.Containers {
		if q := c.Resources.Requests.Cpu(); q != nil {
			cpu += q.MilliValue()
		}
		if q := c.Resources.Requests.Memory(); q != nil {
			mem += q.Value()
		}
	}
	return
}

// workloadOwner returns the controlling owner kind/name of a pod (one hop; a
// Deployment pod reports its ReplicaSet here - goWorkloadName collapses it).
func workloadOwner(p *corev1.Pod) (kind, name string) {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller {
			return o.Kind, o.Name
		}
	}
	if len(p.OwnerReferences) > 0 {
		return p.OwnerReferences[0].Kind, p.OwnerReferences[0].Name
	}
	return "", ""
}

// goWorkloadName mirrors the frontend workloadName(): a ReplicaSet name has its
// trailing pod-template hash stripped to yield the Deployment name.
func goWorkloadName(kind, ownerName, podName string) string {
	if kind == "" || ownerName == "" {
		return podName
	}
	if kind == "ReplicaSet" {
		if i := strings.LastIndex(ownerName, "-"); i > 0 {
			return ownerName[:i]
		}
	}
	return ownerName
}

// displayKind normalises the owner kind for display: a ReplicaSet is almost
// always a Deployment's, so show that.
func displayKind(kind string) string {
	if kind == "ReplicaSet" {
		return "Deployment"
	}
	if kind == "" {
		return "Pod"
	}
	return kind
}

// RightsizeRow is one workload's history-based analysis for the frontend.
type RightsizeRow struct {
	Namespace  string `json:"namespace"`
	Workload   string `json:"workload"`
	OwnerKind  string `json:"ownerKind"`
	Replicas   int    `json:"replicas"`
	CPURequest int64  `json:"cpuRequest"`
	CPUP95     int64  `json:"cpuP95"`
	CPUPeak    int64  `json:"cpuPeak"`
	MemRequest int64  `json:"memRequest"`
	MemP95     int64  `json:"memP95"`
	MemPeak    int64  `json:"memPeak"`
	Samples    int    `json:"samples"`
	WindowMins int    `json:"windowMins"`
	Verdict    string `json:"verdict"`
	VerdictCls string `json:"verdictCls"`
	SuggestCPU int64  `json:"suggestCpu"` // milli, 0 = no change
	SuggestMem int64  `json:"suggestMem"` // bytes, 0 = no change
}

// handleRightsizing aggregates the last minWindow of samples per workload and
// returns a recommendation. ?ns=<namespace> scopes it; ns=ALL/empty = all.
func (s *server) handleRightsizing(w http.ResponseWriter, r *http.Request) {
	if s.hist == nil || !s.hist.ready.Load() {
		writeJSON(w, 200, map[string]any{
			"ready":      false,
			"reason":     "Usage history unavailable - sign in to your Clusterheart account to enable rightsizing.",
			"windowMins": int(minWindow.Minutes()),
			"rows":       []RightsizeRow{},
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	raw, err := s.hist.fetchAggregate(ctx, r.URL.Query().Get("ns"), int(minWindow.Minutes()))
	if err != nil {
		writeErr(w, 502, "history unavailable: "+err.Error())
		return
	}

	rows := make([]RightsizeRow, 0, len(raw))
	for _, g := range raw {
		row := RightsizeRow{
			Namespace: g.NS, Workload: g.Workload, OwnerKind: g.OwnerKind,
			Replicas: g.Replicas, CPURequest: g.CPUReq, MemRequest: g.MemReq,
			CPUPeak: g.CPUPeak, MemPeak: g.MemPeak, Samples: g.Samples,
			WindowMins: int(g.Last.Sub(g.First).Minutes()),
		}
		if len(g.CPUP95) > 0 {
			row.CPUP95 = int64(g.CPUP95[0])
		}
		if len(g.MemP95) > 0 {
			row.MemP95 = int64(g.MemP95[0])
		}
		coverage := g.Last.Sub(g.First)
		row.Verdict, row.VerdictCls, row.SuggestCPU, row.SuggestMem = rightsizeVerdict(row, coverage)
		rows = append(rows, row)
	}

	writeJSON(w, 200, map[string]any{
		"ready":      true,
		"windowMins": int(minWindow.Minutes()),
		"rows":       rows,
	})
}

// aggRow mirrors one grouped row returned by the account server's aggregate API.
type aggRow struct {
	NS        string    `json:"ns"`
	Workload  string    `json:"workload"`
	CPUP95    []float64 `json:"cpuP95"`
	MemP95    []float64 `json:"memP95"`
	CPUPeak   int64     `json:"cpuPeak"`
	MemPeak   int64     `json:"memPeak"`
	CPUReq    int64     `json:"cpuReq"`
	MemReq    int64     `json:"memReq"`
	Replicas  int       `json:"replicas"`
	OwnerKind string    `json:"ownerKind"`
	Samples   int       `json:"samples"`
	First     time.Time `json:"first"`
	Last      time.Time `json:"last"`
}

// fetchAggregate asks the account server for the per-workload window aggregation.
func (h *history) fetchAggregate(ctx context.Context, ns string, windowMins int) ([]aggRow, error) {
	u := fmt.Sprintf("%s/api/usage/aggregate?ns=%s&windowMins=%d", h.server, url.QueryEscape(ns), windowMins)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("server returned %d", resp.StatusCode)
	}
	var out struct {
		Rows []aggRow `json:"rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Rows, nil
}

// rightsizeVerdict decides downsize/upsize/ok from the windowed stats. It waits
// for a full window of data before making an actionable call, and uses p95 (not
// the instantaneous reading) so a spiky workload isn't told to downsize.
func rightsizeVerdict(r RightsizeRow, coverage time.Duration) (verdict, cls string, suggestCPU, suggestMem int64) {
	// Need most of a full window (and more than a couple of samples) before
	// advising - "6 hours at least, then suggest".
	if r.Samples < 3 || coverage < time.Duration(float64(minWindow)*0.9) {
		return "LEARNING", "verdict-none", 0, 0
	}
	if r.CPURequest == 0 && r.MemRequest == 0 {
		return "NO REQ", "verdict-none", 0, 0
	}

	cpuP95Pct := pctOf(r.CPUP95, r.CPURequest)
	memP95Pct := pctOf(r.MemP95, r.MemRequest)
	cpuPeakPct := pctOf(r.CPUPeak, r.CPURequest)
	memPeakPct := pctOf(r.MemPeak, r.MemRequest)

	// Under-provisioned: p95 near the request or peak over it → throttling/OOM risk.
	if (cpuP95Pct >= 0 && cpuP95Pct > 90) || (cpuPeakPct >= 0 && cpuPeakPct >= 100) {
		return "UPSIZE CPU", "verdict-up", 0, 0
	}
	if (memP95Pct >= 0 && memP95Pct > 90) || (memPeakPct >= 0 && memPeakPct >= 100) {
		return "UPSIZE MEM", "verdict-up", 0, 0
	}

	// Downsize hints are only useful where the user can act. Platform-managed
	// (EKS) namespaces can't be patched from the UI, so don't flag them.
	if strings.HasPrefix(r.Namespace, "kube-") {
		return "OK", "verdict-ok", 0, 0
	}

	// Over-provisioned: even the p95 sits well below the request. Suggest a new
	// request sized to peak + 20% headroom (never above the current request).
	cpuDown := cpuP95Pct >= 0 && cpuP95Pct < 40
	memDown := memP95Pct >= 0 && memP95Pct < 40
	if cpuDown {
		suggestCPU = headroom(r.CPUPeak, r.CPURequest)
	}
	if memDown {
		suggestMem = headroom(r.MemPeak, r.MemRequest)
	}
	switch {
	case cpuDown && memDown:
		return "DOWNSIZE", "verdict-down", suggestCPU, suggestMem
	case cpuDown:
		return "DOWNSIZE CPU", "verdict-down", suggestCPU, 0
	case memDown:
		return "DOWNSIZE MEM", "verdict-down", 0, suggestMem
	}
	return "OK", "verdict-ok", 0, 0
}

// pctOf returns used/req as a percentage, or -1 when there's no request.
func pctOf(used, req int64) float64 {
	if req <= 0 {
		return -1
	}
	return float64(used) / float64(req) * 100
}

// headroom sizes a new request at peak + 20%, capped at the current request.
func headroom(peak, req int64) int64 {
	v := peak + peak/5
	if v <= 0 || v >= req {
		return 0
	}
	return v
}
