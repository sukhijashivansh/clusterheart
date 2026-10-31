package controlplane

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"clusterheart/sre/internal/contract"
)

// Secret reveal relay. A person clicks Reveal on a secret finding; the request
// waits here until the cluster's agent picks it up, the agent answers with the
// raw value from its own memory, and the person's browser collects it once.
// Values live only in this process's memory, are deleted on first read or
// after revealValueTTL, and are never written to a store or a log.
const (
	revealRequestTTL = 2 * time.Minute  // unanswered requests expire
	revealValueTTL   = 60 * time.Second // unread answers expire
	maxRevealValue   = 8192
)

type revealEntry struct {
	id, tenant, cluster, ref string
	created, answered        time.Time
	done                     bool
	value, err               string
}

type revealStore struct {
	mu sync.Mutex
	m  map[string]*revealEntry
}

func newRevealStore() *revealStore { return &revealStore{m: map[string]*revealEntry{}} }

// gc drops expired requests and unread answers. Callers hold mu.
func (rs *revealStore) gc(now time.Time) {
	for id, e := range rs.m {
		if (!e.done && now.Sub(e.created) > revealRequestTTL) || (e.done && now.Sub(e.answered) > revealValueTTL) {
			delete(rs.m, id)
		}
	}
}

// handleCreateReveal records a reveal request from the dashboard:
// POST /api/v1/clusters/{id}/reveals?tenantId=... {"ref","requestedBy","description"}.
// The request is audited (who, which finding), never the value.
func (s *Server) handleCreateReveal(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	c, err := s.store.GetCluster(r.Context(), r.PathValue("id"))
	if tenantID == "" || err != nil || c.TenantID != tenantID {
		writeErr(w, http.StatusNotFound, "cluster not found")
		return
	}
	var req struct {
		Ref         string `json:"ref"`
		RequestedBy string `json:"requestedBy"`
		Description string `json:"description"`
	}
	if err := decode(r, &req); err != nil || strings.TrimSpace(req.Ref) == "" || strings.TrimSpace(req.RequestedBy) == "" {
		writeErr(w, http.StatusBadRequest, "ref and requestedBy are required")
		return
	}
	now := s.now()
	e := &revealEntry{id: newID("rvl"), tenant: tenantID, cluster: c.ID, ref: req.Ref, created: now}
	s.reveals.mu.Lock()
	s.reveals.gc(now)
	s.reveals.m[e.id] = e
	s.reveals.mu.Unlock()
	if s.remediation != nil {
		desc := strings.TrimSpace(req.Description)
		if len(desc) > 300 {
			desc = desc[:300]
		}
		s.remediation.appendAudit(r.Context(), tenantID, req.RequestedBy, "secret.revealed", "", "", "cluster:"+c.ID+" "+desc)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": e.id})
}

// handleGetReveal returns a reveal's state to the dashboard:
// GET /api/v1/reveals/{id}?tenantId=... -> {"done":false} | {"done":true,"value"|"error"}.
// An answer is handed out once and then deleted.
func (s *Server) handleGetReveal(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenantId"))
	s.reveals.mu.Lock()
	defer s.reveals.mu.Unlock()
	s.reveals.gc(s.now())
	e := s.reveals.m[r.PathValue("id")]
	if e == nil || tenantID == "" || e.tenant != tenantID {
		writeErr(w, http.StatusNotFound, "reveal request not found or expired")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !e.done {
		writeJSON(w, http.StatusOK, map[string]any{"done": false})
		return
	}
	delete(s.reveals.m, e.id)
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "value": e.value, "error": e.err})
}

// handleAgentReveals lists the authenticated cluster's pending reveal requests:
// GET /api/v1/agents/reveals.
func (s *Server) handleAgentReveals(w http.ResponseWriter, r *http.Request) {
	c, err := clusterFromCtx(r.Context())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	s.reveals.mu.Lock()
	defer s.reveals.mu.Unlock()
	s.reveals.gc(s.now())
	out := []contract.RevealRequest{}
	for _, e := range s.reveals.m {
		if e.cluster == c.ID && !e.done {
			out = append(out, contract.RevealRequest{ID: e.id, Ref: e.ref})
		}
	}
	writeJSON(w, http.StatusOK, contract.RevealRequestsResponse{Requests: out})
}

// handleAgentRevealResult takes the agent's answer for one of its own requests:
// POST /api/v1/agents/reveals/{id}.
func (s *Server) handleAgentRevealResult(w http.ResponseWriter, r *http.Request) {
	c, err := clusterFromCtx(r.Context())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var res contract.RevealResult
	if err := decode(r, &res); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid reveal result")
		return
	}
	now := s.now()
	s.reveals.mu.Lock()
	defer s.reveals.mu.Unlock()
	s.reveals.gc(now)
	e := s.reveals.m[r.PathValue("id")]
	if e == nil || e.cluster != c.ID || e.done {
		writeErr(w, http.StatusNotFound, "reveal request not found")
		return
	}
	if len(res.Value) > maxRevealValue {
		res.Value = res.Value[:maxRevealValue]
	}
	e.done, e.answered, e.value, e.err = true, now, res.Value, res.Error
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
