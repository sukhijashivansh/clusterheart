package controlplane

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
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
	Mode                       Mode         `json:"mode"`
	AllowedActions             []ActionType `json:"allowedActions"`
	NamespaceAllowlist         []string     `json:"namespaceAllowlist"`
	ProtectedEnvironments      []string     `json:"protectedEnvironments"`
	MaxConcurrent              int          `json:"maxConcurrent"`
	PerWorkloadCooldownSeconds int          `json:"perWorkloadCooldownSeconds"`
	Paused                     bool         `json:"paused"`
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
		TenantID:              tenantID,
		Mode:                  req.Mode,
		AllowedActions:        allowed,
		NamespaceAllowlist:    req.NamespaceAllowlist,
		ProtectedEnvironments: req.ProtectedEnvironments,
		MaxConcurrent:         req.MaxConcurrent,
		PerWorkloadCooldown:   time.Duration(req.PerWorkloadCooldownSeconds) * time.Second,
		Paused:                req.Paused,
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
	pol, err := s.remediation.SetPaused(r.Context(), tenantID, paused, "operator:api")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not update policy")
		return
	}
	writeJSON(w, http.StatusOK, pol)
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
