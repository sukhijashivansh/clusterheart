package controlplane

// AI-assisted root-cause analysis (spec §6, §15). Design rules enforced here:
//
//   - The AI only ever produces a STRUCTURED, schema-validated Diagnosis. It
//     never emits shell or patches, and a recommended action is advisory only —
//     it does NOT create or authorize a remediation. A human/policy still runs
//     the deterministic CreateProposal path, so a hallucinated target can never
//     cause a mutation.
//   - A DETERMINISTIC diagnoser is always available and is the default. External
//     AI is opt-in (disabled unless configured) and, on any error, the service
//     falls back to the deterministic result — graceful degradation.
//   - Telemetry (evidence messages) is treated as untrusted DATA, never
//     instructions (injection hardening lives in the LLM diagnoser).
//   - Every diagnosis distinguishes measured FACTS from inferred HYPOTHESES.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"clusterheart/sre/internal/contract"
)

// Confidence is a coarse, explainable confidence level.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// Hypothesis is one ranked root-cause candidate with its supporting and
// contradicting evidence (spec §6: never present an unverified hypothesis as an
// established root cause).
type Hypothesis struct {
	Cause         string     `json:"cause" bson:"cause"`
	Confidence    Confidence `json:"confidence" bson:"confidence"`
	Supporting    []string   `json:"supporting" bson:"supporting"`
	Contradicting []string   `json:"contradicting,omitempty" bson:"contradicting,omitempty"`
}

// Diagnosis is the evidence-backed analysis attached to an incident.
type Diagnosis struct {
	ID                    string       `json:"id" bson:"_id"`
	IncidentID            string       `json:"incidentId" bson:"incidentId"`
	TenantID              string       `json:"tenantId" bson:"tenantId"`
	Summary               string       `json:"summary" bson:"summary"`
	ObservedFacts         []string     `json:"observedFacts" bson:"observedFacts"`
	Hypotheses            []Hypothesis `json:"hypotheses" bson:"hypotheses"`
	RecommendedAction     string       `json:"recommendedAction" bson:"recommendedAction"` // an ActionType, or "investigate"/"escalate"
	RecommendedActionable bool         `json:"recommendedActionable" bson:"recommendedActionable"`
	RecommendedTarget     *TargetRef   `json:"recommendedTarget,omitempty" bson:"recommendedTarget,omitempty"`
	Risks                 string       `json:"risks" bson:"risks"`
	VerificationCriteria  []string     `json:"verificationCriteria" bson:"verificationCriteria"`
	Rollback              string       `json:"rollback" bson:"rollback"`
	Confidence            Confidence   `json:"confidence" bson:"confidence"`
	Source                string       `json:"source" bson:"source"` // "deterministic" | "llm"
	Model                 string       `json:"model,omitempty" bson:"model,omitempty"`
	PromptVersion         string       `json:"promptVersion,omitempty" bson:"promptVersion,omitempty"`
	LatencyMS             int64        `json:"latencyMs" bson:"latencyMs"`
	CreatedAt             time.Time    `json:"createdAt" bson:"createdAt"`
}

// Diagnoser produces a Diagnosis for an incident. Implementations must be safe
// to call with untrusted evidence content.
type Diagnoser interface {
	Diagnose(ctx context.Context, inc Incident) (Diagnosis, error)
	Name() string
}

// --- deterministic diagnoser --------------------------------------------

// deterministicDiagnoser maps known incident types to evidence-backed,
// rule-derived diagnoses. No network, always available, fully testable.
type deterministicDiagnoser struct{}

func (deterministicDiagnoser) Name() string { return "deterministic" }

