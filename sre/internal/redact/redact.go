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
	"strings"
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

// hints lists, per detector, substrings at least one of which every match
// contains, so text without any of them skips that regex (most log lines skip
// all of them). Case-insensitive detectors are checked against lowercased
// text; text with non-ASCII bytes runs every regex, because Unicode case
// folding (e.g. the Kelvin sign) can match where lowercasing does not.
var hints = map[string]struct {
	any  []string
	fold bool
}{
	"private-key":       {any: []string{"PRIVATE KEY"}},
	"aws-access-key":    {any: []string{"AKIA", "ASIA"}},
	"github-token":      {any: []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"}},
	"slack-token":       {any: []string{"xox"}},
	"google-api-key":    {any: []string{"AIza"}},
	"stripe-key":        {any: []string{"_live_"}},
	"jwt":               {any: []string{"eyJ"}},
	"bearer-token":      {any: []string{"bearer"}, fold: true},
	"url-credentials":   {any: []string{"://"}},
	"password-or-token": {any: []string{"pass", "pwd", "token", "secret", "key"}, fold: true},
	"email":             {any: []string{"@"}},
}

// prefilter decides, from the original text, which detectors can match.
// Scrubbing only replaces matched text with the placeholder, which contains
// no hint and cannot join neighbours into one, so the answer stays valid
// after earlier detectors scrubbed s.
type prefilter struct {
	s, lower string
	ascii    bool
}

func newPrefilter(s string) prefilter {
	p := prefilter{s: s, ascii: true}
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			p.ascii = false
			break
		}
	}
	return p
}

func (p *prefilter) may(d Detector) bool {
	h, ok := hints[d.Name]
	if !ok {
		return true
	}
	text := p.s
	if h.fold {
		if !p.ascii {
			return true
		}
		if p.lower == "" {
			p.lower = strings.ToLower(p.s)
		}
		text = p.lower
	}
	for _, k := range h.any {
		if strings.Contains(text, k) {
			return true
		}
	}
	return false
}

func (d Detector) scrub(s string) string {
	if d.repl != "" {
		return d.re.ReplaceAllString(s, d.repl)
	}
	return d.re.ReplaceAllLiteralString(s, placeholder)
}

// String replaces secret/PII-shaped substrings with a placeholder.
func String(s string) string {
	pf := newPrefilter(s)
	for _, d := range detectors {
		if pf.may(d) {
			s = d.scrub(s)
		}
	}
	return s
}

// Match describes one secret found in a string, without the secret itself.
type Match struct {
	Type   string // detector name
	Key    string // the field it was under, e.g. "accessToken" or "DB_PASSWORD"; empty if none
	Masked string // a hint to recognise it, e.g. "eyJh…x9Qz"; never more than 8 characters of it
	Length int    // length of the secret value
}

// Matches returns, for each secret detector that fires on s, its first
// match's key name and a masked hint of the value. PII-only detectors (email)
// are not reported, and a later detector only counts if it still matches after
// the earlier ones were scrubbed, so one secret is reported under one name.
func Matches(s string) []Match {
	var out []Match
	pf := newPrefilter(s)
	for _, d := range detectors {
		if !pf.may(d) {
			continue
		}
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
	m.Masked, m.Length = Mask(value), utf8.RuneCountInString(value)
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
