package agent

import "regexp"

// Redaction patterns. The goal is defense-in-depth: strip common secret and PII
// shapes from event/log text before it leaves the cluster, since it is later
// forwarded to the control plane and may be shown to an external model. This is
// not a substitute for not collecting secrets in the first place.
var redactors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`),                               // bearer tokens
	regexp.MustCompile(`eyJ[A-Za-z0-9._\-]{10,}`),                                     // JWTs
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                                            // AWS access key id
	regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^:/\s]+:[^@/\s]+@`),               // creds in URLs
	regexp.MustCompile(`(?i)(password|passwd|pwd|token|secret|api[_-]?key)["']?\s*[:=]\s*["']?[^\s"';,&]{4,}`), // key=value secrets
	regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),            // emails (PII)
}

// Redact replaces secret/PII-shaped substrings with a placeholder.
func Redact(s string) string {
	for _, re := range redactors {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}
