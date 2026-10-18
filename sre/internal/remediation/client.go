package remediation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"clusterheart/sre/internal/contract"
)

// ControlPlaneClient talks to the control plane as the remediation controller,
// authenticating with the cluster's SEPARATE remediation token.
type ControlPlaneClient struct {
	baseURL   string
	clusterID string
	token     string
	http      *http.Client
}

// NewControlPlaneClient constructs the client.
func NewControlPlaneClient(baseURL, clusterID, token string) *ControlPlaneClient {
	return &ControlPlaneClient{
		baseURL: baseURL, clusterID: clusterID, token: token,
		http: &http.Client{Timeout: 20 * time.Second},
	}
}

func (c *ControlPlaneClient) req(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Cluster-ID", c.clusterID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

// Pending fetches this cluster's approved work queue.
func (c *ControlPlaneClient) Pending(ctx context.Context) ([]contract.PendingAction, error) {
	resp, err := c.req(ctx, http.MethodGet, "/api/v1/remediation/pending", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("pending -> %d: %s", resp.StatusCode, string(b))
	}
	var out contract.PendingActionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Actions, nil
}

// Report posts the outcome of an executed action.
func (c *ControlPlaneClient) Report(ctx context.Context, proposalID string, res contract.ActionResult) error {
	resp, err := c.req(ctx, http.MethodPost, "/api/v1/remediation/"+proposalID+"/result", res)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("report -> %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
