// Package controlplane implements the multi-tenant control-plane API: cluster
// registration, agent heartbeats, telemetry ingestion, and read-back for
// operators. Milestone 1 is observe-only plumbing - no detection, AI, policy,
// or remediation lives here yet (those are later milestones).
package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"clusterheart/sre/internal/contract"
	"clusterheart/sre/internal/redact"
)

// staleAfter is how long without a heartbeat before a cluster is reported
// disconnected.
const staleAfter = 90 * time.Second

// maxSnapshotsPerBatch bounds a single telemetry POST (backpressure / abuse).
const maxSnapshotsPerBatch = 50

// Server holds control-plane dependencies.
type Server struct {
	store       Store
	incidents   IncidentStore
	engine      *Engine
	remediation *remediationService
	diagnosis   *diagnosisService
	runbooks    RunbookStore
	notifier    Notifier
	adminToken  string
	log         *slog.Logger
	now         func() time.Time
}

// agentDownAfter is how long without a heartbeat (from a previously-connected
// cluster) before the control plane raises an AgentDown incident. It is beyond
// the UI "connected" window (staleAfter) so brief blips don't alarm.
const agentDownAfter = 3 * time.Minute

// NewServer constructs a control-plane server. adminToken guards operator
// endpoints; it must be non-empty in production (enforced by the caller). The
// remediation + diagnosis stores are injected so the caller can choose Mongo
// (durable) or in-memory (local dev / tests). External AI for diagnosis is
// opt-in via env (newLLMDiagnoser returns nil when unconfigured → deterministic).
func NewServer(store Store, incidents IncidentStore, rem RemediationStore, pol PolicyStore, audit AuditStore, diag DiagnosisStore, runbooks RunbookStore, adminToken string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	notifier := newNotifier(log)
	remSvc := newRemediationService(rem, pol, audit, incidents, store, notifier, log)
	diagSvc := newDiagnosisService(diag, incidents, newLLMDiagnoser())
	return &Server{
		store:       store,
		incidents:   incidents,
		engine:      newEngine(incidents, notifier, log),
		remediation: remSvc,
		diagnosis:   diagSvc,
		runbooks:    runbooks,
		notifier:    notifier,
		adminToken:  adminToken,
		log:         log,
		now:         time.Now,
	}
}

// Routes returns the HTTP handler for the control plane.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	// Operator / admin.
	mux.HandleFunc("POST /api/v1/clusters", s.requireAdmin(s.handleRegister))
	mux.HandleFunc("GET /api/v1/clusters", s.requireAdmin(s.handleListClusters))
	mux.HandleFunc("GET /api/v1/incidents", s.requireAdmin(s.handleListIncidents))
	mux.HandleFunc("GET /api/v1/incidents/{id}", s.requireAdmin(s.handleGetIncident))
	mux.HandleFunc("GET /api/v1/stats", s.requireAdmin(s.handleStats))
	// AI root-cause analysis (deterministic by default; external AI opt-in).
	mux.HandleFunc("POST /api/v1/incidents/{id}/diagnose", s.requireAdmin(s.handleDiagnose))
	mux.HandleFunc("GET /api/v1/incidents/{id}/diagnosis", s.requireAdmin(s.handleGetDiagnosis))
	// One-click: turn an actionable diagnosis into a policy-checked proposal.
	mux.HandleFunc("POST /api/v1/incidents/{id}/propose", s.requireAdmin(s.handleProposeFromDiagnosis))
	// Remediation (deterministic policy, approvals, dry-run execution, audit).
	mux.HandleFunc("POST /api/v1/incidents/{id}/proposals", s.requireAdmin(s.handleCreateProposal))
	mux.HandleFunc("GET /api/v1/proposals", s.requireAdmin(s.handleListProposals))
	mux.HandleFunc("GET /api/v1/proposals/{id}", s.requireAdmin(s.handleGetProposal))
	mux.HandleFunc("POST /api/v1/proposals/{id}/approve", s.requireAdmin(s.handleApproveProposal))
	mux.HandleFunc("POST /api/v1/proposals/{id}/reject", s.requireAdmin(s.handleRejectProposal))
	mux.HandleFunc("GET /api/v1/policy", s.requireAdmin(s.handleGetPolicy))
	mux.HandleFunc("PUT /api/v1/policy", s.requireAdmin(s.handlePutPolicy))
	mux.HandleFunc("POST /api/v1/policy/pause", s.requireAdmin(s.handlePausePolicy))
	mux.HandleFunc("POST /api/v1/policy/resume", s.requireAdmin(s.handleResumePolicy))
	mux.HandleFunc("GET /api/v1/audit", s.requireAdmin(s.handleListAudit))
	mux.HandleFunc("GET /api/v1/runbooks", s.requireAdmin(s.handleListRunbooks))
	mux.HandleFunc("PUT /api/v1/runbooks", s.requireAdmin(s.handlePutRunbook))
	mux.HandleFunc("POST /api/v1/clusters/{id}/remediation-token", s.requireAdmin(s.handleMintRemediationToken))
	// In-cluster remediation controller (separate remediate credential).
	mux.HandleFunc("GET /api/v1/remediation/pending", s.requireRemediator(s.handlePendingRemediation))
	mux.HandleFunc("POST /api/v1/remediation/{id}/result", s.requireRemediator(s.handleReportResult))
	// Agent.
	mux.HandleFunc("POST /api/v1/agents/heartbeat", s.requireAgent(s.handleHeartbeat))
	mux.HandleFunc("POST /api/v1/telemetry", s.requireAgent(s.handleTelemetry))
	// Health.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	return logging(s.log, mux)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req contract.RegisterRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.TenantID = strings.TrimSpace(req.TenantID)
	req.ClusterName = strings.TrimSpace(req.ClusterName)
	if req.TenantID == "" || req.ClusterName == "" {
		writeErr(w, http.StatusBadRequest, "tenantId and clusterName are required")
		return
	}
	token, hash, err := mintAgentToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not mint agent token")
		return
	}
	id := newID("clu")
	c := Cluster{
		ID:             id,
		TenantID:       req.TenantID,
		Name:           req.ClusterName,
		Environment:    req.Environment,
		Region:         req.Region,
		AgentTokenHash: hash,
		CreatedAt:      s.now(),
	}
	if err := s.store.CreateCluster(r.Context(), c); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not register cluster")
		return
	}
	s.log.Info("cluster registered", "clusterId", id, "tenantId", req.TenantID, "name", req.ClusterName)
	writeJSON(w, http.StatusCreated, contract.RegisterResponse{
		ClusterID:  id,
		AgentToken: token, // shown once; only the hash is stored
		APIVersion: contract.APIVersion,
	})
}

