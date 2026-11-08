package redact

import (
	"math/rand"
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
		var got []string
		for _, m := range Matches(in) {
			got = append(got, m.Type)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Matches(%q) types = %v, want %v", in, got, want)
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

func TestMatchesDescribeWithoutLeaking(t *testing.T) {
	line := `{"level":"info","accessToken":"eyJhbGciOiJIUzI1NiJ9xyzxyzxyzQz9","msg":"ok"}`
	m := Matches(line)
	if len(m) != 1 {
		t.Fatalf("matches = %+v", m)
	}
	if m[0].Key != "accessToken" || m[0].Length != 32 {
		t.Errorf("key/length = %q/%d", m[0].Key, m[0].Length)
	}
	if m[0].Masked != "eyJh…zQz9" {
		t.Errorf("masked = %q", m[0].Masked)
	}
	if got := Matches("DB_PASSWORD=hunter22"); len(got) != 1 || got[0].Key != "DB_PASSWORD" || got[0].Masked != "••••" {
		t.Errorf("short password: %+v", got)
	}
	if got := Matches("postgres://app:s3cretpassword1@db/app"); len(got) != 1 || got[0].Key != "password for user app" || strings.Contains(got[0].Masked, "cretpass") {
		t.Errorf("url creds: %+v", got)
	}
	if got := Matches("key AKIAIOSFODNN7EXAMPLE"); len(got) != 1 || got[0].Masked != "AKIA…MPLE" || got[0].Key != "" {
		t.Errorf("aws: %+v", got)
	}
	if got := Matches("AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE"); len(got) != 1 || got[0].Key != "AWS_ACCESS_KEY_ID" {
		t.Errorf("aws: %+v", got)
	}
}

func TestKeyValueScrubKeepsTheKey(t *testing.T) {
	got := String(`{"msg":"fetched","secret":"s3cr3t","password=hunter22"}`)
	if !strings.Contains(got, `"secret":"[REDACTED]"`) || !strings.Contains(got, `password=[REDACTED]`) || strings.Contains(got, "s3cr3t") {
		t.Fatalf("scrubbed = %q", got)
	}
}

// slowString and slowMatches are String and Matches without the prefilter.
func slowString(s string) string {
	for _, d := range detectors {
		s = d.scrub(s)
	}
	return s
}

func slowMatches(s string) []Match {
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

// TestPrefilterChangesNothing checks the keyword prefilter is only a speed-up:
// random mixes of secret-shaped and ordinary fragments give the same result
// with and without it.
func TestPrefilterChangesNothing(t *testing.T) {
	frags := []string{
		"GET /api/v1/status 200 3.2ms", "level=info msg=ok", " ", ", ", "\"", "'", "=", ": ",
		"-----BEGIN RSA PRIVATE KEY-----", "AKIAIOSFODNN7EXAMPLE", "ASIAABCDEFGHIJKLMNOP",
		"ghp_" + strings.Repeat("a1B2", 9), "github_pat_" + strings.Repeat("x9_", 14), "xoxb-123456789012-abcdef",
		"AIza" + strings.Repeat("Sy_-", 9)[:35], "sk_live_" + strings.Repeat("Z9", 9),
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"Authorization: Bearer abc.def-123", "BEARER   tok123", "postgres://admin:s3cr3tpw@db:5432/app",
		"password=hunter22", "DB_PASSWORD: \"p@ss w0rd\"", "apiKey=abcd1234", "access-key: AKX1234567",
		"pwd='zzzzzz'", "TOKEN=t0k3n!!", "client_secret=shh12345", "user@example.com", "ops@clusterheart.io",
		"Key=abcdefgh", "paſſword=topsecret1", "Ünïcode tëxt token=äöü12345", "naïve",
		"[REDACTED]", "key", "pass", "secret", "://", "@", "eyJ", "xox", "_live_",
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for j := rng.Intn(6) + 1; j > 0; j-- {
			b.WriteString(frags[rng.Intn(len(frags))])
		}
		s := b.String()
		if got, want := String(s), slowString(s); got != want {
			t.Fatalf("String(%q) = %q, want %q", s, got, want)
		}
		if got, want := Matches(s), slowMatches(s); !reflect.DeepEqual(got, want) {
			t.Fatalf("Matches(%q) = %+v, want %+v", s, got, want)
		}
	}
}

func BenchmarkMatchesPlainLog(b *testing.B) {
	line := `{"time":"2026-10-31T20:00:00Z","level":"INFO","msg":"telemetry accepted","clusterId":"clu_9","snapshots":1,"dur":"3.2ms"}`
	for i := 0; i < b.N; i++ {
		Matches(line)
	}
}
