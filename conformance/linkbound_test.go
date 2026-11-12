package conformance_test

import (
	"testing"
	"time"

	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/conformance"
)

// TestLinkEndsWithItsCredential (PLT-489 review 2): a herdlet link opened with an HS256 credential closes at the
// break-glass allowance's end, and the herdlet cannot link again after it: a stream lives no longer than the
// credential that opened it.
func TestLinkEndsWithItsCredential(t *testing.T) {
	until := time.Now().Add(2 * time.Second)
	prev := conformance.Authn
	conformance.Authn = func(key []byte, m *auth.Meter) auth.Authenticator {
		return auth.Chain{BreakGlass: auth.StaticKey{Key: key}, BreakGlassUntil: until, Meter: m}
	}
	t.Cleanup(func() { conformance.Authn = prev })
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 1})
	connected := func() bool {
		for _, w := range e.Hub.Workers() {
			if w.Worker == "h1" && w.Connected {
				return true
			}
		}
		return false
	}
	if !connected() {
		t.Fatal("h1 is not linked before the allowance ends")
	}
	deadline := time.Now().Add(5 * time.Second)
	for connected() {
		if time.Now().After(deadline) {
			t.Fatalf("h1's HS256 link is still up %s after the break-glass allowance ended", time.Since(until).Round(time.Second))
		}
		time.Sleep(50 * time.Millisecond)
	}
}
