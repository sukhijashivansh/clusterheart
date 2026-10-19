package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// Agent credentials are long random bearer tokens. The control plane stores only
// their SHA-256 hash and compares in constant time. mTLS with per-cluster certs
// is the planned hardening step (see threat-model.md); bearer-over-TLS is the
// Milestone-1 baseline.

type ctxKey int

const clusterCtxKey ctxKey = 1

// mintAgentToken returns a new random token and its hash. The plaintext is
// returned to the operator exactly once at registration.
func mintAgentToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(b)
	hash = hashToken(token)
	return token, hash, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func constantEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return ""
}

// requireAdmin guards operator endpoints with the bootstrap admin token. In a
// later milestone this integrates with the existing account server / RBAC.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.adminToken == "" || !constantEq(bearer(r), s.adminToken) {
			writeErr(w, http.StatusUnauthorized, "admin authentication required")
			return
		}
		next(w, r)
	}
}

// requireAgent authenticates a cluster agent and pins the request to that
// cluster. Handlers downstream MUST use clusterFromCtx - never a cluster id
// from the request body - so an agent for cluster A can never write as cluster
// B (tenant isolation at the trust boundary).
func (s *Server) requireAgent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterID := r.Header.Get("X-Cluster-ID")
		tok := bearer(r)
		if clusterID == "" || tok == "" {
			writeErr(w, http.StatusUnauthorized, "agent credentials required")
			return
		}
		c, err := s.store.GetCluster(r.Context(), clusterID)
		if err != nil {
			// Do not distinguish "no such cluster" from "bad token" to callers.
			writeErr(w, http.StatusUnauthorized, "invalid agent credentials")
			return
		}
		if !constantEq(hashToken(tok), c.AgentTokenHash) {
			writeErr(w, http.StatusUnauthorized, "invalid agent credentials")
			return
		}
		ctx := context.WithValue(r.Context(), clusterCtxKey, c)
		next(w, r.WithContext(ctx))
	}
}

// requireRemediator authenticates the in-cluster remediation controller with
// the cluster's SEPARATE remediation token (distinct from the observe agent
// token). Only set when an operator has opted the cluster into remediation.
func (s *Server) requireRemediator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterID := r.Header.Get("X-Cluster-ID")
		tok := bearer(r)
		if clusterID == "" || tok == "" {
			writeErr(w, http.StatusUnauthorized, "remediation credentials required")
			return
		}
		c, err := s.store.GetCluster(r.Context(), clusterID)
		if err != nil || c.RemediationTokenHash == "" || !constantEq(hashToken(tok), c.RemediationTokenHash) {
			writeErr(w, http.StatusUnauthorized, "invalid remediation credentials")
			return
		}
		ctx := context.WithValue(r.Context(), clusterCtxKey, c)
		next(w, r.WithContext(ctx))
	}
}

// clusterFromCtx returns the authenticated cluster pinned by requireAgent.
func clusterFromCtx(ctx context.Context) (Cluster, error) {
	c, ok := ctx.Value(clusterCtxKey).(Cluster)
	if !ok {
		return Cluster{}, errors.New("no authenticated cluster in context")
	}
	return c, nil
}
