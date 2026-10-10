package agent

import "clusterheart/sre/internal/redact"

// Redact scrubs secret/PII-shaped substrings from event/log text before it
// leaves the cluster. Delegates to the shared redactor (also applied at the
// control-plane boundary for defense-in-depth).
func Redact(s string) string { return redact.String(s) }
