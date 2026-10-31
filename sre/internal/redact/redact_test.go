package redact

import (
	"reflect"
	"strings"
	"testing"
)

func TestFindNamesEachSecretOnce(t *testing.T) {
	cases := map[string][]string{
		"aws key AKIAIOSFODNN7EXAMPLE used":                                   {"aws-access-key"},
		"-----BEGIN RSA PRIVATE KEY----- MIIE":                                {"private-key"},
		"token ghp_" + strings.Repeat("a", 36):                                {"github-token"},
		"slack xoxb-1234567890-abcdefghij":                                    {"slack-token"},
		"key AIza" + strings.Repeat("B", 35):                                  {"google-api-key"},
		"stripe sk_live_" + strings.Repeat("c", 24):                           {"stripe-key"},
		"Authorization: Bearer abc.def.ghi":                                   {"bearer-token"},
		"postgres://app:hunter22@db:5432/app":                                 {"url-credentials"},
		"login ok password=hunter22":                                          {"password-or-token"},
		"user alice@example.com signed in":                                    nil, // PII, not a leak
		"GET /healthz 200 in 3ms":                                             nil,
		"DB_PASSWORD=hunter22 and AWS AKIAIOSFODNN7EXAMPLE":                   {"aws-access-key", "password-or-token"},
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.dozjgNryP4J3jVmNHl0w5N_XgL": {"jwt"},
	}
	for in, want := range cases {
		if got := Find(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Find(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestStringScrubsAllDetectors(t *testing.T) {
	in := "AKIAIOSFODNN7EXAMPLE password=hunter22 alice@example.com postgres://u:p4ss@h/db"
	out := String(in)
	for _, leak := range []string{"AKIAIOSFODNN7EXAMPLE", "hunter22", "alice@example.com", "p4ss"} {
		if strings.Contains(out, leak) {
			t.Errorf("String left %q in %q", leak, out)
		}
	}
	// Already-redacted text stays stable (the control plane re-redacts).
	if again := String(out); again != out {
		t.Errorf("not idempotent: %q -> %q", out, again)
	}
}
