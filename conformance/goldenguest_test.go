package conformance_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/apiserver"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/link"
	"google.golang.org/grpc/codes"
)

// Guests of a herd release before and after an upgrade (deploy.GuestIdentity of its kernel and initramfs).
const (
	guestOld = "sha256:guest-v0.1.5"
	guestNew = "sha256:guest-v0.1.6"
)

// upgrade restarts the herdlet called name on guest, as a herd upgrade of its node does: the link goes, and the
// herdlet comes back under the same name declaring the new guest.
func upgrade(t *testing.T, e *conformance.Env, name, guest string) *conformance.Herdlet {
	t.Helper()
	h := e.Herdlets[name]
	h.Disconnect()
	for deadline := time.Now().Add(5 * time.Second); hubHas(e, name); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("herdlet %s still connected after its link ended", name)
		}
	}
	s := h.Spec
	s.Guest = guest
	return e.AddHerdlet(t, s)
}

func hubHas(e *conformance.Env, name string) bool {
	for _, w := range e.Hub.Workers() {
		if w.Worker == name {
			return true
		}
	}
	return false
}

// goldenTagOf reads the template's golden tag: its name and the key of its snapshot.
func goldenTagOf(t *testing.T, e *conformance.Env, tpl string) (name, key string) {
	t.Helper()
	got, err := e.Client.GetActorTemplate(ctxT(t), &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, tpl)})
	if err != nil {
		t.Fatalf("GetActorTemplate %s: %v", tpl, err)
	}
	gs := got.GetStatus().GetGoldenSnapshotStatus()
	if gs.GetGoldenTag() == nil || gs.GetErrorMessage() != "" {
		t.Fatalf("template %s: golden_snapshot_status = %v, want a golden tag and no error", tpl, gs)
	}
	tag, err := e.Client.GetTag(ctxT(t), &ateapi.GetTagRequest{Tag: gs.GetGoldenTag()})
	if err != nil {
		t.Fatalf("GetTag %s: %v", gs.GetGoldenTag().GetName(), err)
	}
	return gs.GetGoldenTag().GetName(), tag.GetStatus().GetSnapshot().GetUuid()
}

// waitRebuilt runs Reconcile, as herd-api's loop does, until the template's golden tag is no longer old.
func waitRebuilt(t *testing.T, e *conformance.Env, tpl, old string) (name, key string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if _, err := e.Server.Reconcile(ctxT(t)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if name, key := goldenTagOf(t, e, tpl); name != old {
			return name, key
		}
	}
	t.Fatalf("template %s: golden tag still %s after 10 s of Reconcile on the upgraded guest", tpl, old)
	return "", ""
}

// goldenWork records a herdlet's work on golden actors (an actor herd-api made for a template, named by its uid) and
// on the others, and can refuse its snapshots.
type goldenWork struct {
	mu       sync.Mutex
	work     []link.Work
	refuse   bool
	golden   map[string]bool // golden actors' VMs
	live     map[string]bool // golden actors' VMs booted and not deleted
	maxLive  int
	snapshot int // snapshots of golden actors asked
}

func watchGolden(h *conformance.Herdlet, refuse bool) *goldenWork {
	g := &goldenWork{refuse: refuse, golden: map[string]bool{}, live: map[string]bool{}}
	h.Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.work = append(g.work, w)
		switch {
		case w.Op == link.OpBoot && w.Actor != nil && w.Template != "" && strings.HasPrefix(w.Actor.Name, w.Template):
			g.golden[w.VM], g.live[w.VM] = true, true
			g.maxLive = max(g.maxLive, len(g.live))
		case w.Op == link.OpDelete:
			delete(g.live, w.VM)
		case w.Op == link.OpSnapshot && g.golden[w.VM]:
			g.snapshot++
			if g.refuse {
				return link.Outcome{ID: o.ID, Result: link.ResultFailed, Reason: "refused by the test"}, true
			}
		}
		return o, true
	})
	return g
}

func (g *goldenWork) of(vm string) []link.Work {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []link.Work
	for _, w := range g.work {
		if w.VM == vm {
			out = append(out, w)
		}
	}
	return out
}

func (g *goldenWork) counts() (snapshots, maxLive, live int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.snapshot, g.maxLive, len(g.live)
}