func (d deterministicDiagnoser) Diagnose(_ context.Context, inc Incident) (Diagnosis, error) {
	dg := Diagnosis{
		Source:        "deterministic",
		ObservedFacts: observedFacts(inc),
		Confidence:    ConfidenceMedium,
	}
	kind, name := splitWorkload(inc.Workload)
	target := &TargetRef{Kind: kind, Namespace: inc.Namespace, Name: name}

	switch inc.Type {
	case contract.ProblemCrashLoopBackOff:
		dg.Summary = fmt.Sprintf("%s is crash-looping in %s: the container starts then exits repeatedly.", inc.Workload, inc.Namespace)
		dg.Hypotheses = []Hypothesis{
			{Cause: "Application fails on startup (bad config, missing dependency, failed migration, or a panic).", Confidence: ConfidenceMedium,
				Supporting: []string{"repeated container restarts with short uptime", "BackOff reported by the kubelet"}},
			{Cause: "A recent rollout shipped a broken image or configuration.", Confidence: ConfidenceLow,
				Supporting: []string{"crash loop began recently"}, Contradicting: []string{"no deploy timestamp correlated yet (changes feed not wired)"}},
			{Cause: "Failing liveness probe is killing a healthy process.", Confidence: ConfidenceLow,
				Supporting: []string{"restart-driven, not OOM"}},
		}
		dg.RecommendedAction, dg.RecommendedActionable, dg.RecommendedTarget = string(ActionRolloutRestart), true, target
		dg.Risks = "A rollout restart clears transient bad state but will not fix a genuinely broken image/config; it may briefly reduce capacity."
		dg.VerificationCriteria = []string{"restart count stops increasing", "pods reach Ready and stay Ready", "incident auto-resolves on the next telemetry cycle"}
		dg.Rollback = "Reversible: `kubectl rollout undo` restores the previous revision if the restart makes things worse."
	case contract.ProblemOOMKilled:
		dg.Summary = fmt.Sprintf("%s is being OOMKilled in %s: the container exceeds its memory limit.", inc.Workload, inc.Namespace)
		dg.Hypotheses = []Hypothesis{
			{Cause: "Memory limit is set too low for real demand.", Confidence: ConfidenceMedium, Supporting: []string{"kernel OOM kill on the container"}},
			{Cause: "Application memory leak grows working set until the limit is hit.", Confidence: ConfidenceMedium, Supporting: []string{"recurring OOM over time"}},
		}
		dg.RecommendedAction, dg.RecommendedActionable = "investigate", false
		dg.Risks = "A restart only defers the next OOM. Raising the limit without headroom analysis can mask a leak or starve the node."
		dg.VerificationCriteria = []string{"no further OOMKilled events", "working-set stays below the limit under normal load"}
		dg.Rollback = "N/A — recommend investigation (memory rightsizing) rather than an automatic mutation."
	case contract.ProblemImagePullBackOff:
		dg.Summary = fmt.Sprintf("%s cannot pull its image in %s.", inc.Workload, inc.Namespace)
		dg.Hypotheses = []Hypothesis{
			{Cause: "Image reference is wrong (typo, missing tag, or deleted image).", Confidence: ConfidenceMedium, Supporting: []string{"ImagePullBackOff reported"}},
			{Cause: "Registry authentication/imagePullSecret is missing or expired.", Confidence: ConfidenceMedium, Supporting: []string{"pull denied by registry"}},
		}
		dg.RecommendedAction, dg.RecommendedActionable = "investigate", false
		dg.Risks = "A rollout restart will NOT fix a bad image reference or missing pull secret — fix the manifest/secret first."
		dg.VerificationCriteria = []string{"image pulls succeed", "pods leave ImagePullBackOff"}
		dg.Rollback = "N/A — manifest/secret fix required."
	case contract.ProblemUnschedulable:
		dg.Summary = fmt.Sprintf("Pods for %s cannot be scheduled in %s.", inc.Workload, inc.Namespace)
		dg.Hypotheses = []Hypothesis{
			{Cause: "Insufficient cluster capacity (CPU/memory) for the pod's requests.", Confidence: ConfidenceMedium, Supporting: []string{"no node satisfies requests"}},
			{Cause: "Affinity, taints/tolerations, topology, or quota prevent placement.", Confidence: ConfidenceLow, Supporting: []string{"scheduling predicate failures"}},
		}
		dg.RecommendedAction, dg.RecommendedActionable = "investigate", false
		dg.Risks = "Restarting won't create capacity. Adding nodes or relaxing constraints should be deliberate."
		dg.VerificationCriteria = []string{"pending pods get scheduled", "no Unschedulable events"}
		dg.Rollback = "N/A — capacity/scheduling change required."
	case contract.ProblemRolloutStuck:
		dg.Summary = fmt.Sprintf("Rollout of %s is stuck in %s: the new revision is not becoming healthy.", inc.Workload, inc.Namespace)
		dg.Hypotheses = []Hypothesis{
			{Cause: "The new revision is unhealthy (crash, failing readiness, bad config).", Confidence: ConfidenceMedium, Supporting: []string{"progress deadline exceeded"}},
			{Cause: "Insufficient capacity to bring up the updated pods.", Confidence: ConfidenceLow, Supporting: []string{"updated replicas not becoming available"}},
		}
		dg.RecommendedAction, dg.RecommendedActionable, dg.RecommendedTarget = string(ActionRolloutRestart), true, target
		dg.Risks = "If the new revision itself is broken, a restart re-rolls the same bad spec; a rollback may be the correct fix."
		dg.VerificationCriteria = []string{"updated replicas become available", "rollout completes (observedGeneration catches up)"}
		dg.Rollback = "Reversible via `kubectl rollout undo`."
	default:
		dg.Summary = fmt.Sprintf("%s incident on %s in %s.", inc.Type, inc.Workload, inc.Namespace)
		dg.Hypotheses = []Hypothesis{{Cause: "Unclassified problem type; manual investigation required.", Confidence: ConfidenceLow}}
		dg.RecommendedAction, dg.RecommendedActionable = "investigate", false
		dg.Confidence = ConfidenceLow
		dg.Risks = "No deterministic rule for this type."
		dg.VerificationCriteria = []string{"operator confirms resolution"}
		dg.Rollback = "N/A"
	}
	return dg, nil
}

