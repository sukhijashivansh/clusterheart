package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubControlPlane records what the proxy sends and returns canned responses.
type stubControlPlane struct {
	gotAuth    string
	gotTenant  string
	gotPath    string
	gotMethod  string
	gotBody    string
	respStatus int
	respBody   string
}

func (c *stubControlPlane) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.gotAuth = r.Header.Get("Authorization")
		c.gotTenant = r.URL.Query().Get("tenantId")
		c.gotPath = r.URL.Path
		c.gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		c.gotBody = string(b)
		w.WriteHeader(c.respStatus)
		_, _ = w.Write([]byte(c.respBody))
	}
}

func newProxyServer(t *testing.T, cp *stubControlPlane) *server {
	t.Helper()
	ts := httptest.NewServer(cp.handler())
	t.Cleanup(ts.Close)
	return &server{sre: &sreProxy{
		baseURL: ts.URL, token: "admin-secret", tenantID: "acme",
		http: &http.Client{Timeout: 5 * time.Second},
	}}
}

func TestSREConfigDoesNotLeakToken(t *testing.T) {
	s := &server{sre: &sreProxy{baseURL: "http://x", token: "admin-secret", tenantID: "acme"}}
	rr := httptest.NewRecorder()
	s.handleSREConfig(rr, httptest.NewRequest(http.MethodGet, "/api/sre/config", nil))
	body := rr.Body.String()
	if !strings.Contains(body, `"configured":true`) || !strings.Contains(body, `"tenantId":"acme"`) {
		t.Fatalf("unexpected config body: %s", body)
	}
	if strings.Contains(body, "admin-secret") {
		t.Fatalf("admin token leaked to the renderer: %s", body)
	}
}

func TestSREIncidentsInjectsTenantAndToken(t *testing.T) {
	cp := &stubControlPlane{respStatus: 200, respBody: `{"incidents":[]}`}
	s := newProxyServer(t, cp)
	rr := httptest.NewRecorder()
	// The renderer tries to pass a DIFFERENT tenant; it must be ignored.
	s.handleSREIncidents(rr, httptest.NewRequest(http.MethodGet, "/api/sre/incidents?status=open&tenantId=evil", nil))
	if rr.Code != 200 {
		t.Fatalf("want 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	if cp.gotTenant != "acme" {
		t.Fatalf("tenant not pinned server-side: control plane saw tenantId=%q", cp.gotTenant)
	}
	if cp.gotAuth != "Bearer admin-secret" {
		t.Fatalf("admin token not injected: %q", cp.gotAuth)
	}
	if cp.gotPath != "/api/v1/incidents" {
		t.Fatalf("wrong upstream path: %q", cp.gotPath)
	}
}

func TestSREApproveForwardsPost(t *testing.T) {
	cp := &stubControlPlane{respStatus: 200, respBody: `{"status":"approved"}`}
	s := newProxyServer(t, cp)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/sre/proposals/rem_123/approve", strings.NewReader(`{"approver":"x"}`))
	s.handleSREProposalItem(rr, req)
	if rr.Code != 200 {
		t.Fatalf("want 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	if cp.gotMethod != http.MethodPost || cp.gotPath != "/api/v1/proposals/rem_123/approve" {
		t.Fatalf("wrong upstream call: %s %s", cp.gotMethod, cp.gotPath)
	}
	if cp.gotTenant != "acme" {
		t.Fatalf("tenant not pinned: %q", cp.gotTenant)
	}
	if !strings.Contains(cp.gotBody, "approver") {
		t.Fatalf("request body not forwarded: %q", cp.gotBody)
	}
}

func TestSRERejectsUnknownAction(t *testing.T) {
	cp := &stubControlPlane{respStatus: 200, respBody: `{}`}
	s := newProxyServer(t, cp)
	rr := httptest.NewRecorder()
	s.handleSREProposalItem(rr, httptest.NewRequest(http.MethodPost, "/api/sre/proposals/rem_1/delete", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unsupported action should be 400, got %d", rr.Code)
	}
}

func TestSREUnconfiguredReturns503(t *testing.T) {
	s := &server{sre: &sreProxy{}} // nothing set => not configured
	rr := httptest.NewRecorder()
	s.handleSREIncidents(rr, httptest.NewRequest(http.MethodGet, "/api/sre/incidents", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured should be 503, got %d", rr.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if !strings.Contains(body["error"], "not configured") {
		t.Fatalf("unexpected error: %v", body)
	}
}

func TestSREPassesThroughUpstreamErrorStatus(t *testing.T) {
	cp := &stubControlPlane{respStatus: 404, respBody: `{"error":"incident not found"}`}
	s := newProxyServer(t, cp)
	rr := httptest.NewRecorder()
	s.handleSREIncidents(rr, httptest.NewRequest(http.MethodGet, "/api/sre/incidents", nil))
	if rr.Code != 404 {
		t.Fatalf("upstream 404 should pass through, got %d", rr.Code)
	}
}
