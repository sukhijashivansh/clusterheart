package main

// SRE control-plane proxy. The dashboard talks to the Autonomous SRE control
// plane (a separate multi-tenant service) on the user's behalf. The admin token
// stays SERVER-SIDE here and is never handed to the renderer; the dashboard
// pins a single tenant (SRE_TENANT_ID) so the UI can only ever see/act on that
// tenant. If the control plane isn't configured, every endpoint returns a clear
// 503 and the UI shows a "not configured" state (never fabricated data).
//
// Config (env, passed to the dashboard binary):
//   CONTROLPLANE_URL         e.g. http://127.0.0.1:9100 or https://sre.example.com
//   CONTROLPLANE_ADMIN_TOKEN operator token (guards the control-plane admin API)
//   SRE_TENANT_ID            the tenant this dashboard is scoped to

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type sreProxy struct {
	baseURL  string
	token    string
	tenantID string
	http     *http.Client
}

func newSREProxy() *sreProxy {
	return &sreProxy{
		baseURL:  strings.TrimRight(os.Getenv("CONTROLPLANE_URL"), "/"),
		token:    os.Getenv("CONTROLPLANE_ADMIN_TOKEN"),
		tenantID: strings.TrimSpace(os.Getenv("SRE_TENANT_ID")),
		http:     &http.Client{Timeout: 20 * time.Second},
	}
}

func (p *sreProxy) configured() bool {
	return p.baseURL != "" && p.token != "" && p.tenantID != ""
}

// call forwards to the control plane with the admin token and returns the raw
// status + body so handlers can pass control-plane responses straight through.
func (p *sreProxy) call(ctx context.Context, method, cpPath string, q url.Values, body io.Reader) (int, []byte, error) {
	u := p.baseURL + cpPath
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, nil
}

// forward runs a control-plane call and relays its status + body. tenantId is
// always injected here (never taken from the renderer) so the UI is pinned to
// the configured tenant.
func (s *server) sreForward(w http.ResponseWriter, r *http.Request, method, cpPath string, extra url.Values, body io.Reader) {
	if !s.sre.configured() {
		writeErr(w, http.StatusServiceUnavailable, "SRE control plane is not configured")
		return
	}
	q := url.Values{}
	q.Set("tenantId", s.sre.tenantID)
	for k, vs := range extra {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	status, b, err := s.sre.call(ctx, method, cpPath, q, body)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "control plane unreachable: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// handleSREConfig tells the UI whether SRE is wired up (and to which tenant),
// WITHOUT exposing the admin token.
func (s *server) handleSREConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": s.sre.configured(),
		"tenantId":   s.sre.tenantID,
		"baseURL":    s.sre.baseURL,
	})
}

func (s *server) handleSREIncidents(w http.ResponseWriter, r *http.Request) {
	extra := url.Values{}
	if st := strings.TrimSpace(r.URL.Query().Get("status")); st != "" {
		extra.Set("status", st)
	}
	s.sreForward(w, r, http.MethodGet, "/api/v1/incidents", extra, nil)
}

func (s *server) handleSREProposals(w http.ResponseWriter, r *http.Request) {
	extra := url.Values{}
	if st := strings.TrimSpace(r.URL.Query().Get("status")); st != "" {
		extra.Set("status", st)
	}
	s.sreForward(w, r, http.MethodGet, "/api/v1/proposals", extra, nil)
}

// handleSREProposalItem handles /api/sre/proposals/{id}/approve|reject.
func (s *server) handleSREProposalItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/sre/proposals/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	id, action := parts[0], parts[1]
	if action != "approve" && action != "reject" {
		writeErr(w, http.StatusBadRequest, "unsupported action")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	s.sreForward(w, r, http.MethodPost, "/api/v1/proposals/"+url.PathEscape(id)+"/"+action, nil, r.Body)
}

func (s *server) handleSREPolicy(w http.ResponseWriter, r *http.Request) {
	s.sreForward(w, r, http.MethodGet, "/api/v1/policy", nil, nil)
}

func (s *server) handleSREPolicyPause(w http.ResponseWriter, r *http.Request) {
	s.sreForward(w, r, http.MethodPost, "/api/v1/policy/pause", nil, nil)
}

func (s *server) handleSREPolicyResume(w http.ResponseWriter, r *http.Request) {
	s.sreForward(w, r, http.MethodPost, "/api/v1/policy/resume", nil, nil)
}

func (s *server) handleSREAudit(w http.ResponseWriter, r *http.Request) {
	s.sreForward(w, r, http.MethodGet, "/api/v1/audit", nil, nil)
}
