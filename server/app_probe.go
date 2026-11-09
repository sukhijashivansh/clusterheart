package main

// Latency checks for the browser dashboard. The web server never reaches a
// pod: it queues the check on the SRE control plane, the cluster's agent sends
// a few GET requests straight to the pod and reports the timings (TCP connect,
// TLS handshake, first byte, download), and the page polls for them.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"
)

// probeRL caps how often one person can start latency checks.
var probeRL = newRateLimiter(40, 10*time.Minute)

// handleAppStartProbe: POST /api/app/clusters/{id}/probes
// {"namespace","pod","port","scheme","path","samples"}
func (a *app) handleAppStartProbe(w http.ResponseWriter, r *http.Request) {
	u, tenant, ok := a.appSREUser(w, r)
	if !ok {
		return
	}
	if !probeRL.allow("probe|" + u.Email) {
		writeJSON(w, 429, map[string]any{"error": "Too many checks. Please wait a few minutes."})
		return
	}
	var in struct {
		Namespace string `json:"namespace"`
		Pod       string `json:"pod"`
		Port      int    `json:"port"`
		Scheme    string `json:"scheme"`
		Path      string `json:"path"`
		Samples   int    `json:"samples"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid request"})
		return
	}
	code, data, err := a.cpDo("POST", "/api/v1/clusters/"+url.PathEscape(r.PathValue("id"))+"/probes?tenantId="+url.QueryEscape(tenant), in)
	cpRelay(w, code, data, err)
}

// handleAppProbeResult: GET /api/app/probes/{id}
func (a *app) handleAppProbeResult(w http.ResponseWriter, r *http.Request) {
	_, tenant, ok := a.appSREUser(w, r)
	if !ok {
		return
	}
	code, data, err := a.cpDo("GET", "/api/v1/probes/"+url.PathEscape(r.PathValue("id"))+"?tenantId="+url.QueryEscape(tenant), nil)
	cpRelay(w, code, data, err)
}

// Pod and job logs follow the same path: the agent reads the tail (redacted)
// and the page polls for it.

// logRL caps how often one person can open logs.
var logRL = newRateLimiter(120, 10*time.Minute)

// handleAppStartLog: POST /api/app/clusters/{id}/logs
// {"namespace","pod"|"job","container","previous","tail"}
func (a *app) handleAppStartLog(w http.ResponseWriter, r *http.Request) {
	u, ws, ok := a.wsAllowed(w, r, "logs")
	if !ok {
		return
	}
	tenant := ws.Tenant
	if !logRL.allow("log|" + u.Email) {
		writeJSON(w, 429, map[string]any{"error": "Too many log reads. Please wait a few minutes."})
		return
	}
	var in struct {
		Namespace string `json:"namespace"`
		Pod       string `json:"pod,omitempty"`
		Job       string `json:"job,omitempty"`
		Container string `json:"container,omitempty"`
		Previous  bool   `json:"previous,omitempty"`
		Tail      int    `json:"tail"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid request"})
		return
	}
	code, data, err := a.cpDo("POST", "/api/v1/clusters/"+url.PathEscape(r.PathValue("id"))+"/logs?tenantId="+url.QueryEscape(tenant), in)
	cpRelay(w, code, data, err)
}

// handleAppLogResult: GET /api/app/logs/{id}
func (a *app) handleAppLogResult(w http.ResponseWriter, r *http.Request) {
	_, ws, ok := a.wsAllowed(w, r, "logs")
	if !ok {
		return
	}
	tenant := ws.Tenant
	code, data, err := a.cpDo("GET", "/api/v1/logs/"+url.PathEscape(r.PathValue("id"))+"?tenantId="+url.QueryEscape(tenant), nil)
	cpRelay(w, code, data, err)
}
