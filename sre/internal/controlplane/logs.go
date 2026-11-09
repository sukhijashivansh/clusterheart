package controlplane

import (
	"net/http"
	"strings"
	"time"

	"clusterheart/sre/internal/contract"
	"clusterheart/sre/internal/redact"
)

// On-demand logs: a person clicks a pod or job in the dashboard, the control
// plane queues the read for that cluster's agent (handed out on the same poll
// as latency checks), the agent reads the tail, redacts it and posts it back.
const (
	logPickupTimeout = 30 * time.Second
	logRunTimeout    = 40 * time.Second
	logKeep          = 2 * time.Minute
	maxPendingLogs   = 6 // per cluster
	maxLogTail       = 500
	defaultLogTail   = 200
	maxLogLine       = 2000
)

type logEntry struct {
	id, tenant, cluster string
	target              contract.LogTarget
	created, taken      time.Time
	done                bool
	doneAt              time.Time
	result              *contract.LogResult
	err                 string
}

// expireLogs mirrors expire for log reads. Callers hold mu.
func (ps *probeStore) expireLogs(now time.Time) {
	for id, e := range ps.logs {
		switch {
		case e.done && now.Sub(e.doneAt) > logKeep:
			delete(ps.logs, id)
		case !e.done && e.taken.IsZero() && now.Sub(e.created) > logPickupTimeout:
			e.done, e.doneAt, e.err = true, now, "The agent didn't pick this up. Update the agent to 0.4.0 or later and check that it is running."
		case !e.done && !e.taken.IsZero() && now.Sub(e.taken) > logRunTimeout:
			e.done, e.doneAt, e.err = true, now, "The agent started reading the log but didn't answer in time."
		}
	}
}

// takeLogs hands a cluster's queued log reads to its agent, once each.
// Callers hold mu.
func (ps *probeStore) takeLogs(cluster string, now time.Time) []contract.LogRequest {
	var out []contract.LogRequest
	for _, e := range ps.logs {
		if e.cluster == cluster && !e.done && e.taken.IsZero() {
			e.taken = now
			out = append(out, contract.LogRequest{ID: e.id, Target: e.target})
		}
	}
	return out
}

func validLogTarget(t *contract.LogTarget) string {
	t.Namespace, t.Pod, t.Job, t.Container = strings.TrimSpace(t.Namespace), strings.TrimSpace(t.Pod), strings.TrimSpace(t.Job), strings.TrimSpace(t.Container)
	if !k8sName.MatchString(t.Namespace) {
		return "pick a pod or job"
	}
	if (t.Pod == "") == (t.Job == "") {
		return "pick a pod or a job"
	}
	if (t.Pod != "" && !k8sName.MatchString(t.Pod)) || (t.Job != "" && !k8sName.MatchString(t.Job)) {
		return "pick a pod or job"
	}
	if t.Container != "" && !k8sName.MatchString(t.Container) {
		return "invalid container name"
	}
	if t.Tail <= 0 {
		t.Tail = defaultLogTail
	}
	t.Tail = min(t.Tail, maxLogTail)
	return ""
}

// handleCreateLog queues a log read:
// POST /api/v1/clusters/{id}/logs?tenantId=... {namespace,pod|job,container,previous,tail}
func (s *Server) handleCreateLog(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	c, err := s.store.GetCluster(r.Context(), r.PathValue("id"))
	if err != nil || tenantID == "" || c.TenantID != tenantID {
		writeErr(w, http.StatusNotFound, "cluster not found")
		return
	}
	if kindOf(c) != contract.KindKubernetes {
		writeErr(w, http.StatusBadRequest, "pod logs are for Kubernetes clusters")
		return
	}
	var t contract.LogTarget
	if err := decode(r, &t); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if msg := validLogTarget(&t); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	now := s.now()
	ps := s.probes
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.expireLogs(now)
	pending := 0
	for _, e := range ps.logs {
		if e.cluster == c.ID && !e.done {
			pending++
		}
	}
	if pending >= maxPendingLogs {
		writeErr(w, http.StatusTooManyRequests, "a few log reads are already waiting on this cluster; try again in a moment")
		return
	}
	e := &logEntry{id: newID("log"), tenant: tenantID, cluster: c.ID, target: t, created: now}
	ps.logs[e.id] = e
	writeJSON(w, http.StatusCreated, map[string]any{"id": e.id})
}

// handleGetLog returns a log read's state: GET /api/v1/logs/{id}?tenantId=...
func (s *Server) handleGetLog(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	ps := s.probes
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.expireLogs(s.now())
	e := ps.logs[r.PathValue("id")]
	if e == nil || tenantID == "" || e.tenant != tenantID {
		writeErr(w, http.StatusNotFound, "log request not found or expired")
		return
	}
	if !e.done {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "started": !e.taken.IsZero()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "result": e.result, "error": e.err, "target": e.target})
}

// handleAgentLogResult takes the agent's lines for one of its own log reads:
// POST /api/v1/agents/logs/{id}
func (s *Server) handleAgentLogResult(w http.ResponseWriter, r *http.Request) {
	c, err := clusterFromCtx(r.Context())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var res contract.LogResult
	if err := decode(r, &res); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if len(res.Lines) > maxLogTail {
		res.Lines = res.Lines[len(res.Lines)-maxLogTail:]
	}
	for i, l := range res.Lines {
		if len(l) > maxLogLine {
			l = l[:maxLogLine]
		}
		res.Lines[i] = redact.String(l) // the agent redacts too; never trust one side alone
	}
	if len(res.Containers) > 20 {
		res.Containers = res.Containers[:20]
	}
	if len(res.Error) > 300 {
		res.Error = res.Error[:300]
	}
	ps := s.probes
	ps.mu.Lock()
	defer ps.mu.Unlock()
	e := ps.logs[r.PathValue("id")]
	if e == nil || e.cluster != c.ID || e.taken.IsZero() {
		writeErr(w, http.StatusNotFound, "log request not found")
		return
	}
	if !e.done {
		e.done, e.doneAt, e.result = true, s.now(), &res
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
