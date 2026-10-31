package hostagent

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"clusterheart/sre/internal/contract"
	"clusterheart/sre/internal/redact"
)

const (
	secretFindingTTL  = 24 * time.Hour
	maxSecretFindings = 100
	maxContextLen     = 400
	maxRevealLen      = 8192
)

// sensitiveEnvName matches env var names that should not hold a literal value.
var sensitiveEnvName = regexp.MustCompile(`(?i)(pass(word|wd)?|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential|auth|dsn|conn(ection)?[_-]?str)`)

// secretScanner finds credentials in the journal and in services' literal
// Environment= settings. Raw values stay in memory for explicit reveals only.
type secretScanner struct {
	mu     sync.Mutex
	host   string
	logs   map[string]*contract.SecretFinding
	specs  map[string]*contract.SecretFinding
	values map[string]string
}

func newSecretScanner() *secretScanner {
	return &secretScanner{logs: map[string]*contract.SecretFinding{}, specs: map[string]*contract.SecretFinding{}, values: map[string]string{}}
}

func clip(s string) string {
	if len(s) <= maxContextLen {
		return s
	}
	start := 0
	if i := strings.Index(s, "[REDACTED]"); i > maxContextLen/3 {
		start = i - maxContextLen/3
	}
	end := min(start+maxContextLen, len(s))
	for start > 0 && !utf8.RuneStart(s[start]) {
		start--
	}
	for end < len(s) && !utf8.RuneStart(s[end]) {
		end++
	}
	out := s[start:end]
	if start > 0 {
		out = "…" + out
	}
	if end < len(s) {
		out += "…"
	}
	return out
}

func (s *secretScanner) owner(unit string) string {
	if unit == "" {
		return "Host/" + s.host
	}
	return "Service/" + strings.TrimSuffix(unit, ".service")
}

// recordJournal scans new journal entries for credentials.
func (s *secretScanner) recordJournal(entries []journalEntry, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		for _, m := range redact.Matches(e.message) {
			f := contract.SecretFinding{Source: "log", Type: m.Type, Namespace: s.host, Owner: s.owner(e.unit), Container: e.unit, Key: m.Key}
			ref := f.Ref()
			cur := s.logs[ref]
			if cur == nil {
				f.FirstSeen = now
				cur = &f
				s.logs[ref] = cur
			}
			cur.Count++
			cur.LastSeen = now
			cur.Masked, cur.Length = m.Masked, m.Length
			cur.Context = clip(redact.String(strings.TrimSpace(e.message)))
			if m.Value != "" {
				s.values[ref] = m.Value
			}
		}
	}
}

// scanUnitEnv recomputes findings for literal secrets in services' settings.
func (s *secretScanner) scanUnitEnv(env map[string][]string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.specs
	s.specs = map[string]*contract.SecretFinding{}
	for unit, kvs := range env {
		for _, kv := range kvs {
			name, val, ok := strings.Cut(kv, "=")
			if !ok || strings.TrimSpace(val) == "" {
				continue
			}
			var m redact.Match
			if ms := redact.Matches(val); len(ms) > 0 {
				m = ms[0]
			} else if sensitiveEnvName.MatchString(name) && len(val) >= 6 {
				m = redact.Match{Type: "hardcoded-env", Masked: redact.Mask(val), Length: utf8.RuneCountInString(val), Value: val}
			} else {
				continue
			}
			f := contract.SecretFinding{Source: "env", Type: m.Type, Namespace: s.host, Owner: s.owner(unit), Container: unit,
				Field: name, Key: name, Masked: m.Masked, Length: m.Length, Context: name + "=[REDACTED]", Count: 1, FirstSeen: now, LastSeen: now}
			ref := f.Ref()
			if old, ok := prev[ref]; ok {
				f.FirstSeen = old.FirstSeen
			}
			s.specs[ref] = &f
			s.values[ref] = m.Value
		}
	}
}

// findings returns current findings, newest first, and forgets values of
// findings that are gone.
func (s *secretScanner) findings(now time.Time) []contract.SecretFinding {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, f := range s.logs {
		if now.Sub(f.LastSeen) > secretFindingTTL {
			delete(s.logs, k)
		}
	}
	out := make([]contract.SecretFinding, 0, len(s.logs)+len(s.specs))
	for _, f := range s.logs {
		out = append(out, *f)
	}
	for _, f := range s.specs {
		out = append(out, *f)
	}
	current := map[string]bool{}
	for _, f := range out {
		current[f.Ref()] = true
	}
	for ref := range s.values {
		if !current[ref] {
			delete(s.values, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	if len(out) > maxSecretFindings {
		out = out[:maxSecretFindings]
	}
	return out
}

// Value returns the raw secret behind a current finding, for a reveal.
func (s *secretScanner) Value(ref string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[ref]
	if len(v) > maxRevealLen {
		v = v[:maxRevealLen]
	}
	return v, ok
}