// observedFacts turns the incident's measured state into plain statements of
// FACT (distinct from the inferred hypotheses).
func observedFacts(inc Incident) []string {
	facts := []string{
		fmt.Sprintf("Type %s, severity %s, status %s.", inc.Type, inc.Severity, inc.Status),
		fmt.Sprintf("Seen %d time(s) between %s and %s.", inc.Occurrences,
			inc.FirstSeen.UTC().Format(time.RFC3339), inc.LastSeen.UTC().Format(time.RFC3339)),
	}
	// Up to 3 most-recent evidence lines (already redacted upstream).
	ev := inc.Evidence
	for i := len(ev) - 1; i >= 0 && len(facts) < 5; i-- {
		e := ev[i]
		line := fmt.Sprintf("%s: %s", e.Ref, e.Message)
		if e.RestartCount > 0 {
			line += fmt.Sprintf(" (restarts=%d)", e.RestartCount)
		}
		facts = append(facts, line)
	}
	return facts
}

func splitWorkload(w string) (kind, name string) {
	if i := strings.Index(w, "/"); i >= 0 {
		return w[:i], w[i+1:]
	}
	return "", w
}

// --- diagnosis store -----------------------------------------------------

// DiagnosisStore persists the latest diagnosis per incident, tenant-scoped.
type DiagnosisStore interface {
	Put(ctx context.Context, d Diagnosis) error
	GetByIncident(ctx context.Context, tenantID, incidentID string) (Diagnosis, error)
}

type memDiagnosisStore struct {
	mu sync.RWMutex
	m  map[string]Diagnosis // tenantID|incidentID -> latest
}

// NewMemDiagnosisStore returns an in-memory DiagnosisStore.
func NewMemDiagnosisStore() DiagnosisStore { return &memDiagnosisStore{m: map[string]Diagnosis{}} }

func diagKey(tenantID, incidentID string) string { return tenantID + "|" + incidentID }

func (s *memDiagnosisStore) Put(_ context.Context, d Diagnosis) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[diagKey(d.TenantID, d.IncidentID)] = d
	return nil
}

func (s *memDiagnosisStore) GetByIncident(_ context.Context, tenantID, incidentID string) (Diagnosis, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.m[diagKey(tenantID, incidentID)]
	if !ok {
		return Diagnosis{}, ErrNotFound
	}
	return d, nil
}

// --- diagnosis service ---------------------------------------------------

