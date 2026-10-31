package hostagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"clusterheart/sre/internal/contract"
)

// Version is reported in heartbeats.
const Version = "host-0.1.0"

const (
	maxJournalLines = 2000 // per cycle
	maxErrorLogs    = 50   // kept and sent
	errorLogTTL     = 24 * time.Hour
	maxBuffer       = 20 // snapshots held while the control plane is unreachable
	revealInterval  = 5 * time.Second
)

// Client talks to the control plane with the server's agent token.
type Client struct {
	base, clusterID, token string
	http                   *http.Client
}

func NewClient(base, clusterID, token string) *Client {
	return &Client{base: base, clusterID: clusterID, token: token, http: &http.Client{Timeout: 20 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Cluster-ID", c.clusterID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("control plane %s -> %d: %s", path, resp.StatusCode, b)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	return nil
}

// Agent collects host snapshots on an interval and sends them.
type Agent struct {
	Client   *Client
	Interval time.Duration
	Log      *slog.Logger

	sys     *sys
	run     runFunc
	secrets *secretScanner
	cursor  string
	errLogs []contract.HostLogLine
	buffer  []contract.ClusterSnapshot
}

// New builds an agent reading the real /proc, /etc, systemd and journal.
func New(c *Client, interval time.Duration, log *slog.Logger) *Agent {
	return &Agent{Client: c, Interval: interval, Log: log,
		sys: &sys{proc: "/proc", etc: "/etc", statfs: statfsMi}, run: execRun, secrets: newSecretScanner()}
}

// collect builds one snapshot.
func (a *Agent) collect(ctx context.Context, now time.Time) contract.ClusterSnapshot {
	h := a.sys.sample(now)
	a.secrets.host = h.Hostname
	h.FailedUnits = failedUnits(ctx, a.run)

	entries, cursor := readJournal(ctx, a.run, a.cursor)
	a.cursor = cursor
	for _, e := range entries {
		if e.priority <= 3 { // emerg, alert, crit, err
			a.errLogs = append(a.errLogs, redactedLogLine(e))
		}
	}
	kept := a.errLogs[:0]
	for _, l := range a.errLogs {
		if l.Time.IsZero() || now.Sub(l.Time) <= errorLogTTL {
			kept = append(kept, l)
		}
	}
	if len(kept) > maxErrorLogs {
		kept = kept[len(kept)-maxErrorLogs:]
	}
	a.errLogs = kept
	h.ErrorLogs = append([]contract.HostLogLine(nil), a.errLogs...)

	a.secrets.recordJournal(entries, now)
	a.secrets.scanUnitEnv(unitEnv(ctx, a.run), now)
	h.Problems = problems(h)
	return contract.ClusterSnapshot{CollectedAt: now.UTC(), Host: &h, SecretFindings: a.secrets.findings(now)}
}

// Run collects and sends until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) {
	if a.Interval <= 0 {
		a.Interval = 30 * time.Second
	}
	go a.revealLoop(ctx)
	for ctx.Err() == nil {
		a.cycle(ctx)
		select {
		case <-ctx.Done():
		case <-time.After(a.Interval):
		}
	}
}

func (a *Agent) cycle(ctx context.Context) {
	snap := a.collect(ctx, time.Now())
	// Only the newest buffered snapshot keeps logs and findings (bounded batches).
	for i := range a.buffer {
		a.buffer[i].SecretFindings = nil
		if a.buffer[i].Host != nil {
			a.buffer[i].Host.ErrorLogs = nil
		}
	}
	a.buffer = append(a.buffer, snap)
	if len(a.buffer) > maxBuffer {
		a.buffer = a.buffer[len(a.buffer)-maxBuffer:]
	}
	if err := a.Client.do(ctx, http.MethodPost, "/api/v1/telemetry", contract.TelemetryBatch{APIVersion: contract.APIVersion, Snapshots: a.buffer}, nil); err != nil {
		a.Log.Warn("could not send snapshot; will retry", "buffered", len(a.buffer), "err", err)
	} else {
		a.buffer = a.buffer[:0]
	}
	hb := contract.Heartbeat{APIVersion: contract.APIVersion, AgentVersion: Version, SentAt: time.Now().UTC(), BufferedSnapshots: len(a.buffer)}
	if err := a.Client.do(ctx, http.MethodPost, "/api/v1/agents/heartbeat", hb, nil); err != nil {
		a.Log.Warn("heartbeat failed", "err", err)
	}
}

// revealLoop answers reveal requests from the dashboard with values from
// memory. The value is never logged.
func (a *Agent) revealLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(revealInterval):
		}
		var q contract.RevealRequestsResponse
		if err := a.Client.do(ctx, http.MethodGet, "/api/v1/agents/reveals", nil, &q); err != nil {
			continue
		}
		for _, r := range q.Requests {
			res := contract.RevealResult{}
			if v, ok := a.secrets.Value(r.Ref); ok {
				res.Value = v
			} else {
				res.Error = "This secret is no longer visible to the agent (fixed, or not seen since the agent restarted)."
			}
			if err := a.Client.do(ctx, http.MethodPost, "/api/v1/agents/reveals/"+url.PathEscape(r.ID), res, nil); err != nil {
				a.Log.Warn("could not answer reveal request", "id", r.ID, "err", err)
			}
		}
	}
}