func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	clusters, err := s.store.ListClusters(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list clusters")
		return
	}
	out := make([]contract.ClusterStatus, 0, len(clusters))
	for _, c := range clusters {
		st := contract.ClusterStatus{
			ClusterID:     c.ID,
			TenantID:      c.TenantID,
			Name:          c.Name,
			Environment:   c.Environment,
			Region:        c.Region,
			LastHeartbeat: c.LastHeartbeat,
			AgentVersion:  c.AgentVersion,
			K8sVersion:    c.K8sVersion,
			Connected:     c.LastHeartbeat != nil && s.now().Sub(*c.LastHeartbeat) < staleAfter,
		}
		if snap, err := s.store.LatestSnapshot(r.Context(), c.ID); err == nil {
			st.Latest = snap
		}
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": out})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	c, err := clusterFromCtx(r.Context())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var hb contract.Heartbeat
	if err := decode(r, &hb); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid heartbeat body")
		return
	}
	if hb.APIVersion != contract.APIVersion {
		writeErr(w, http.StatusConflict, "unsupported apiVersion; upgrade the agent")
		return
	}
	if err := s.store.RecordHeartbeat(r.Context(), c.ID, hb, s.now()); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not record heartbeat")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	c, err := clusterFromCtx(r.Context())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var batch contract.TelemetryBatch
	if err := decode(r, &batch); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid telemetry body")
		return
	}
	if batch.APIVersion != contract.APIVersion {
		writeErr(w, http.StatusConflict, "unsupported apiVersion; upgrade the agent")
		return
	}
	if len(batch.Snapshots) == 0 {
		writeErr(w, http.StatusBadRequest, "no snapshots in batch")
		return
	}
	if len(batch.Snapshots) > maxSnapshotsPerBatch {
		writeErr(w, http.StatusRequestEntityTooLarge, "too many snapshots in one batch")
		return
	}
	// Defense-in-depth: re-redact all free-text fields at ingestion so a
	// non-compliant or compromised agent cannot land raw secrets/PII in the
	// incident store or a downstream AI prompt (the agent redacts too).
	for i := range batch.Snapshots {
		sanitizeSnapshot(&batch.Snapshots[i])
	}
	// Telemetry is written ONLY for the authenticated cluster - never a cluster
	// id taken from the request body (tenant isolation at the boundary).
	if err := s.store.SaveSnapshots(r.Context(), c.ID, batch.Snapshots); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not store telemetry")
		return
	}
	// Run deterministic detection on the latest snapshot. Detection failures must
	// not fail ingestion - telemetry is already durably stored.
	if err := s.engine.reconcileLatest(r.Context(), c.TenantID, c.ID, batch.Snapshots); err != nil {
		s.log.Error("incident reconcile failed", "clusterId", c.ID, "err", err)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": len(batch.Snapshots)})
}

