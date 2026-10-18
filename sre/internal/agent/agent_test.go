package agent

import (
	"strings"
	"testing"
	"time"
)

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"authorization: Bearer abc123.def-456 failed":  "[REDACTED]",
		"token=supersecretvalue in config":             "[REDACTED]",
		"connect postgres://user:p4ssw0rd@db:5432/app": "[REDACTED]",
		"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.payload": "[REDACTED]",
		"key AKIAIOSFODNN7EXAMPLE leaked":              "[REDACTED]",
		"paged alice@example.com about the outage":     "[REDACTED]",
	}
	for in, mustContain := range cases {
		got := Redact(in)
		if !strings.Contains(got, mustContain) {
			t.Errorf("Redact(%q) = %q; expected to contain %q", in, got, mustContain)
		}
		if got == in {
			t.Errorf("Redact(%q) did not redact anything", in)
		}
	}
	// benign text must be left alone
	if got := Redact("pod nginx-abc restarted 5 times (CrashLoopBackOff)"); strings.Contains(got, "[REDACTED]") {
		t.Errorf("Redact over-redacted benign text: %q", got)
	}
}

func TestBackoffMonotonicAndCapped(t *testing.T) {
	const cap = 60 * time.Second
	var prevMax time.Duration
	for n := 1; n <= 10; n++ {
		d := backoff(n)
		// jitter is +/-20%, so the hard ceiling is cap*1.2
		if d > time.Duration(float64(cap)*1.2)+time.Millisecond {
			t.Fatalf("backoff(%d)=%s exceeded cap", n, d)
		}
		if d <= 0 {
			t.Fatalf("backoff(%d) non-positive: %s", n, d)
		}
		// the center value should grow until it saturates
		center := centerBackoff(n)
		if center < prevMax && center != cap {
			t.Fatalf("backoff center decreased at n=%d", n)
		}
		prevMax = center
	}
}

// centerBackoff mirrors backoff() without jitter for the monotonicity check.
func centerBackoff(failures int) time.Duration {
	const base, cap = 2 * time.Second, 60 * time.Second
	d := base << min(failures-1, 5)
	if d > cap {
		d = cap
	}
	return d
}

func TestJitterWithinBounds(t *testing.T) {
	base := 10 * time.Second
	for i := 0; i < 1000; i++ {
		j := jitter(base)
		if j < 8*time.Second || j > 12*time.Second {
			t.Fatalf("jitter(%s)=%s outside +/-20%%", base, j)
		}
	}
}
