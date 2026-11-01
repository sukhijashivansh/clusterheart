package controlplane

import (
	"net/http"
	"testing"
)

// Older agents keep polling for reveal requests; they get an empty list. The
// endpoints that created and answered reveals are gone.
func TestRevealRemoved(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	reg := register(t, ts.URL, "acme", "prod")

	code, body := do(t, http.MethodGet, ts.URL+"/api/v1/agents/reveals", reg.AgentToken, reg.ClusterID, nil)
	if code != http.StatusOK || string(body) != "{\"requests\":[]}\n" {
		t.Fatalf("old agent poll: %d %q", code, body)
	}
	if code, _ := do(t, http.MethodGet, ts.URL+"/api/v1/agents/reveals", "wrong", reg.ClusterID, nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated poll: %d", code)
	}
	for _, c := range []struct{ method, path, tok, cid string }{
		{http.MethodPost, "/api/v1/clusters/" + reg.ClusterID + "/reveals?tenantId=acme", adminTok, ""},
		{http.MethodGet, "/api/v1/reveals/rvl_1?tenantId=acme", adminTok, ""},
		{http.MethodPost, "/api/v1/agents/reveals/rvl_1", reg.AgentToken, reg.ClusterID},
	} {
		if code, _ := do(t, c.method, ts.URL+c.path, c.tok, c.cid, map[string]string{"ref": "x", "value": "y"}); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s still answers: %d", c.method, c.path, code)
		}
	}
}
