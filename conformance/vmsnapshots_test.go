package conformance_test

import (
	"slices"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/herdlet"
	"github.com/everdict/herd/herdlet/machine"
	"github.com/everdict/herd/vmm/fake"
)

// localSnapshotHost is a herdlet with Machines over a fake hypervisor whose snapshot store is its own (the host's
// vm-snapshots), sweeping it every 10 ms.
func localSnapshotHost(t *testing.T) (*conformance.Env, *fake.Store) {
	t.Helper()
	local := fake.NewStore()
	var m *machine.Machines
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 4, Backend: fake.New(chCaps(famA), local),
		Configure: func(a *herdlet.Agent) { machines(t, images(), &m)(a); a.SnapshotSweep = 10 * time.Millisecond }})
	return e, local
}

// settles waits until the host's snapshots are want, and fails with what they are otherwise.
func settles(t *testing.T, local *fake.Store, want []string, after string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !slices.Equal(local.IDs(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("after %s: the host holds snapshots %v; want %v", after, local.IDs(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// keeps checks that the host's snapshots stay want across several sweeps.
func keeps(t *testing.T, local *fake.Store, want []string, after string) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	if got := local.IDs(); !slices.Equal(got, want) {
		t.Fatalf("after %s: the host holds snapshots %v; want %v kept", after, got, want)
	}
}

// TestDeletedSuspendedActorLeavesNoSnapshotOnTheHost (PLT-374, criterion bounded): an actor deleted while suspended
// gets no work on its herdlet (herd-api only drops its objects), so its host's copy of the suspend's snapshot would
// stay for ever, as the supervisor measured on real KVM. The herdlet removes a copy once no object in the store holds
// its snapshot any more, which is when no actor or tag can restore from it (A-45).
func TestDeletedSuspendedActorLeavesNoSnapshotOnTheHost(t *testing.T) {
	e, local := localSnapshotHost(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	if _, err := e.Client.SuspendActor(ctxT(t), &ateapi.SuspendActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	held := local.IDs()
	if len(held) != 1 {
		t.Fatalf("after the suspend the host holds %v; want the suspend's snapshot", held)
	}
	// The actor may still resume from it: the copy is kept while its object is in the store.
	keeps(t, local, held, "the suspend")
	if _, err := e.Client.DeleteActor(ctxT(t), &ateapi.DeleteActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("DeleteActor: %v", err)
	}
	settles(t, local, nil, "the suspended actor's delete")
}

// TestTaggedSnapshotStaysAndRestores (PLT-374, criterion bounded): a snapshot a tag references outlives its actor's
// delete on the host, a fork of the tag restores, and the copy goes with the tag.
func TestTaggedSnapshotStaysAndRestores(t *testing.T) {
	e, local := localSnapshotHost(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	if _, err := e.Client.SuspendActor(ctxT(t), &ateapi.SuspendActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	held := local.IDs()
	if _, err := e.Client.CreateTag(ctxT(t), &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "t1"}, SourceActor: oref(e, "a1")}}); err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	if _, err := e.Client.DeleteActor(ctxT(t), &ateapi.DeleteActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("DeleteActor: %v", err)
	}
	keeps(t, local, held, "the tagged actor's delete")
	req := actorReq(e, "fork", "tpl", nil)
	req.Actor.SourceTag = oref(e, "t1")
	if a, err := e.Client.CreateActor(ctxT(t), req); err != nil || a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("CreateActor from the tag: %v, %v", a.GetStatus().GetState(), err)
	}
	keeps(t, local, held, "the fork")
	if _, err := e.Client.DeleteActor(ctxT(t), &ateapi.DeleteActorRequest{Actor: oref(e, "fork")}); err != nil {
		t.Fatalf("DeleteActor fork: %v", err)
	}
	keeps(t, local, held, "the fork's delete")
	if _, err := e.Client.DeleteTag(ctxT(t), &ateapi.DeleteTagRequest{Tag: oref(e, "t1")}); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}
	settles(t, local, nil, "the tag's delete")
}
