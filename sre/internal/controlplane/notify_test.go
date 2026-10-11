package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"clusterheart/sre/internal/contract"
)

func TestWebhookNotifierGenericSend(t *testing.T) {
	got := make(chan Notification, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var n Notification
		_ = json.Unmarshal(b, &n)
		got <- n
	}))
	defer ts.Close()
	wn := &webhookNotifier{url: ts.URL, slack: false, client: ts.Client()}
	if err := wn.send(Notification{Kind: "incident.opened", Title: "x", TenantID: "t1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-got:
		if n.Kind != "incident.opened" || n.TenantID != "t1" {
			t.Fatalf("bad payload: %+v", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no webhook delivery")
	}
}

func TestWebhookNotifierSlackFormat(t *testing.T) {
	got := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &payload)
		got <- payload["text"]
	}))
	defer ts.Close()
	wn := &webhookNotifier{url: ts.URL, slack: true, client: ts.Client()}
	if err := wn.send(Notification{Kind: "remediation.failed", Title: "RolloutRestart on Deployment/web/api", Detail: "failed: boom", TenantID: "t1", ClusterID: "cluA"}); err != nil {
		t.Fatal(err)
	}
	text := <-got
	if !strings.Contains(text, ":x:") || !strings.Contains(text, "remediation.failed") || !strings.Contains(text, "cluster=cluA") {
		t.Fatalf("unexpected slack text: %q", text)
	}
}

func TestNewNotifierNoopWhenUnconfigured(t *testing.T) {
	// No env set in the test process → no-op.
	if _, ok := newNotifier(nil).(noopNotifier); !ok {
		t.Fatal("expected no-op notifier when no channel is configured")
	}
}

// recordNotifier captures notifications synchronously for engine/service tests.
type recordNotifier struct{ got []Notification }

func (r *recordNotifier) Notify(n Notification) { r.got = append(r.got, n) }

// The engine must notify exactly once when a NEW incident opens, and NOT on
// subsequent occurrences of the same incident (no notification storms).
func TestEngineNotifiesOncePerIncident(t *testing.T) {
	rec := &recordNotifier{}
	eng := newEngine(NewMemIncidentStore(), rec, nil)
	snap := snapWithProblems(time.Now(),
		[]contract.PodProblem{{Namespace: "web", Pod: "api-1", Owner: "Deployment/api", Container: "api", Type: contract.ProblemCrashLoopBackOff, Message: "boom"}}, nil)
	ctx := context.Background()
	if err := eng.Reconcile(ctx, "t1", "cluA", snap); err != nil {
		t.Fatal(err)
	}
	// Same fault again → updates the incident, must NOT re-notify.
	snap2 := snapWithProblems(time.Now().Add(time.Minute),
		[]contract.PodProblem{{Namespace: "web", Pod: "api-1", Owner: "Deployment/api", Container: "api", Type: contract.ProblemCrashLoopBackOff, Message: "boom again"}}, nil)
	if err := eng.Reconcile(ctx, "t1", "cluA", snap2); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 1 {
		t.Fatalf("expected exactly 1 notification, got %d: %+v", len(rec.got), rec.got)
	}
	if rec.got[0].Kind != "incident.opened" || rec.got[0].Severity != "high" {
		t.Fatalf("unexpected notification: %+v", rec.got[0])
	}
}
