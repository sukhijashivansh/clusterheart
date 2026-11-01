package hostagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"clusterheart/sre/internal/contract"
)

// Version is reported in heartbeats.
const Version = "host-0.2.2"

const (
	maxJournalLines = 2000 // per cycle
	maxErrorLogs    = 50   // kept and sent
	errorLogTTL     = 24 * time.Hour
	maxBuffer       = 20 // snapshots held while the control plane is unreachable
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
	defer func() {
		// Drain so the connection is reused instead of a new TLS handshake
		// for every request.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	}()
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
	gpus    *gpuReader
	secrets *secretScanner
	cursor  string
	// oldJournal is set once journalctl rejected --output-fields (systemd < 236).
	oldJournal bool
	errLogs    []contract.HostLogLine
	buffer     []contract.ClusterSnapshot
}

// New builds an agent reading the real /proc, /etc, systemd and journal.
func New(c *Client, interval time.Duration, log *slog.Logger) *Agent {
	return &Agent{Client: c, Interval: interval, Log: log,
		sys: &sys{proc: "/proc", etc: "/etc", sysfs: "/sys", statfs: statfsMi, uidOf: fileOwner}, run: execRun,
		gpus: newGPUReader(execRun, execStart, "/sys"), secrets: newSecretScanner()}
}

// collect builds one snapshot.
func (a *Agent) collect(ctx context.Context, now time.Time) contract.ClusterSnapshot {
	h := a.sys.sample(now)
	a.secrets.host = h.Hostname
	h.FailedUnits = failedUnits(ctx, a.run)
	var gpuProbs []contract.HostProblem
	if a.gpus != nil {
		h.GPUs, gpuProbs = a.gpus.read(ctx, now)
	}

	entries, cursor, err := readJournal(ctx, a.run, a.cursor, !a.oldJournal)
	if err != nil && !a.oldJournal {
		a.oldJournal = true
		entries, cursor, _ = readJournal(ctx, a.run, a.cursor, false)
	}
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
	h.Problems = append(problems(h), gpuProbs...)
	return contract.ClusterSnapshot{CollectedAt: now.UTC(), Host: &h, SecretFindings: a.secrets.findings(now)}
}

// Run collects and sends until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) {
	if a.Interval <= 0 {
		a.Interval = 30 * time.Second
	}
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