func (s *Server) handleListIncidents(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status")) // "", "open", or "resolved"
	incs, err := s.incidents.ListIncidents(r.Context(), tenantID, status)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list incidents")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": incs})
}

func (s *Server) handleGetIncident(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	inc, err := s.incidents.GetIncident(r.Context(), tenantID, r.PathValue("id"))
	if err == ErrNotFound {
		writeErr(w, http.StatusNotFound, "incident not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not load incident")
		return
	}
	writeJSON(w, http.StatusOK, inc)
}

// handleDiagnose runs (or re-runs) root-cause analysis for an incident.
func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	dg, err := s.diagnosis.Diagnose(r.Context(), tenantID, r.PathValue("id"))
	if err == ErrNotFound {
		writeErr(w, http.StatusNotFound, "incident not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not diagnose incident")
		return
	}
	writeJSON(w, http.StatusOK, dg)
}

// handleGetDiagnosis returns the latest stored diagnosis for an incident.
func (s *Server) handleGetDiagnosis(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	dg, err := s.diagnosis.Get(r.Context(), tenantID, r.PathValue("id"))
	if err == ErrNotFound {
		writeErr(w, http.StatusNotFound, "no diagnosis yet for this incident")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not load diagnosis")
		return
	}
	writeJSON(w, http.StatusOK, dg)
}

// handleProposeFromDiagnosis turns an incident's AI recommendation into a
// remediation proposal - through the normal policy-checked CreateProposal path
// (the diagnosis only pre-fills action+target; it never bypasses policy). If no
// diagnosis exists yet, one is produced first.
func (s *Server) handleProposeFromDiagnosis(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	id := r.PathValue("id")
	dg, err := s.diagnosis.Get(r.Context(), tenantID, id)
	if err == ErrNotFound {
		dg, err = s.diagnosis.Diagnose(r.Context(), tenantID, id)
	}
	if err == ErrNotFound {
		writeErr(w, http.StatusNotFound, "incident not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not diagnose incident")
		return
	}
	// Default to the AI recommendation.
	action, params, target := dg.RecommendedAction, map[string]string(nil), dg.RecommendedTarget
	rationale := "from AI diagnosis: " + dg.Summary
	actionable := dg.RecommendedActionable && dg.RecommendedTarget != nil

	// An enabled operator runbook for this incident's type takes precedence. The
	// target is always the incident's own workload (never runbook-supplied).
	if s.runbooks != nil {
		if inc, ierr := s.incidents.GetIncident(r.Context(), tenantID, id); ierr == nil {
			if rb, rerr := s.runbooks.GetForType(r.Context(), tenantID, string(inc.Type)); rerr == nil && rb.Enabled {
				kind, name := splitWorkload(inc.Workload)
				action, params = string(rb.Action), rb.Params
				target = &TargetRef{Kind: kind, Namespace: inc.Namespace, Name: name}
				rationale = fmt.Sprintf("from runbook v%d (%s)", rb.Version, rb.IncidentType)
				actionable = true
			}
		}
	}
	if !actionable || target == nil {
		writeErr(w, http.StatusUnprocessableEntity, "no automatic action recommended for this incident; investigate")
		return
	}
	p, err := s.remediation.CreateProposal(r.Context(), tenantID, id,
		ActionType(action), *target, params, rationale, "operator:dashboard")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleListRunbooks(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	rbs, err := s.runbooks.List(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list runbooks")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runbooks": rbs})
}

func (s *Server) handlePutRunbook(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	var rb Runbook
	if err := decode(r, &rb); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid runbook body")
		return
	}
	rb.TenantID = tenantID
	rb.UpdatedAt = s.now()
	if rb.UpdatedBy == "" {
		rb.UpdatedBy = "operator:api"
	}
	if err := validateRunbook(rb); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := s.runbooks.Upsert(r.Context(), rb)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save runbook")
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// RunAgentHealthSweeps periodically checks for clusters that have stopped
// reporting and raises AgentDown incidents, so the platform never silently
// claims a cluster is healthy while it is actually blind (spec §17). It returns
// when ctx is cancelled.
func (s *Server) RunAgentHealthSweeps(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepAgentHealth(ctx, s.now())
		}
	}
}

