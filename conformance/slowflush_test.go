package conformance_test

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/vmm/fake"
	"github.com/everdict/herd/vmm/fakeguest"
)

// TestSuspendWaitsForAFlushLongerThanTheCallTimeout (PLT-378): a guest whose flush (saving a large writable layer)
// outlasts herd-api's call timeout, and stays inside the herdlet's flush bound for its memory, suspends. herd-api
// learns before the flush that the snapshot may move the VM's memory (link.Progress), so its call waits as long as the
// herdlet's flush bound and does not cut the flush at CallTimeout.
func TestSuspendWaitsForAFlushLongerThanTheCallTimeout(t *testing.T) {
	dir, err := os.MkdirTemp("", "pf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	v := &slowGuests{VMM: fakeguest.New(fake.New(ubuntuCaps, nil), dir, nil)}
	t.Cleanup(v.Close)
	flush := 4 * conformance.CallTimeout
	var slept atomic.Bool
	v.SyncFS = func(*os.File) error {
		v.syncs.Add(1)
		if !slept.Swap(true) {
			time.Sleep(flush)
		}
		return nil
	}
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 2, Backend: v, Configure: slowGuestHost(t, v)})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	start := time.Now()
	r, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a1")})
	if err != nil {
		a, owed := getActor(t, e, "a1")
		t.Fatalf("SuspendActor with a %v flush (call timeout %v): %v after %v; actor %v, owed %q", flush, conformance.CallTimeout,
			err, time.Since(start), a.GetStatus().GetState(), owed)
	}
	if s := r.GetActor().GetStatus().GetState(); s != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("SuspendActor with a %v flush answered %v, want SUSPENDED", flush, s)
	}
	if !slept.Load() {
		t.Fatal("no guest flush: the suspend did not go through herd-guest's Sync")
	}
}
