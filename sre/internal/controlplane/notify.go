package controlplane

// Outbound notifications (spec §11). Fire-and-forget: delivery never blocks or
// fails a detection/remediation path — problems are logged. Channels are opt-in
// via env; with none configured the notifier is a no-op. Notification content is
// built from already-redacted incident/remediation fields.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// Notification is an operationally significant event worth telling an operator.
type Notification struct {
	Kind      string `json:"kind"` // incident.opened | remediation.executed | remediation.failed
	Severity  string `json:"severity,omitempty"`
	Title     string `json:"title"`
	Detail    string `json:"detail,omitempty"`
	TenantID  string `json:"tenantId"`
	ClusterID string `json:"clusterId,omitempty"`
}

// Notifier delivers notifications. Notify must not block the caller and never
// returns an error (delivery issues are logged).
type Notifier interface {
	Notify(n Notification)
}

type noopNotifier struct{}

func (noopNotifier) Notify(Notification) {}

// newNotifier builds a Notifier from env, or a no-op when nothing is configured:
//
//	SRE_SLACK_WEBHOOK_URL  Slack incoming webhook (posts {"text": ...})
//	SRE_WEBHOOK_URL        generic JSON webhook (posts the Notification)
func newNotifier(log *slog.Logger) Notifier {
	if log == nil {
		log = slog.Default()
	}
	var ns []Notifier
	if u := strings.TrimSpace(os.Getenv("SRE_SLACK_WEBHOOK_URL")); u != "" {
		ns = append(ns, &webhookNotifier{url: u, slack: true, log: log, client: &http.Client{Timeout: 8 * time.Second}})
	}
	if u := strings.TrimSpace(os.Getenv("SRE_WEBHOOK_URL")); u != "" {
		ns = append(ns, &webhookNotifier{url: u, slack: false, log: log, client: &http.Client{Timeout: 8 * time.Second}})
	}
	switch len(ns) {
	case 0:
		return noopNotifier{}
	case 1:
		return ns[0]
	default:
		return multiNotifier(ns)
	}
}

type multiNotifier []Notifier

func (m multiNotifier) Notify(n Notification) {
	for _, x := range m {
		x.Notify(n)
	}
}

// webhookNotifier posts to Slack (as {text}) or a generic endpoint (raw
// Notification JSON). The HTTP call runs in a goroutine so Notify returns at once.
type webhookNotifier struct {
	url    string
	slack  bool
	log    *slog.Logger
	client *http.Client
}

func (w *webhookNotifier) Notify(n Notification) {
	go func() {
		if err := w.send(n); err != nil {
			w.log.Warn("notification delivery failed", "kind", n.Kind, "err", err)
		}
	}()
}

// send performs the synchronous POST (separated out so it is directly testable).
func (w *webhookNotifier) send(n Notification) error {
	var body []byte
	if w.slack {
		body, _ = json.Marshal(map[string]string{"text": slackText(n)})
	} else {
		body, _ = json.Marshal(n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}

func slackText(n Notification) string {
	icon := ":rotating_light:"
	switch n.Kind {
	case "remediation.executed":
		icon = ":white_check_mark:"
	case "remediation.failed":
		icon = ":x:"
	}
	s := fmt.Sprintf("%s *ClusterHeart SRE* [%s] %s", icon, n.Kind, n.Title)
	if n.Detail != "" {
		s += "\n" + n.Detail
	}
	meta := "tenant=" + n.TenantID
	if n.ClusterID != "" {
		meta += " cluster=" + n.ClusterID
	}
	return s + "\n_" + meta + "_"
}
