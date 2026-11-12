package auth_test

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/auth"
)

// TestBreakGlassLogIsRateLimited (PLT-489, A-60): a polling caller does not flood the log. The counter counts every
// HS256 call, while the log names a subject's first call at once and then sums its calls in one line per subject per
// minute. The same limit holds for the unowned-actor-write lines (Meter.Note).
func TestBreakGlassLogIsRateLimited(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := auth.NewMeter()
	m.Now = func() time.Time { return now }
	chain := auth.Chain{BreakGlass: auth.StaticKey{Key: key, Now: func() time.Time { return now }}, Meter: m}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	tok := func(sub string) string {
		s, err := auth.Sign(key, auth.Claims{Subject: sub, ExpiresAt: now.Add(time.Hour).Unix()})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	herder, other := tok("everdict"), tok("ops")
	call := func(tok string, n int) {
		for i := 0; i < n; i++ {
			if _, err := chain.Authenticate(context.Background(), tok); err != nil {
				t.Fatal(err)
			}
		}
	}
	lines := func(sub string) []string {
		var out []string
		for _, l := range strings.Split(logs.String(), "\n") {
			if strings.Contains(l, `subject "`+sub+`"`) {
				out = append(out, l)
			}
		}
		return out
	}

	call(herder, 100)
	call(other, 1)
	if n := m.Value(auth.MetricBreakGlassCalls, "subject", "everdict"); n != 100 {
		t.Errorf("counter = %d after 100 calls, want 100: the count is exact whatever the log does", n)
	}
	if l := lines("everdict"); len(l) != 1 || !strings.Contains(l[0], "break-glass HS256 call by subject") {
		t.Fatalf("after 100 calls in one minute the log has %d lines for the subject, want its first call only: %q", len(l), l)
	}
	if l := lines("ops"); len(l) != 1 {
		t.Errorf("another subject's first call: %d lines, want 1 (the limit is per subject)", len(l))
	}

	now = now.Add(30 * time.Second)
	call(herder, 10)
	if l := lines("everdict"); len(l) != 1 {
		t.Errorf("30 s later the log has %d lines for the subject, want still 1", len(l))
	}
	now = now.Add(31 * time.Second)
	call(herder, 1)
	l := lines("everdict")
	if len(l) != 2 || !strings.Contains(l[1], `110 break-glass HS256 calls by subject "everdict" in the last 61s`) {
		t.Fatalf("a minute on, the log for the subject is %q; want one summary of the 110 calls since its first", l)
	}
	if n := m.Value(auth.MetricBreakGlassCalls, "subject", "everdict"); n != 111 {
		t.Errorf("counter = %d, want 111", n)
	}

	// The unowned-write lines take the same limit, keyed by what the caller passes.
	logs.Reset()
	for i := 0; i < 50; i++ {
		m.Note("unowned/hs256:x", "herd-api: unowned write by hs256:x", func(n int, d time.Duration) string { return "unowned writes by hs256:x" })
	}
	if c := strings.Count(logs.String(), "unowned write"); c != 1 {
		t.Errorf("50 unowned-write notes in one minute logged %d lines, want 1", c)
	}
}
