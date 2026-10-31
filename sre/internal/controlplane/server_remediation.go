package controlplane

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"clusterheart/sre/internal/contract"
)

// --- request shapes ------------------------------------------------------

type createProposalRequest struct {
	Action    ActionType        `json:"action"`
	Target    TargetRef         `json:"target"`
	Params    map[string]string `json:"params,omitempty"`
	Rationale string            `json:"rationale"`
	CreatedBy string            `json:"createdBy,omitempty"`
}

type decisionRequest struct {
	Approver string `json:"approver,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type policyRequest struct {
	Mode                        Mode         `json:"mode"`
	AllowedActions              []ActionType `json:"allowedActions"`
	NamespaceAllowlist          []string     `json:"namespaceAllowlist"`
	ProtectedEnvironments       []string     `json:"protectedEnvironments"`
	MaxConcurrent               int          `json:"maxConcurrent"`
	PerWorkloadCooldownSeconds  int          `json:"perWorkloadCooldownSeconds"`
	Paused                      bool         `json:"paused"`
	CircuitBreakerFailures      int          `json:"circuitBreakerFailures"`
	CircuitBreakerWindowSeconds int          `json:"circuitBreakerWindowSeconds"`
	MaxReplicas                 int          `json:"maxReplicas"`
}

// --- handlers ------------------------------------------------------------

func (s *Server) handleCreateProposal(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	var req createProposalRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	createdBy := strings.TrimSpace(req.CreatedBy)
	if createdBy == "" {
		createdBy = "operator:api"
	}
	p, err := s.remediation.CreateProposal(r.Context(), tenantID, r.PathValue("id"),
		req.Action, req.Target, req.Params, req.Rationale, createdBy)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "incident not found")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// handleCreateClusterProposal records a proposal not tied to an incident:
// POST /api/v1/clusters/{id}/proposals?tenantId=... (used by Rightsizing).
func (s *Server) handleCreateClusterProposal(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	var req createProposalRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	createdBy := strings.TrimSpace(req.CreatedBy)
	if createdBy == "" {
		createdBy = "operator:api"
	}
	p, err := s.remediation.CreateClusterProposal(r.Context(), tenantID, r.PathValue("id"),
		req.Action, req.Target, req.Params, req.Rationale, createdBy)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "cluster not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleListProposals(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	ps, err := s.remediation.ListProposals(r.Context(), tenantID, strings.TrimSpace(r.URL.Query().Get("status")))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list proposals")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": ps})
}

func (s *Server) handleGetProposal(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	p, err := s.remediation.GetProposal(r.Context(), tenantID, r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "proposal not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not load proposal")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleApproveProposal(w http.ResponseWriter, r *http.Request) {
	s.decide(w, r, true)
}
func (s *Server) handleRejectProposal(w http.ResponseWriter, r *http.Request) {
	s.decide(w, r, false)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request, approve bool) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	var req decisionRequest
	if err := decode(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	actor := strings.TrimSpace(req.Approver)
	if actor == "" {
		actor = "operator:api"
	}
	var p *RemediationProposal
	var err error
	if approve {
		p, err = s.remediation.Approve(r.Context(), tenantID, r.PathValue("id"), actor)
	} else {
		p, err = s.remediation.Reject(r.Context(), tenantID, r.PathValue("id"), actor, req.Reason)
	}
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "proposal not found")
	case errors.Is(err, errConflict):
		writeErr(w, http.StatusConflict, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "could not update proposal")
	default:
		writeJSON(w, http.StatusOK, p)
	}
}

func (s *Server) handleGetPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	pol, err := s.remediation.GetPolicy(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not load policy")
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

func (s *Server) handlePutPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	var req policyRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Validate mode and that every allowlisted action is actually supported.
	switch req.Mode {
	case ModeOff, ModeDryRun, ModeRecommend, ModeApprovalRequired, ModeAuto:
	default:
		writeErr(w, http.StatusBadRequest, "invalid mode")
		return
	}
	allowed := map[ActionType]bool{}
	for _, a := range req.AllowedActions {
		if _, err := lookupAction(a); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		allowed[a] = true
	}
	pol := Policy{
		TenantID:               tenantID,
		Mode:                   req.Mode,
		AllowedActions:         allowed,
		NamespaceAllowlist:     req.NamespaceAllowlist,
		ProtectedEnvironments:  req.ProtectedEnvironments,
		MaxConcurrent:          req.MaxConcurrent,
		PerWorkloadCooldown:    time.Duration(req.PerWorkloadCooldownSeconds) * time.Second,
		Paused:                 req.Paused,
		CircuitBreakerFailures: req.CircuitBreakerFailures,
		CircuitBreakerWindow:   time.Duration(req.CircuitBreakerWindowSeconds) * time.Second,
		MaxReplicas:            req.MaxReplicas,
	}
	if err := s.remediation.PutPolicy(r.Context(), pol, "operator:api"); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save policy")
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

func (s *Server) handlePausePolicy(w http.ResponseWriter, r *http.Request)  { s.setPause(w, r, true) }
func (s *Server) handleResumePolicy(w http.ResponseWriter, r *http.Request) { s.setPause(w, r, false) }

func (s *Server) setPause(w http.ResponseWriter, r *http.Request, paused bool) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	// Optional scope: ?clusterId=... (cluster) or ?workload=Kind/Namespace/Name
	// (workload). With neither, the switch is tenant-wide.
	scope, id := "tenant", ""
	if c := strings.TrimSpace(r.URL.Query().Get("clusterId")); c != "" {
		scope, id = "cluster", c
	} else if wl := strings.TrimSpace(r.URL.Query().Get("workload")); wl != "" {
		scope, id = "workload", wl
	}
	pol, err := s.remediation.SetPauseScope(r.Context(), tenantID, scope, id, paused, "operator:api")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

// --- remediation controller protocol ------------------------------------

// handleMintRemediationToken opts a cluster into remediation by issuing a
// separate remediation token (shown once). Until this is called, no in-cluster
// executor is authorized for the cluster.
func (s *Server) handleMintRemediationToken(w http.ResponseWriter, r *http.Request) {
	clusterID := r.PathValue("id")
	c, err := s.store.GetCluster(r.Context(), clusterID)
	// Optional ?tenantId guards against enabling a cluster outside the caller's
	// tenant (the browser dashboard always sends it).
	if err != nil || (strings.TrimSpace(r.URL.Query().Get("tenantId")) != "" && strings.TrimSpace(r.URL.Query().Get("tenantId")) != c.TenantID) {
		writeErr(w, http.StatusNotFound, "cluster not found")
		return
	}
	token, hash, err := mintAgentToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not mint remediation token")
		return
	}
	if err := s.store.SetRemediationTokenHash(r.Context(), clusterID, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not store remediation token")
		return
	}
	s.log.Info("remediation enabled for cluster", "clusterId", clusterID)
	writeJSON(w, http.StatusCreated, contract.RemediationTokenResponse{ClusterID: clusterID, RemediationToken: token})
}

// handlePendingRemediation returns this cluster's approved work queue.
func (s *Server) handlePendingRemediation(w http.ResponseWriter, r *http.Request) {
	c, err := clusterFromCtx(r.Context())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	ps, err := s.remediation.PendingForCluster(r.Context(), c.TenantID, c.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list pending actions")
		return
	}
	actions := make([]contract.PendingAction, 0, len(ps))
	for _, p := range ps {
		dryRun := p.Decision != nil && p.Decision.Mode == ModeDryRun
		actions = append(actions, contract.PendingAction{
			ProposalID: p.ID, Action: string(p.Action),
			Target:         contract.ActionTarget{Kind: p.Target.Kind, Namespace: p.Target.Namespace, Name: p.Target.Name},
			Params:         p.Params,
			DryRun:         dryRun,
			TimeoutSeconds: 120,
		})
	}
	writeJSON(w, http.StatusOK, contract.PendingActionsResponse{Actions: actions})
}

// handleReportResult records the controller's outcome for an approved action.
func (s *Server) handleReportResult(w http.ResponseWriter, r *http.Request) {
	c, err := clusterFromCtx(r.Context())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var res contract.ActionResult
	if err := decode(r, &res); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid result body")
		return
	}
	p, err := s.remediation.ReportResult(r.Context(), c, r.PathValue("id"), res)
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "proposal not found for this cluster")
	case errors.Is(err, errConflict):
		writeErr(w, http.StatusConflict, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "could not record result")
	default:
		writeJSON(w, http.StatusOK, p)
	}
}

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	if tenantID == "" {
		writeErr(w, http.StatusBadRequest, "tenantId query parameter is required")
		return
	}
	evs, err := s.remediation.ListAudit(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list audit events")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}
