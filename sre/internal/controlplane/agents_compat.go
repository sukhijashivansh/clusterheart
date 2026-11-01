package controlplane

import (
	"net/http"

	"clusterheart/sre/internal/contract"
)

// handleAgentRevealsCompat answers agents up to 0.2.2 / host-0.2.1, which poll
// GET /api/v1/agents/reveals every few seconds. Revealing secret values was
// removed (findings only carry a masked hint), so there is never anything to
// do; newer agents don't poll.
func (s *Server) handleAgentRevealsCompat(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, contract.RevealRequestsResponse{Requests: []struct{}{}})
}