// restoredFrom says what the actor's first herdlet work restored from ("" for a cold boot).
func restoredFrom(t *testing.T, g *goldenWork, a *ateapi.Actor) string {
	t.Helper()
	work := g.of(a.GetMetadata().GetUid())
	if len(work) == 0 {
		t.Fatalf("actor %s: no herdlet work", a.GetMetadata().GetName())
	}
	if work[0].Op != link.OpRestore || work[0].Snapshot == nil {
		return ""
	}
	return work[0].Snapshot.Key
}

// TestUpgradedGuestRebuildsGolden (DEFAUL-1606): a template's golden snapshot built by one guest is rebuilt by
// Reconcile, with no change to the template, once the workers run another guest; actors then start from the new
// golden, and the old golden tag (and its snapshot) goes. Before, the golden of the older guest served for good.
func TestUpgradedGuestRebuildsGolden(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4, Guest: guestOld})
	tpl := goldenTemplate(t, e, "tpl", nil)
	waitGolden(t, e, "tpl")
	old, oldKey := goldenTagOf(t, e, "tpl")

	g := watchGolden(upgrade(t, e, "h1", guestNew), false)
	name, key := waitRebuilt(t, e, "tpl", old)
	if key == oldKey || key == "" {
		t.Errorf("the rebuilt golden tag %s holds snapshot %q, the old one held %q", name, key, oldKey)
	}
	got, err := e.Client.GetActorTemplate(ctxT(t), &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, "tpl")})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetMetadata().GetUid() != tpl.GetMetadata().GetUid() || got.GetContainers()[0].GetImage() != tpl.GetContainers()[0].GetImage() {
		t.Errorf("the template changed: %v, was %v", got, tpl)
	}
	a := mkActor(t, e, "a1", "tpl", nil)
	if from := restoredFrom(t, g, a); from != key {
		t.Errorf("an actor after the rebuild restored from %q, want the new golden %q", from, key)
	}
	// The old golden goes once nothing can still be restoring from it.
	reconcileFor(t, e, time.Second)
	_, err = e.Client.GetTag(ctxT(t), &ateapi.GetTagRequest{Tag: oref(e, old)})
	wantCode(t, "GetTag of the replaced golden tag", err, codes.NotFound)
	if n, _, _ := g.counts(); n != 1 {
		t.Errorf("the upgraded guest took %d golden snapshots, want 1", n)
	}
}

// TestOldGoldenServesUntilRebuildReady (DEFAUL-1606): while the golden of the new guest is being built, the template
// still names the old golden and an actor of it restores from that, with no error; the new one takes over only once
// its tag exists, and the actor on the old one keeps running.
func TestOldGoldenServesUntilRebuildReady(t *testing.T) {
	conformance.GoldenWarmup = 400 * time.Millisecond
	t.Cleanup(func() { conformance.GoldenWarmup = 0 })
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4, Guest: guestOld})
	goldenTemplate(t, e, "tpl", nil)
	waitGolden(t, e, "tpl")
	old, oldKey := goldenTagOf(t, e, "tpl")

	g := watchGolden(upgrade(t, e, "h1", guestNew), false)
	if _, err := e.Server.Reconcile(ctxT(t)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, _, live := g.counts(); live != 1 {
		t.Fatalf("after Reconcile on the upgraded guest: %d golden actors running, want the rebuild's one", live)
	}
	if name, key := goldenTagOf(t, e, "tpl"); name != old || key != oldKey {
		t.Fatalf("while the rebuild warms up the template names %s (%s), want the old golden %s (%s)", name, key, old, oldKey)
	}
	a := mkActor(t, e, "a1", "tpl", nil)
	if from := restoredFrom(t, g, a); from != oldKey {
		t.Errorf("an actor during the rebuild restored from %q, want the old golden %q", from, oldKey)
	}
	_, key := waitRebuilt(t, e, "tpl", old)
	b := mkActor(t, e, "a2", "tpl", nil)
	if from := restoredFrom(t, g, b); from != key {
		t.Errorf("an actor after the rebuild restored from %q, want the new golden %q", from, key)
	}
	if got, owed := getActor(t, e, "a1"); got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING || owed != "" {
		t.Errorf("the actor started on the old golden: %v (owed %q), want RUNNING", got.GetStatus().GetState(), owed)
	}
}

