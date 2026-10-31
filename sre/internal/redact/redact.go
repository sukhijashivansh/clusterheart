// Package redact strips common secret and PII shapes from free text. It is used
// in two places (defense-in-depth): the agent redacts before telemetry leaves
// the cluster, and the control plane redacts again at ingestion so a
// non-compliant or compromised agent cannot inject raw secrets into the incident
// store or a downstream AI prompt.
//
// The same detectors power secret-leak scanning: Find reports which kinds of
// secret a string contains, without returning the secret itself.
package redact

import (
	"regexp"
	"unicode/utf8"
)

// Detector is one named secret (or PII) shape.
type Detector struct {
	Name string // stable id, e.g. "aws-access-key"
	// Secret is false for PII (emails): scrubbed, but not reported as a leak.
	Secret bool
	re     *regexp.Regexp
	// repl is the replacement; empty means the whole match becomes the
	// placeholder. key=value rules keep the key so a scrubbed line still says
	// what was there.
	repl string
}

// Detectors run in order; specific credential formats come before the generic
// key=value rule so a finding is named by what it really is.
var detectors = []Detector{
	{"private-key", true, regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), ""},
	{"aws-access-key", true, regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), ""},
	{"github-token", true, regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b|\bgithub_pat_[A-Za-z0-9_]{40,}`), ""},
	{"slack-token", true, regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), ""},
	{"google-api-key", true, regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`), ""},
	{"stripe-key", true, regexp.MustCompile(`\b(sk|rk)_live_[0-9A-Za-z]{16,}`), ""},
	{"jwt", true, regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}|eyJ[A-Za-z0-9._\-]{10,}`), ""},
	{"bearer-token", true, regexp.MustCompile(`(?i)bearer\s+([A-Za-z0-9._\-]+)`), ""},
	{"url-credentials", true, regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://([^:/\s]+):([^@/\s]+)@`), ""},
	{"password-or-token", true, regexp.MustCompile(`(?i)(password|passwd|pwd|token|secret|api[_-]?key|access[_-]?key)(["']?\s*[:=]\s*["']?)([^\s"';,&\[]{4,})`), "${1}${2}" + placeholder},
	{"email", false, regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`), ""},
}

const placeholder = "[REDACTED]"

func (d Detector) scrub(s string) string {
	if d.repl != "" {
		return d.re.ReplaceAllString(s, d.repl)
	}
	return d.re.ReplaceAllLiteralString(s, placeholder)
}

// String replaces secret/PII-shaped substrings with a placeholder.
func String(s string) string {
	for _, d := range detectors {
		s = d.scrub(s)
	}
	return s
}

// Match describes one secret found in a string, without the secret itself.
type Match struct {
	Type   string // detector name
	Key    string // the field it was under, e.g. "accessToken" or "DB_PASSWORD"; empty if none
	Masked string // a hint to recognise it, e.g. "eyJh…x9Qz"; never more than 8 characters of it
	Length int    // length of the secret value
	// Value is the raw secret. The agent keeps it in memory, inside the cluster,
	// and only hands it over for an explicit, audited reveal. Never put it in a
	// finding, a log line or telemetry.
	Value string
}

// Find returns the names of the secret detectors that match s, in detector
// order, each at most once. PII-only detectors (email) are not reported. A
// later detector only counts if it still matches after the earlier ones were
// scrubbed, so one secret is reported under one name.
func Find(s string) []string {
	var out []string
	for _, m := range Matches(s) {
		out = append(out, m.Type)
	}
	return out
}

// Matches is Find with details: for each detector, its first match's key name
// and a masked hint of the value.
func Matches(s string) []Match {
	var out []Match
	for _, d := range detectors {
		loc := d.re.FindStringSubmatchIndex(s)
		if loc == nil {
			continue
		}
		if d.Secret {
			out = append(out, describe(d.Name, s, loc))
		}
		s = d.scrub(s)
	}
	return out
}

func describe(name, s string, loc []int) Match {
	group := func(i int) string {
		if 2*i+1 < len(loc) && loc[2*i] >= 0 {
			return s[loc[2*i]:loc[2*i+1]]
		}
		return ""
	}
	m := Match{Type: name}
	value := group(0)
	switch name {
	case "password-or-token":
		// Widen the matched keyword to the whole field name (accessToken, DB_PASSWORD).
		start := loc[2]
		for start > 0 && isKeyByte(s[start-1]) {
			start--
		}
		end := loc[3]
		for end < len(s) && isKeyByte(s[end]) {
			end++
		}
		m.Key, value = s[start:end], group(3)
	case "bearer-token":
		m.Key, value = "Authorization: Bearer", group(1)
	case "url-credentials":
		m.Key, value = "password for user "+group(1), group(2)
	}
	if m.Key == "" {
		m.Key = keyBefore(s[:loc[0]])
	}
	m.Masked, m.Length, m.Value = Mask(value), utf8.RuneCountInString(value), value
	return m
}

// keyBeforeRe matches a field name right before a value: `"accessToken": "`,
// `AWS_KEY=`, `token: `.
var keyBeforeRe = regexp.MustCompile(`([A-Za-z0-9_.\-]+)["']?\s*[:=]\s*["']?$`)

// keyBefore returns the field name a value was assigned to, if any.
func keyBefore(prefix string) string {
	if len(prefix) > 80 {
		prefix = prefix[len(prefix)-80:]
	}
	if m := keyBeforeRe.FindStringSubmatch(prefix); m != nil {
		return m[1]
	}
	return ""
}

func isKeyByte(b byte) bool {
	return b == '_' || b == '-' || b == '.' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// Mask keeps just enough of a secret to recognise it: up to 4 leading and 4
// trailing characters for long values, 2+2 for medium ones, nothing for short
// ones (a short password would be mostly revealed).
func Mask(v string) string {
	r := []rune(v)
	switch n := len(r); {
	case n >= 20:
		return string(r[:4]) + "…" + string(r[n-4:])
	case n >= 12:
		return string(r[:2]) + "…" + string(r[n-2:])
	default:
		return "••••"
	}
}
