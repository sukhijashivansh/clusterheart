package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"time"

	"clusterheart/sre/internal/contract"
)

// Version is the agent build version, reported in heartbeats for compatibility.
const Version = "0.2.2"

// maxBuffer bounds how many snapshots the agent holds during a control-plane
// outage. Oldest snapshots are dropped first (bounded telemetry, no unbounded
// memory growth - spec §3B, §18).
const maxBuffer = 60

// Client talks to the control plane as a single registered cluster.
type Client struct {
	baseURL   string
	clusterID string
	token     string
	http      *http.Client
}

// NewClient constructs an authenticated control-plane client.
func NewClient(baseURL, clusterID, token string) *Client {
	return &Client{
		baseURL:   baseURL,
		clusterID: clusterID,
		token:     token,
		http:      &http.Client{Timeout: 20 * time.Second},
	}
}

func (c *Client) post(ctx context.Context, path string, body any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Cluster-ID", c.clusterID)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("control plane %s -> %d: %s", path, resp.StatusCode, string(b))
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Cluster-ID", c.clusterID)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("control plane %s -> %d: %s", path, resp.StatusCode, string(b))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// RevealRequests returns pending reveal requests for this cluster.
func (c *Client) RevealRequests(ctx context.Context) ([]contract.RevealRequest, error) {
	var out contract.RevealRequestsResponse
	if err := c.get(ctx, "/api/v1/agents/reveals", &out); err != nil {
		return nil, err
	}
	return out.Requests, nil
}

// SendReveal answers one reveal request.
func (c *Client) SendReveal(ctx context.Context, id string, res contract.RevealResult) error {
	return c.post(ctx, "/api/v1/agents/reveals/"+url.PathEscape(id), res)
}

// Heartbeat reports agent liveness and health.
func (c *Client) Heartbeat(ctx context.Context, hb contract.Heartbeat) error {
	return c.post(ctx, "/api/v1/agents/heartbeat", hb)
}

// SendTelemetry flushes a batch of snapshots.
func (c *Client) SendTelemetry(ctx context.Context, snaps []contract.ClusterSnapshot) error {
	return c.post(ctx, "/api/v1/telemetry", contract.TelemetryBatch{
		APIVersion: contract.APIVersion,
		Snapshots:  snaps,
	})
}

// Runner drives the collect → buffer → flush → heartbeat loop.
type Runner struct {
	Client    *Client
	Collector *Collector
	Interval  time.Duration
	Log       *slog.Logger

	buffer     []contract.ClusterSnapshot
	failures   int
	collectErr int
}

// Run blocks until ctx is cancelled, collecting on Interval and backing off on
// control-plane errors.
func (r *Runner) Run(ctx context.Context) {
	if r.Interval <= 0 {
		r.Interval = 30 * time.Second
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	go r.revealLoop(ctx)
	for {
		if ctx.Err() != nil {
			return
		}
		r.collectOnce(ctx)
		ok := r.flushAndBeat(ctx)
		if ok {
			r.failures = 0
			sleep(ctx, jitter(r.Interval))
		} else {
			r.failures++
			d := backoff(r.failures)
			r.Log.Warn("control plane unreachable; backing off", "failures", r.failures, "retryIn", d.String(), "buffered", len(r.buffer))
			sleep(ctx, d)
		}
	}
}

func (r *Runner) collectOnce(ctx context.Context) {
	snap, err := r.Collector.Snapshot(ctx)
	if err != nil {
		r.collectErr++
		r.Log.Error("snapshot collection failed", "err", err)
		return
	}
	// Only the newest buffered snapshot keeps its log tails and secret findings, so a backlog after an
	// outage can't grow the telemetry batch past the control plane's body limit.
	for i := range r.buffer {
		r.buffer[i].PodLogs = nil
		r.buffer[i].SecretFindings = nil // cumulative: the newest snapshot has them all
	}
	r.buffer = append(r.buffer, snap)
	if len(r.buffer) > maxBuffer {
		r.buffer = r.buffer[len(r.buffer)-maxBuffer:] // drop oldest
	}
}

func (r *Runner) flushAndBeat(ctx context.Context) bool {
	ok := true
	if len(r.buffer) > 0 {
		if err := r.Client.SendTelemetry(ctx, r.buffer); err != nil {
			ok = false
		} else {
			r.buffer = r.buffer[:0]
		}
	}
	hb := contract.Heartbeat{
		APIVersion:        contract.APIVersion,
		AgentVersion:      Version,
		K8sVersion:        r.Collector.K8sVersion(),
		SentAt:            time.Now().UTC(),
		BufferedSnapshots: len(r.buffer),
		CollectErrors:     r.collectErr,
	}
	if err := r.Client.Heartbeat(ctx, hb); err != nil {
		ok = false
	}
	return ok
}

// backoff returns an exponential, jittered, capped delay for the Nth failure.
func backoff(failures int) time.Duration {
	const base, cap = 2 * time.Second, 60 * time.Second
	d := base << min(failures-1, 5) // 2,4,8,16,32,64->cap
	if d > cap {
		d = cap
	}
	return jitter(d)
}

// jitter applies +/-20% randomness so agents don't synchronize (thundering herd).
func jitter(d time.Duration) time.Duration {
	delta := float64(d) * 0.2
	return time.Duration(float64(d) - delta + rand.Float64()*2*delta)
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// revealInterval is how often the agent checks for reveal requests, so a
// person clicking Reveal waits seconds, not a whole collection cycle.
const revealInterval = 5 * time.Second

// revealLoop answers reveal requests with values from the agent's memory. A
// value is sent only in reply to a request a person made in the dashboard.
func (r *Runner) revealLoop(ctx context.Context) {
	for {
		sleep(ctx, revealInterval)
		if ctx.Err() != nil {
			return
		}
		reqs, err := r.Client.RevealRequests(ctx)
		if err != nil {
			continue // control plane down or older: try again later
		}
		for _, q := range reqs {
			res := contract.RevealResult{}
			if v, ok := r.Collector.SecretValue(q.Ref); ok {
				res.Value = v
			} else {
				res.Error = "This secret is no longer visible to the agent (fixed, or not seen since the agent restarted)."
			}
			if err := r.Client.SendReveal(ctx, q.ID, res); err != nil {
				r.Log.Warn("could not answer reveal request", "id", q.ID, "err", err) // never log the value
			}
		}
	}
}
