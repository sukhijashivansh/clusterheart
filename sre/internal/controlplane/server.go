// Package controlplane implements the multi-tenant control-plane API: cluster
// registration, agent heartbeats, telemetry ingestion, and read-back for
// operators. Milestone 1 is observe-only plumbing — no detection, AI, policy,
// or remediation lives here yet (those are later milestones).
package controlplane

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"clusterheart/sre/internal/contract"
)

// staleAfter is how long without a heartbeat before a cluster is reported
// disconnected.
const staleAfter = 90 * time.Second

// maxSnapshotsPerBatch bounds a single telemetry POST (backpressure / abuse).
const maxSnapshotsPerBatch = 50

// Server holds control-plane dependencies.
type Server struct {
	store      Store
	adminToken string
	log        *slog.Logger
	now        func() time.Time
}

// NewServer constructs a control-plane server. adminToken guards operator
// endpoints; it must be non-empty in production (enforced by the caller).
func NewServer(store Store, adminToken string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{store: store, adminToken: adminToken, log: log, now: time.Now}
}

// Routes returns the HTTP handler for the control plane.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	// Operator / admin.
	mux.HandleFunc("POST /api/v1/clusters", s.requireAdmin(s.handleRegister))
	mux.HandleFunc("GET /api/v1/clusters", s.requireAdmin(s.handleListClusters))
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
	// Telemetry is written ONLY for the authenticated cluster — never a cluster
	// id taken from the request body (tenant isolation at the boundary).
	if err := s.store.SaveSnapshots(r.Context(), c.ID, batch.Snapshots); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not store telemetry")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": len(batch.Snapshots)})
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
