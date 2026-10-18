package controlplane

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log/slog"

	"clusterheart/sre/internal/contract"
)

// Engine turns measured fault signals (reported by agents) into deduplicated,
// evidence-backed incidents with an open/resolved lifecycle. It is fully
// deterministic — no AI is involved in detection. Telemetry is treated as data,
// never as instructions.
type Engine struct {
	incidents IncidentStore
	notifier  Notifier
	log       *slog.Logger
}

func newEngine(incidents IncidentStore, notifier Notifier, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	if notifier == nil {
		notifier = noopNotifier{}
	}
	return &Engine{incidents: incidents, notifier: notifier, log: log}
}

// severityFor maps a problem type to a severity. These are conservative and
// deterministic; they can later be tuned per-policy.
func severityFor(t contract.ProblemType) string {
	switch t {
	case contract.ProblemOOMKilled, contract.ProblemCrashLoopBackOff, contract.ProblemRolloutStuck, contract.ProblemAgentDown:
		return "high"
	case contract.ProblemImagePullBackOff, contract.ProblemUnschedulable:
		return "medium"
	default:
		return "medium"
	}
}

// subject returns the stable workload/pod identity used for dedup: the
// controlling workload when known (so churned pods collapse into one incident),
// else the pod name.
func podSubject(p contract.PodProblem) string {
	if p.Owner != "" {
		return p.Owner
	}
	return "Pod/" + p.Pod
}

func fingerprint(clusterID, namespace, subject, container string, t contract.ProblemType) string {
	h := sha1.Sum([]byte(clusterID + "|" + namespace + "|" + subject + "|" + container + "|" + string(t)))
	return hex.EncodeToString(h[:])
}

// observe builds the set of observations for one snapshot.
func observe(clusterID string, snap contract.ClusterSnapshot) []observation {
	var obs []observation
	for _, p := range snap.PodProblems {
		subj := podSubject(p)
		ref := p.Namespace + "/" + p.Pod
		if p.Container != "" {
			ref += "/" + p.Container
		}
		obs = append(obs, observation{
			fingerprint: fingerprint(clusterID, p.Namespace, subj, p.Container, p.Type),
			typ:         p.Type,
			severity:    severityFor(p.Type),
			title:       fmt.Sprintf("%s: %s in %s", p.Type, subj, p.Namespace),
			namespace:   p.Namespace,
			workload:    p.Owner,
			node:        p.Node,
			evidence: EvidenceItem{
				ObservedAt: snap.CollectedAt, Ref: ref, Reason: string(p.Type),
				Message: p.Message, RestartCount: p.RestartCount,
			},
		})
	}
	for _, w := range snap.WorkloadProblems {
		subj := w.Kind + "/" + w.Name
		obs = append(obs, observation{
			fingerprint: fingerprint(clusterID, w.Namespace, subj, "", w.Type),
			typ:         w.Type,
			severity:    severityFor(w.Type),
			title:       fmt.Sprintf("%s: %s in %s", w.Type, subj, w.Namespace),
			namespace:   w.Namespace,
			workload:    subj,
			evidence: EvidenceItem{
				ObservedAt: snap.CollectedAt, Ref: w.Namespace + "/" + subj,
				Reason: string(w.Type), Message: w.Message,
			},
		})
	}
	return obs
}

// Reconcile detects incidents from a snapshot: it upserts an incident per active
// fault signal and resolves previously-open incidents that are no longer
// present. Idempotent per fingerprint (dedup).
func (e *Engine) Reconcile(ctx context.Context, tenantID, clusterID string, snap contract.ClusterSnapshot) error {
	obs := observe(clusterID, snap)
	active := make(map[string]bool, len(obs))
	for _, o := range obs {
		active[o.fingerprint] = true
		created, err := e.incidents.UpsertObservation(ctx, tenantID, clusterID, snap.CollectedAt, o)
		if err != nil {
			return err
		}
		if created {
			// Notify once, only when a NEW incident opens (dedup prevents storms).
			e.notifier.Notify(Notification{
				Kind: "incident.opened", Severity: o.severity, Title: o.title,
				Detail: o.evidence.Message, TenantID: tenantID, ClusterID: clusterID,
			})
		}
	}
	resolved, err := e.incidents.ResolveMissing(ctx, clusterID, active, snap.CollectedAt)
	if err != nil {
		return err
	}
	if len(obs) > 0 || resolved > 0 {
		e.log.Info("reconciled incidents", "clusterId", clusterID, "active", len(obs), "resolved", resolved)
	}
	return nil
}

// reconcileLatest runs detection on the most recent snapshot in a batch (current
// cluster state). Older buffered snapshots are stored but not re-detected, to
// avoid flapping incident lifecycles during reconnect flushes.
func (e *Engine) reconcileLatest(ctx context.Context, tenantID, clusterID string, snaps []contract.ClusterSnapshot) error {
	if len(snaps) == 0 {
		return nil
	}
	latest := snaps[0]
	for _, s := range snaps[1:] {
		if s.CollectedAt.After(latest.CollectedAt) {
			latest = s
		}
	}
	return e.Reconcile(ctx, tenantID, clusterID, latest)
}
