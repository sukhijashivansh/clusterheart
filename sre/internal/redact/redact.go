// Package redact strips common secret and PII shapes from free text. It is used
// in two places (defense-in-depth): the agent redacts before telemetry leaves
// the cluster, and the control plane redacts again at ingestion so a
// non-compliant or compromised agent cannot inject raw secrets into the incident
// store or a downstream AI prompt.
//
// The same detectors power secret-leak scanning: Find reports which kinds of
// secret a string contains, without returning the secret itself.
package redact

import "regexp"

// Detector is one named secret (or PII) shape.
type Detector struct {
	Name string // stable id, e.g. "aws-access-key"
	// Secret is false for PII (emails): scrubbed, but not reported as a leak.
	Secret bool
	re     *regexp.Regexp
}

// Detectors run in order; specific credential formats come before the generic
// key=value rule so a finding is named by what it really is.
var detectors = []Detector{
	{"private-key", true, regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"aws-access-key", true, regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"github-token", true, regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b|\bgithub_pat_[A-Za-z0-9_]{40,}`)},
	{"slack-token", true, regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"google-api-key", true, regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
	{"stripe-key", true, regexp.MustCompile(`\b(sk|rk)_live_[0-9A-Za-z]{16,}`)},
	{"jwt", true, regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}|eyJ[A-Za-z0-9._\-]{10,}`)},
	{"bearer-token", true, regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`)},
	{"url-credentials", true, regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^:/\s]+:[^@/\s]+@`)},
	{"password-or-token", true, regexp.MustCompile(`(?i)(password|passwd|pwd|token|secret|api[_-]?key|access[_-]?key)["']?\s*[:=]\s*["']?[^\s"';,&\[]{4,}`)},
	{"email", false, regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)},
}

const placeholder = "[REDACTED]"

// String replaces secret/PII-shaped substrings with a placeholder.
func String(s string) string {
	for _, d := range detectors {
		s = d.re.ReplaceAllString(s, placeholder)
	}
	return s
}

// Find returns the names of the secret detectors that match s, in detector
// order, each at most once. PII-only detectors (email) are not reported. A
// later detector only counts if it still matches after the earlier ones were
// scrubbed, so one secret is reported under one name.
func Find(s string) []string {
	var out []string
	for _, d := range detectors {
		if !d.re.MatchString(s) {
			continue
		}
		if d.Secret {
			out = append(out, d.Name)
		}
		s = d.re.ReplaceAllString(s, placeholder)
	}
	return out
}
