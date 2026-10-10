// Package redact strips common secret and PII shapes from free text. It is used
// in two places (defense-in-depth): the agent redacts before telemetry leaves
// the cluster, and the control plane redacts again at ingestion so a
// non-compliant or compromised agent cannot inject raw secrets into the incident
// store or a downstream AI prompt.
package redact

import "regexp"

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`),                                                        // bearer tokens
	regexp.MustCompile(`eyJ[A-Za-z0-9._\-]{10,}`),                                                              // JWTs
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                                                                     // AWS access key id
	regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^:/\s]+:[^@/\s]+@`),                                        // creds in URLs
	regexp.MustCompile(`(?i)(password|passwd|pwd|token|secret|api[_-]?key)["']?\s*[:=]\s*["']?[^\s"';,&]{4,}`), // key=value secrets
	regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),                                     // emails (PII)
}

// String replaces secret/PII-shaped substrings with a placeholder.
func String(s string) string {
	for _, re := range patterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}
