package conformance_test

import (
	"testing"
	"time"

	"github.com/everdict/herd/conformance"
)

func (g *goldenWork) setRefuse(refuse bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refuse = refuse
}

// rebuiltWithin runs Reconcile for at most d and says whether the template's golden tag stopped being old.
func rebuiltWithin(t *testing.T, e *conformance.Env, tpl, old string, d time.Duration) bool {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if _, err := e.Server.Reconcile(ctxT(t)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if name, _ := goldenTagOf(t, e, tpl); name != old {
			return true
		}
	}
	return false
}

// TestRebuildWaitingOutABackoffHoldsNoSlot is the counterexample of the upgrade that took hours (PLT-591): with one
// rebuild allowed at a time, a template whose rebuild failed and waits out its backoff must not keep the next
// template from being rebuilt. On the cluster a template whose image no node could pull held a slot for the 31
// minutes of its five retries, while every template behind it kept serving a golden of the old guest.
func TestRebuildWaitingOutABackoffHoldsNoSlot(t *testing.T) {
	conformance.GoldenRetryBackoff = time.Hour
	conformance.GoldenRebuilds = 1
	t.Cleanup(func() { conformance.GoldenRetryBackoff, conformance.GoldenRebuilds = 0, 0 })
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 8, Guest: guestOld})
	for _, n := range []string{"t1", "t2"} {
		goldenTemplate(t, e, n, nil)
	}
	old := map[string]string{}
	for _, n := range []string{"t1", "t2"} {
		waitGolden(t, e, n)
		old[n], _ = goldenTagOf(t, e, n)
	}

	g := watchGolden(upgrade(t, e, "h1", guestNew), true)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := e.Server.Reconcile(ctxT(t)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if n, _, live := g.counts(); n == 1 && live == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test setup: the first rebuild was not refused within 5 s")
		}
	}
	g.setRefuse(false)
	rebuilt := ""
	for _, n := range []string{"t1", "t2"} {
		if rebuiltWithin(t, e, n, old[n], 1500*time.Millisecond) {
			rebuilt += n
		}
	}
	if rebuilt == "" {
		t.Fatalf("with one rebuild waiting out an hour's backoff, no other template was rebuilt in 3 s: the wait holds the slot")
	}
	if n, maxLive, _ := g.counts(); maxLive != 1 {
		t.Errorf("%d golden snapshots with at most %d golden actors at once, want one at a time", n, maxLive)
	}
}

// TestRebuildsTheTemplateInUseFirst (PLT-591): after an upgrade the templates actors are made from are rebuilt before
// the ones nothing uses. By name alone, the template every new actor of the cluster used was 37th of 41, and each of
// its actors kept starting from the old guest for hours.
func TestRebuildsTheTemplateInUseFirst(t *testing.T) {
	conformance.GoldenWarmup = 100 * time.Millisecond
	conformance.GoldenRebuilds = 1
	t.Cleanup(func() { conformance.GoldenWarmup, conformance.GoldenRebuilds = 0, 0 })
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 8, Guest: guestOld})
	tpls := []string{"t1", "t2", "t3"}
	old := map[string]string{}
	for _, n := range tpls {
		goldenTemplate(t, e, n, nil)
	}
	for _, n := range tpls {
		waitGolden(t, e, n)
		old[n], _ = goldenTagOf(t, e, n)
	}

	watchGolden(upgrade(t, e, "h1", guestNew), false)
	mkActor(t, e, "a1", "t3", nil)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := e.Server.Reconcile(ctxT(t)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		var done []string
		for _, n := range tpls {
			if name, _ := goldenTagOf(t, e, n); name != old[n] {
				done = append(done, n)
			}
		}
		if len(done) > 0 {
			if len(done) != 1 || done[0] != "t3" {
				t.Fatalf("the first golden rebuilt was of %v, want t3, the template an actor was made from", done)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no golden was rebuilt within 10 s")
		}
	}
}
