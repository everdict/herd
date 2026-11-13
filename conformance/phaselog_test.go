package conformance_test

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/vmm/fake"
	"github.com/everdict/herd/vmm/fakeguest"
)

// phaseLine is herdlet's one line per snapshot (PLT-591): the seconds and bytes of each phase of it.
var phaseLine = regexp.MustCompile(`herdlet(?: \S+)?: snapshot (\S+) of (\S+) \(([a-z]+)\): flush ([0-9.]+)s, vmm snapshot ([0-9.]+)s, ` +
	`bundle ([0-9.]+)s (\d+) B, hash ([0-9.]+)s (\d+) B, upload ([0-9.]+)s (\d+) B, total ([0-9.]+)s`)

// TestSnapshotPhasesAreLogged (PLT-591): a suspend's time is read from herdlet's log, phase by phase. A MEMORY suspend
// through the fake VMM, whose guest flush takes at least 20 ms, logs exactly one line naming the snapshot's object with
// the seconds of the flush, the VMM's snapshot, the bundle, its hash and its upload, and the bytes the bundle, the hash
// and the upload moved: the object's size, each.
func TestSnapshotPhasesAreLogged(t *testing.T) {
	dir, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	v := &slowGuests{VMM: fakeguest.New(fake.New(ubuntuCaps, nil), dir, nil)}
	t.Cleanup(v.Close)
	v.SyncFS = func(*os.File) error {
		v.syncs.Add(1)
		time.Sleep(20 * time.Millisecond)
		return nil
	}
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 2, Backend: v, Configure: slowGuestHost(t, v)})
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	logs := captureLog(t)
	if _, err := e.Client.SuspendActor(ctxT(t), &ateapi.SuspendActorRequest{Actor: oref(e, "a1"),
		Options: &ateapi.SuspendOptions{Mode: ateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS}}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		a, _ := getActor(t, e, "a1")
		if a.GetStatus().GetState() == ateapi.ActorState_ACTOR_STATE_SUSPENDED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("actor not SUSPENDED within 10s: %v", a.GetStatus().GetState())
		}
		if _, err := e.Server.Reconcile(ctxT(t)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	objs, err := e.Snapshots.List(ctxT(t), "")
	if err != nil || len(objs) != 1 {
		t.Fatalf("snapshot store holds %v (%v), want the one suspend's object", objs, err)
	}
	lines := phaseLine.FindAllStringSubmatch(logs.String(), -1)
	if len(lines) != 1 {
		t.Fatalf("herdlet logged %d phase lines for one snapshot, want 1; the log:\n%s", len(lines), logs.String())
	}
	l := lines[0]
	if l[1] != objs[0].Key || l[3] != "suspend" {
		t.Errorf("the phase line names snapshot %s (%s), want %s (suspend): %s", l[1], l[3], objs[0].Key, l[0])
	}
	secs := func(i int) float64 {
		f, err := strconv.ParseFloat(l[i], 64)
		if err != nil {
			t.Fatalf("%q: %v", l[i], err)
		}
		return f
	}
	if f := secs(4); f < 0.020 {
		t.Errorf("flush %.3fs, want at least the guest's 20 ms: %s", f, l[0])
	}
	var sum float64
	for _, i := range []int{4, 5, 6, 8, 10} {
		sum += secs(i)
	}
	if total := secs(12); total+0.002 < sum {
		t.Errorf("the phases add up to %.3fs, more than the total %.3fs: %s", sum, total, l[0])
	}
	for _, i := range []int{7, 9, 11} {
		if n, _ := strconv.ParseInt(l[i], 10, 64); n != objs[0].Size {
			t.Errorf("a phase moved %d bytes, want the object's %d: %s", n, objs[0].Size, l[0])
		}
	}
}