// TestFailedRebuildKeepsOldGolden (DEFAUL-1606): a rebuild that fails leaves the template on its old golden, with no
// error a client would read as a failed template, and is retried with A-35's backoff and bound; then it stops.
func TestFailedRebuildKeepsOldGolden(t *testing.T) {
	conformance.GoldenRetryBackoff = 20 * time.Millisecond
	t.Cleanup(func() { conformance.GoldenRetryBackoff = 0 })
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4, Guest: guestOld})
	goldenTemplate(t, e, "tpl", nil)
	waitGolden(t, e, "tpl")
	old, oldKey := goldenTagOf(t, e, "tpl")

	g := watchGolden(upgrade(t, e, "h1", guestNew), true)
	reconcileFor(t, e, 3*time.Second)
	n, _, live := g.counts()
	if want := 1 + apiserver.DefaultGoldenRetries; n != want {
		t.Errorf("the failing rebuild asked for %d snapshots, want %d (one build and its retries)", n, want)
	}
	if live != 0 {
		t.Errorf("%d golden actors left running after the failed rebuilds", live)
	}
	if name, key := goldenTagOf(t, e, "tpl"); name != old || key != oldKey {
		t.Errorf("after the failed rebuilds the template names %s (%s), want the old golden %s (%s)", name, key, old, oldKey)
	}
	a := mkActor(t, e, "a1", "tpl", nil)
	if from := restoredFrom(t, g, a); from != oldKey {
		t.Errorf("an actor after the failed rebuilds restored from %q, want the old golden %q", from, oldKey)
	}
	if left, err := e.Server.Reconcile(ctxT(t)); err != nil || left != 0 {
		t.Errorf("Reconcile after the rebuild's retries = %d, %v; want nothing owed", left, err)
	}
	if again, _, _ := g.counts(); again != n {
		t.Errorf("Reconcile asked for %d more snapshots after the retries ended", again-n)
	}
}

// TestGoldenRebuildsBounded (DEFAUL-1606): after an upgrade, Reconcile rebuilds at most Config.GoldenRebuilds
// templates' goldens at a time, and in the end all of them.
func TestGoldenRebuildsBounded(t *testing.T) {
	conformance.GoldenWarmup = 150 * time.Millisecond
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

	g := watchGolden(upgrade(t, e, "h1", guestNew), false)
	for _, n := range tpls {
		waitRebuilt(t, e, n, old[n])
	}
	if n, maxLive, _ := g.counts(); n != len(tpls) || maxLive != 1 {
		t.Errorf("rebuilds: %d golden snapshots, at most %d golden actors at once; want %d, one at a time", n, maxLive, len(tpls))
	}
}

// TestSameGuestRebuildsNothing (DEFAUL-1606): a herdlet that restarts on the guest that built the golden rebuilds
// nothing, and neither does a pool with both guests during a rolling upgrade: the old golden serves while any worker
// runs its guest, and once none does it is rebuilt once, and not again.
func TestSameGuestRebuildsNothing(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4, Guest: guestOld})
	goldenTemplate(t, e, "tpl", nil)
	waitGolden(t, e, "tpl")
	old, oldKey := goldenTagOf(t, e, "tpl")

	g1 := watchGolden(upgrade(t, e, "h1", guestOld), false)
	g2 := watchGolden(e.AddHerdlet(t, conformance.HerdletSpec{Name: "h2", Slots: 4, Guest: guestNew}), false)
	reconcileFor(t, e, time.Second)
	n1, _, _ := g1.counts()
	n2, _, _ := g2.counts()
	if n1+n2 != 0 {
		t.Errorf("the same guest, and a pool with both: %d golden snapshots, want none", n1+n2)
	}
	if name, key := goldenTagOf(t, e, "tpl"); name != old || key != oldKey {
		t.Errorf("the template names %s (%s), want still %s (%s)", name, key, old, oldKey)
	}

	// The rolling upgrade ends: no worker runs the old guest.
	g1 = watchGolden(upgrade(t, e, "h1", guestNew), false)
	waitRebuilt(t, e, "tpl", old)
	reconcileFor(t, e, time.Second)
	n1, _, _ = g1.counts()
	n2, _, _ = g2.counts()
	if n1+n2 != 1 {
		t.Errorf("after the rolling upgrade: %d golden snapshots, want 1", n1+n2)
	}
}
