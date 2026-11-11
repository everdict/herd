package conformance_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/store"
	"github.com/everdict/herd/store/memstore"
)

// age makes the snapshot object (or file) at key in the harness's disk store d old.
func age(t *testing.T, e *conformance.Env, key string, d time.Duration) {
	t.Helper()
	when := time.Now().Add(-d)
	if err := os.Chtimes(filepath.Join(e.SnapshotDir, "objects", filepath.FromSlash(key)), when, when); err != nil {
		t.Fatal(err)
	}
}

// plant puts an object no record names, as a suspend whose answer never came back, a golden attempt herd-api forgot,
// or an older herd leaves one.
func plant(t *testing.T, e *conformance.Env, key string) {
	t.Helper()
	if _, err := e.Snapshots.Put(context.Background(), key, strings.NewReader("snapshot no record names")); err != nil {
		t.Fatal(err)
	}
}

// TestSweepRemovesWhatNoRecordNames (PLT-462, A-58): herd-api's sweep removes a snapshot object no actor, tag or
// template record names once it is older than the grace, and the stale temporary file of a write that never ended.
// An object a suspended actor or a tag names stays whatever its age, and so does an unnamed one younger than the
// grace (a write whose record is not saved yet); the actor still resumes from its own. On 8241c2c nothing removed such
// an object: 821 MB named by nothing stayed in the live bucket.
func TestSweepRemovesWhatNoRecordNames(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES)
	mkActor(t, e, "a", "tpl", nil)
	akey := suspend(t, e, "a").GetUuid()
	mkActor(t, e, "b", "tpl", nil)
	suspend(t, e, "b")
	tag, _ := timedTag(t, e, "t1", "b")
	bkey := tag.GetStatus().GetSnapshot().GetUuid()
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "b")}); err != nil {
		t.Fatalf("DeleteActor b: %v", err)
	}
	orphan, young := e.Space+"/actors/ghost/0000/1", e.Space+"/actors/ghost/0000/2"
	plant(t, e, orphan)
	plant(t, e, young)
	for _, k := range []string{akey, bkey, orphan} {
		age(t, e, k, 2*time.Hour)
	}
	stale := filepath.Join(e.SnapshotDir, "objects", e.Space, ".tmp-dead")
	if err := os.WriteFile(stale, []byte("a write that never ended"), 0o600); err != nil {
		t.Fatal(err)
	}
	age(t, e, e.Space+"/.tmp-dead", 25*time.Hour)

	r, err := e.Server.SweepSnapshots(ctx)
	if err != nil {
		t.Fatalf("SweepSnapshots: %v", err)
	}
	wantObject(t, e, orphan, false, "a sweep, for an object no record names")
	wantObject(t, e, young, true, "a sweep, for an unnamed object younger than the grace")
	wantObject(t, e, akey, true, "a sweep, for the object a suspended actor names")
	wantObject(t, e, bkey, true, "a sweep, for the object a tag names")
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale temporary file after a sweep: %v, want it removed", err)
	}
	if r.Removed != 1 || r.Aborted != 1 || r.Young != 1 || r.Named != 2 {
		t.Errorf("sweep report %+v; want 1 removed, 1 stale write aborted, 1 young, 2 named", r)
	}
	if _, err := e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "a")}); err != nil {
		t.Fatalf("ResumeActor a after the sweep: %v", err)
	}
	if r, err := e.Server.SweepSnapshots(ctx); err != nil || r.Removed != 0 {
		t.Errorf("a second sweep = %+v, %v; want nothing more removed", r, err)
	}
}

// TestSweepOwesWhatItCouldNotRemove (A-58): a delete the store refuses is counted owed and logged, the object stays,
// and the next pass removes it.
func TestSweepOwesWhatItCouldNotRemove(t *testing.T) {
	logs := captureLog(t)
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 1})
	ctx := ctxT(t)
	orphan := e.Space + "/actors/ghost/0000/1"
	plant(t, e, orphan)
	age(t, e, orphan, 2*time.Hour)
	e.FailSnapshotDeletes(true)
	r, err := e.Server.SweepSnapshots(ctx)
	if err != nil || r.Owed != 1 || r.Removed != 0 {
		t.Errorf("a sweep whose delete failed = %+v, %v; want 1 owed, nothing removed", r, err)
	}
	wantObject(t, e, orphan, true, "a sweep whose delete failed")
	if !strings.Contains(logs.String(), "owed") || !strings.Contains(logs.String(), orphan) {
		t.Errorf("the log does not name %s as owed:\n%s", orphan, logs.String())
	}
	e.FailSnapshotDeletes(false)
	if r, err := e.Server.SweepSnapshots(ctx); err != nil || r.Removed != 1 {
		t.Errorf("the next sweep = %+v, %v; want the owed object removed", r, err)
	}
	wantObject(t, e, orphan, false, "the next sweep")
}