// sweepAgentHealth performs one agent-liveness pass. A cluster that has never
// connected (nil heartbeat) is not alarmed; one whose last heartbeat is older
// than agentDownAfter gets an open AgentDown incident. Recovery is handled
// automatically: when telemetry resumes, the normal ResolveMissing pass resolves
// the AgentDown incident (its fingerprint isn't in the live fault set).
func (s *Server) sweepAgentHealth(ctx context.Context, now time.Time) {
	clusters, err := s.store.AllClusters(ctx)
	if err != nil {
		s.log.Error("agent-health sweep could not list clusters", "err", err)
		return
	}
	for _, c := range clusters {
		if c.LastHeartbeat == nil {
			continue // never connected - nothing to compare against
		}
		age := now.Sub(*c.LastHeartbeat)
		if age <= agentDownAfter {
			continue
		}
		obs := observation{
			fingerprint: fingerprint(c.ID, "", "", "", contract.ProblemAgentDown),
			typ:         contract.ProblemAgentDown,
			severity:    severityFor(contract.ProblemAgentDown),
			title:       fmt.Sprintf("AgentDown: no telemetry from cluster %s", c.Name),
			evidence: EvidenceItem{
				ObservedAt: now, Ref: c.ID, Reason: string(contract.ProblemAgentDown),
				Message: fmt.Sprintf("no heartbeat for %s (last seen %s)", age.Round(time.Second), c.LastHeartbeat.UTC().Format(time.RFC3339)),
			},
		}
		created, err := s.incidents.UpsertObservation(ctx, c.TenantID, c.ID, now, obs)
		if err != nil {
			s.log.Error("could not raise AgentDown incident", "clusterId", c.ID, "err", err)
			continue
		}
		if created {
			s.log.Warn("cluster went silent", "clusterId", c.ID, "ageSeconds", int(age.Seconds()))
			s.notifier.Notify(Notification{
				Kind: "incident.opened", Severity: obs.severity, Title: obs.title,
				Detail: obs.evidence.Message, TenantID: c.TenantID, ClusterID: c.ID,
			})
		}
	}
}

// handleStats aggregates a tenant-scoped overview of the platform's own state
// (spec §14 Global Overview / §17 self-observability) - all measured, nothing
// fabricated.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	ctx := r.Context()
	clusters, _ := s.store.ListClusters(ctx, tenantID)
	connected := 0
	for _, c := range clusters {
		if c.LastHeartbeat != nil && s.now().Sub(*c.LastHeartbeat) < staleAfter {
			connected++
		}
	}
	incs, _ := s.incidents.ListIncidents(ctx, tenantID, "")
	incByStatus, incBySeverity, incByType := map[string]int{}, map[string]int{}, map[string]int{}
	for _, i := range incs {
		incByStatus[i.Status]++
		if i.Status == "open" {
			incBySeverity[i.Severity]++
			incByType[string(i.Type)]++
		}
	}
	props, _ := s.remediation.ListProposals(ctx, tenantID, "")
	propByStatus := map[string]int{}
	for _, p := range props {
		propByStatus[string(p.Status)]++
	}
	pol, _ := s.remediation.GetPolicy(ctx, tenantID)
	writeJSON(w, http.StatusOK, map[string]any{
		"clusters":  map[string]any{"total": len(clusters), "connected": connected},
		"incidents": map[string]any{"total": len(incs), "byStatus": incByStatus, "openBySeverity": incBySeverity, "openByType": incByType},
		"proposals": map[string]any{"total": len(props), "byStatus": propByStatus},
		"policy":    map[string]any{"mode": pol.Mode, "paused": pol.Paused},
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// A trivial store round-trip proves readiness without leaking data.
	if _, err := s.store.ListClusters(r.Context(), "__readyz__"); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

// sanitizeSnapshot redacts secret/PII-shaped substrings from all free-text
// fields of a snapshot in place.
func sanitizeSnapshot(s *contract.ClusterSnapshot) {
	for i := range s.Warnings {
		s.Warnings[i].Message = redact.String(s.Warnings[i].Message)
	}
	for i := range s.PodProblems {
		s.PodProblems[i].Message = redact.String(s.PodProblems[i].Message)
	}
	for i := range s.WorkloadProblems {
		s.WorkloadProblems[i].Message = redact.String(s.WorkloadProblems[i].Message)
	}
}

// --- helpers -------------------------------------------------------------

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)) // 1 MiB cap
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, contract.ErrorResponse{Error: msg})
}

// logging records method, path, status, and duration for every request.
func logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "dur", time.Since(start).String())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(c int) { s.status = c; s.ResponseWriter.WriteHeader(c) }
