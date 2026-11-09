package controlplane

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"clusterheart/sre/internal/contract"
	"clusterheart/sre/internal/probe"
)

// Latency checks: a person picks a pod in the dashboard, the control plane
// queues the check for that cluster's agent, the agent times GET requests to
// the pod and posts the timings back. Everything lives in memory for minutes.
const (
	probePickupTimeout = 45 * time.Second // agent never took it: probably an old agent
	probeRunTimeout    = 90 * time.Second // agent took it but never answered
	probeKeep          = 10 * time.Minute // finished results stay readable this long
	maxPendingProbes   = 3                // per cluster
)

type probeEntry struct {
	id, tenant, cluster string
	target              contract.ProbeTarget
	created, taken      time.Time
	done                bool
	doneAt              time.Time
	result              *contract.ProbeResult
	err                 string
}

// probeStore holds both kinds of on-demand agent work: latency checks (m) and
// log reads (logs, see logs.go). One lock, one agent poll.
type probeStore struct {
	mu   sync.Mutex
	m    map[string]*probeEntry
	logs map[string]*logEntry
}

func newProbeStore() *probeStore {
	return &probeStore{m: map[string]*probeEntry{}, logs: map[string]*logEntry{}}
}

// expire fails checks the agent never picked up or never answered, and drops
// old results. Callers hold mu.
func (ps *probeStore) expire(now time.Time) {
	for id, e := range ps.m {
		switch {
		case e.done && now.Sub(e.doneAt) > probeKeep:
			delete(ps.m, id)
		case !e.done && e.taken.IsZero() && now.Sub(e.created) > probePickupTimeout:
			e.done, e.doneAt, e.err = true, now, "The agent didn't pick this up. Update the agent to 0.3.0 or later and check that it is running."
		case !e.done && !e.taken.IsZero() && now.Sub(e.taken) > probeRunTimeout:
			e.done, e.doneAt, e.err = true, now, "The agent started the check but didn't answer in time."
		}
	}
	ps.expireLogs(now)
}

// k8sName is a DNS-1123 name (namespaces and pod names).
var k8sName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

func validProbeTarget(t *contract.ProbeTarget) string {
	t.Namespace, t.Pod = strings.TrimSpace(t.Namespace), strings.TrimSpace(t.Pod)
	if !k8sName.MatchString(t.Namespace) || !k8sName.MatchString(t.Pod) {
		return "pick a pod"
	}
	if t.Port < 0 || t.Port > 65535 {
		return "port must be between 1 and 65535 (or empty for the pod's own port)"
	}
	if t.Scheme == "" {
		t.Scheme = "http"
	}
	if t.Scheme != "http" && t.Scheme != "https" {
		return "scheme must be http or https"
	}
	t.Path = strings.TrimSpace(t.Path)
	if t.Path == "" {
		t.Path = "/"
	}
	if !strings.HasPrefix(t.Path, "/") || len(t.Path) > 512 || strings.ContainsAny(t.Path, " \t\r\n#") {
		return "path must start with / and have no spaces"
	}
	t.Samples = min(max(t.Samples, 1), probe.MaxSamples)
	return ""
}

// handleCreateProbe queues a latency check:
// POST /api/v1/clusters/{id}/probes?tenantId=... {namespace,pod,port,scheme,path,samples}
func (s *Server) handleCreateProbe(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	c, err := s.store.GetCluster(r.Context(), r.PathValue("id"))
	if err != nil || tenantID == "" || c.TenantID != tenantID {
		writeErr(w, http.StatusNotFound, "cluster not found")
		return
	}
	if kindOf(c) != contract.KindKubernetes {
		writeErr(w, http.StatusBadRequest, "latency checks are for Kubernetes clusters")
		return
	}
	var t contract.ProbeTarget
	if err := decode(r, &t); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if msg := validProbeTarget(&t); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	now := s.now()
	ps := s.probes
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.expire(now)
	pending := 0
	for _, e := range ps.m {
		if e.cluster == c.ID && !e.done {
			pending++
		}
	}
	if pending >= maxPendingProbes {
		writeErr(w, http.StatusTooManyRequests, "a few checks are already running on this cluster; wait for them to finish")
		return
	}
	e := &probeEntry{id: newID("prb"), tenant: tenantID, cluster: c.ID, target: t, created: now}
	ps.m[e.id] = e
	writeJSON(w, http.StatusCreated, map[string]any{"id": e.id})
}

// handleGetProbe returns a check's state: GET /api/v1/probes/{id}?tenantId=...
func (s *Server) handleGetProbe(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	ps := s.probes
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.expire(s.now())
	e := ps.m[r.PathValue("id")]
	if e == nil || tenantID == "" || e.tenant != tenantID {
		writeErr(w, http.StatusNotFound, "check not found or expired")
		return
	}
	if !e.done {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "started": !e.taken.IsZero()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "result": e.result, "error": e.err, "target": e.target})
}

// handleAgentProbes hands the authenticated cluster's queued checks to its
// agent, once each: GET /api/v1/agents/probes
func (s *Server) handleAgentProbes(w http.ResponseWriter, r *http.Request) {
	c, err := clusterFromCtx(r.Context())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	now := s.now()
	ps := s.probes
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.expire(now)
	out := []contract.ProbeRequest{}
	for _, e := range ps.m {
		if e.cluster == c.ID && !e.done && e.taken.IsZero() {
			e.taken = now
			out = append(out, contract.ProbeRequest{ID: e.id, Target: e.target})
		}
	}
	writeJSON(w, http.StatusOK, contract.ProbeRequestsResponse{Requests: out, Logs: ps.takeLogs(c.ID, now)})
}

// handleAgentProbeResult takes the agent's timings for one of its own checks:
// POST /api/v1/agents/probes/{id}
func (s *Server) handleAgentProbeResult(w http.ResponseWriter, r *http.Request) {
	c, err := clusterFromCtx(r.Context())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var res contract.ProbeResult
	if err := decode(r, &res); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if len(res.Samples) > probe.MaxSamples {
		res.Samples = res.Samples[:probe.MaxSamples]
	}
	clip := func(s string, n int) string {
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	res.Error, res.URL = clip(res.Error, 300), clip(res.URL, 600)
	for i := range res.Samples {
		res.Samples[i].Err = clip(res.Samples[i].Err, 200)
	}
	ps := s.probes
	ps.mu.Lock()
	defer ps.mu.Unlock()
	e := ps.m[r.PathValue("id")]
	if e == nil || e.cluster != c.ID || e.taken.IsZero() {
		writeErr(w, http.StatusNotFound, "check not found")
		return
	}
	if !e.done {
		e.done, e.doneAt, e.result = true, s.now(), &res
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