// TestSweepRemovesNothingWhenRecordsCannotBeRead (A-58): the sweep reads every record kind before it removes
// anything; a List that fails removes nothing, however old the objects.
func TestSweepRemovesNothingWhenRecordsCannotBeRead(t *testing.T) {
	recs := &failingLists{Store: memstore.New()}
	prev := conformance.NewStore
	conformance.NewStore = func(testing.TB) store.Store { return recs }
	defer func() { conformance.NewStore = prev }()
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 1})
	ctx := ctxT(t)
	orphan := e.Space + "/actors/ghost/0000/1"
	plant(t, e, orphan)
	age(t, e, orphan, 2*time.Hour)
	recs.fail.Store(true)
	if r, err := e.Server.SweepSnapshots(ctx); err == nil || r.Removed != 0 {
		t.Errorf("a sweep whose record read failed = %+v, %v; want an error and nothing removed", r, err)
	}
	wantObject(t, e, orphan, true, "a sweep whose record read failed")
}

// TestSweepOnAnotherRecordStoreRemovesNothing (A-58, PLT-458): a herd-api started on another record store (an empty
// one, as on 2026-11-11) names none of the objects in the snapshot store it shares; its sweep must remove none of them.
// The first sweep claims the snapshot store for its record store, and a sweep from any other refuses, naming both.
func TestSweepOnAnotherRecordStoreRemovesNothing(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES)
	mkActor(t, e, "a", "tpl", nil)
	akey := suspend(t, e, "a").GetUuid()
	age(t, e, akey, 2*time.Hour)
	if _, err := e.Server.SweepSnapshots(ctx); err != nil {
		t.Fatalf("SweepSnapshots on the records that name the object: %v", err)
	}
	wantObject(t, e, akey, true, "a sweep on the records that name it")
	own := e.Server.StoreID()

	e.Store = memstore.New()
	e.RestartAPI(t)
	_, err := e.Server.SweepSnapshots(ctx)
	if err == nil || !strings.Contains(err.Error(), own) || !strings.Contains(err.Error(), e.Server.StoreID()) {
		t.Errorf("a sweep from another record store: %v; want a refusal naming both stores (%s, %s)", err, own, e.Server.StoreID())
	}
	wantObject(t, e, akey, true, "a sweep by a herd-api on another record store")
}

// TestResuspendDropsThePreviousGeneration (PLT-462, A-58): a re-suspend deletes the actor's previous snapshot once the
// new one is verified and recorded, unless a tag still names it, in which case it goes with the tag; generations keep
// counting up. On 8241c2c every generation stayed until the actor's delete.
func TestResuspendDropsThePreviousGeneration(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES)
	mkActor(t, e, "a", "tpl", nil)
	resume := func() {
		t.Helper()
		if _, err := e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "a")}); err != nil {
			t.Fatalf("ResumeActor: %v", err)
		}
	}
	g1 := suspend(t, e, "a")
	resume()
	g2 := suspend(t, e, "a")
	wantObject(t, e, g1.GetUuid(), false, "the second suspend, for the first generation")
	wantObject(t, e, g2.GetUuid(), true, "the second suspend, for its own generation")
	if g2.GetGeneration() != 2 {
		t.Errorf("the second suspend's generation is %d, want 2", g2.GetGeneration())
	}
	a, _ := getActor(t, e, "a")
	if n := len(a.GetStatus().GetSnapshots()); n != 1 {
		t.Errorf("the actor lists %d snapshots after the second suspend, want the one that exists", n)
	}

	// A tag of the suspended actor references its generation, which then outlives the next suspend.
	timedTag(t, e, "t", "a")
	resume()
	g3 := suspend(t, e, "a")
	if g3.GetGeneration() != 3 || g3.GetUuid() == g2.GetUuid() {
		t.Errorf("the third suspend is generation %d under %s, want 3 under a key of its own", g3.GetGeneration(), g3.GetUuid())
	}
	wantObject(t, e, g2.GetUuid(), true, "the third suspend, for the generation a tag names")
	if _, err := e.Client.DeleteTag(ctx, &ateapi.DeleteTagRequest{Tag: oref(e, "t")}); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}
	wantObject(t, e, g2.GetUuid(), false, "the delete of the tag, its last holder")
	resume()
	g4 := suspend(t, e, "a")
	wantObject(t, e, g3.GetUuid(), false, "the fourth suspend, for the third generation")
	if _, err := e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "a")}); err != nil {
		t.Fatalf("ResumeActor from generation %d: %v", g4.GetGeneration(), err)
	}
}