type diagnosisService struct {
	det       Diagnoser // deterministic, always present
	llm       Diagnoser // optional external AI; nil when disabled
	store     DiagnosisStore
	incidents IncidentStore
	now       func() time.Time
}

func newDiagnosisService(store DiagnosisStore, incidents IncidentStore, llm Diagnoser) *diagnosisService {
	return &diagnosisService{det: deterministicDiagnoser{}, llm: llm, store: store, incidents: incidents, now: time.Now}
}

// Diagnose runs analysis for an incident and stores the result. It prefers the
// external AI when configured, but validates its output and falls back to the
// deterministic diagnoser on any error or invalid response.
func (s *diagnosisService) Diagnose(ctx context.Context, tenantID, incidentID string) (Diagnosis, error) {
	inc, err := s.incidents.GetIncident(ctx, tenantID, incidentID)
	if err != nil {
		return Diagnosis{}, err
	}
	var dg Diagnosis
	if s.llm != nil {
		start := s.now()
		if out, lerr := s.llm.Diagnose(ctx, inc); lerr == nil {
			if verr := validateDiagnosis(&out, inc); verr == nil {
				out.LatencyMS = s.now().Sub(start).Milliseconds()
				dg = out
			}
		}
	}
	if dg.Source == "" { // no usable AI result → deterministic fallback
		dg, _ = s.det.Diagnose(ctx, inc)
	}
	dg.ID = newID("dg")
	dg.IncidentID = incidentID
	dg.TenantID = tenantID
	dg.CreatedAt = s.now()
	if err := s.store.Put(ctx, dg); err != nil {
		return Diagnosis{}, err
	}
	return dg, nil
}

func (s *diagnosisService) Get(ctx context.Context, tenantID, incidentID string) (Diagnosis, error) {
	return s.store.GetByIncident(ctx, tenantID, incidentID)
}

// validateDiagnosis enforces the safety contract on any (possibly AI-produced)
// diagnosis: required fields present, and — critically — a recommended
// EXECUTABLE action must be a known action type targeting THIS incident's
// workload. Anything else is downgraded to a non-actionable "investigate", so
// the AI can never smuggle an arbitrary action/target into the UI.
func validateDiagnosis(d *Diagnosis, inc Incident) error {
	if strings.TrimSpace(d.Summary) == "" || len(d.Hypotheses) == 0 {
		return fmt.Errorf("diagnosis missing summary or hypotheses")
	}
	for i := range d.Hypotheses {
		if strings.TrimSpace(d.Hypotheses[i].Cause) == "" {
			return fmt.Errorf("hypothesis %d has no cause", i)
		}
		d.Hypotheses[i].Confidence = normalizeConfidence(d.Hypotheses[i].Confidence)
	}
	// Rank hypotheses high→low so the UI shows the most likely first.
	sort.SliceStable(d.Hypotheses, func(i, j int) bool {
		return confRank(d.Hypotheses[i].Confidence) > confRank(d.Hypotheses[j].Confidence)
	})
	d.Confidence = normalizeConfidence(d.Confidence)

	if d.RecommendedActionable {
		if _, err := lookupAction(ActionType(d.RecommendedAction)); err != nil {
			// Unknown action from the model → make it advisory, drop the target.
			d.RecommendedActionable, d.RecommendedAction, d.RecommendedTarget = false, "investigate", nil
		} else {
			kind, name := splitWorkload(inc.Workload)
			// Force the target to the incident's own workload — never trust a
			// model-supplied target.
			d.RecommendedTarget = &TargetRef{Kind: kind, Namespace: inc.Namespace, Name: name}
		}
	} else {
		d.RecommendedTarget = nil
	}
	return nil
}

func normalizeConfidence(c Confidence) Confidence {
	switch Confidence(strings.ToLower(string(c))) {
	case ConfidenceHigh:
		return ConfidenceHigh
	case ConfidenceLow:
		return ConfidenceLow
	default:
		return ConfidenceMedium
	}
}

func confRank(c Confidence) int {
	switch c {
	case ConfidenceHigh:
		return 3
	case ConfidenceMedium:
		return 2
	default:
		return 1
	}
}
