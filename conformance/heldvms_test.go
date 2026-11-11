package conformance_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/store"
	"github.com/everdict/herd/store/memstore"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
)

// forgetInstance rewrites the actor's record as one that carries no herdlet process (a record whose start a herdlet
// older than PLT-371 answered, or whose instance was lost): A-19's restart rule concludes nothing about it.
func forgetInstance(t *testing.T, e *conformance.Env, name string) {
	t.Helper()
	ctx := ctxT(t)
	r, err := e.Store.Get(ctx, store.Key{Kind: store.KindActor, Space: e.Space, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(r.State, &st); err != nil {
		t.Fatal(err)
	}
	if st["instance"] == nil {
		t.Fatalf("actor %s carries no herdlet instance to forget: state %s", name, r.State)
	}
	delete(st, "instance")
	if r.State, err = json.Marshal(st); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
}

// awaitGone waits up to d for the herdlet's hypervisor to hold no VM of the actor, and returns its presence as last
// read.
func awaitGone(t *testing.T, h *conformance.Herdlet, a *ateapi.Actor, d time.Duration) vmm.Presence {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		p := present(t, h, a)
		if p == vmm.PresenceGone || time.Now().After(deadline) {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestLinkCrashesARecordWhoseVMTheHerdletDoesNotHold (PLT-459): when a herdlet links, it names the VMs it holds, and
// herd-api settles each record on that worker that holds a VM the herdlet does not name and answers gone: a RUNNING
// actor is CRASHED with the reason, within restartBound of the link, also when its record carries no herdlet process
// (where A-19's restart rule concludes nothing, as on 2026-11-11, when three RUNNING records outlived their VMs). A
// suspended actor holds no VM and stays SUSPENDED; an actor the herdlet does hold stays RUNNING.
func TestLinkCrashesARecordWhoseVMTheHerdletDoesNotHold(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 4, Caps: chCaps(famA)})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	mkActor(t, e, "a2", "tpl", nil)
	if _, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a2")}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	forgetInstance(t, e, "a1")

	e.Restart(t, "w1")
	a := awaitState(t, e, "a1", ateapi.ActorState_ACTOR_STATE_CRASHED, restartBound)
	if st := a.GetStatus().GetState(); st != ateapi.ActorState_ACTOR_STATE_CRASHED {
		t.Fatalf("actor a1 is %v %v after w1 linked holding no VM of it, want CRASHED", st, restartBound)
	}
	if m := a.GetStatus().GetCrash().GetMessage(); !strings.HasPrefix(m, "vm-missing:") || !strings.Contains(m, "w1") {
		t.Errorf("crash message %q, want the reason vm-missing naming worker w1", m)
	}
	if a, _ := getActor(t, e, "a2"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("a suspended actor is %v after its herdlet linked, want SUSPENDED", a.GetStatus().GetState())
	}
	if _, err := e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "a2")}); err != nil {
		t.Fatalf("ResumeActor: %v", err)
	}
	e.Reconnect(t, "w1")
	time.Sleep(restartBound)
	if a, _ := getActor(t, e, "a2"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("actor a2, whose VM w1 holds, is %v after a relink, want RUNNING (crash %q)",
			a.GetStatus().GetState(), a.GetStatus().GetCrash().GetMessage())
	}
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("DeleteActor of the crashed actor: %v", err)
	}
}

// TestLinkStopsAVMNoRecordNames (PLT-459): a VM the herdlet holds, started under this herd-api's store, that no
// actor record names is stopped when the herdlet links; a VM a record names keeps running.
func TestLinkStopsAVMNoRecordNames(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 4, Caps: chCaps(famA)})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	a1 := mkActor(t, e, "a1", "tpl", nil)
	a2 := mkActor(t, e, "a2", "tpl", nil)
	r, err := e.Store.Get(ctx, store.Key{Kind: store.KindActor, Space: e.Space, Name: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.Delete(ctx, r.Key, r.UID, r.Version); err != nil {
		t.Fatal(err)
	}

	h := e.Reconnect(t, "w1")
	if p := awaitGone(t, h, a1, restartBound); p != vmm.PresenceGone {
		t.Fatalf("the VM of a1, which no record names, is %v %v after w1 linked, want gone", p, restartBound)
	}
	if p := present(t, h, a2); p != vmm.PresencePresent {
		t.Fatalf("the VM of a2, which its record names, is %v, want present", p)
	}
	if a, _ := getActor(t, e, "a2"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("actor a2 is %v, want RUNNING", a.GetStatus().GetState())
	}
}

// TestLinkToAnotherStoreStopsNothing (PLT-459, PLT-458): a herd-api running on another record store (one that holds
// none of the herdlet's actors) stops none of the VMs the herdlet started under the store it linked to before: the
// herdlet names each VM with the store it was started under, and herd-api stops only those of its own.
func TestLinkToAnotherStoreStopsNothing(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 4, Caps: chCaps(famA)})
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	a1 := mkActor(t, e, "a1", "tpl", nil)

	e.Store = memstore.New()
	e.RestartAPI(t)
	time.Sleep(restartBound)
	if p := present(t, e.Herdlets["w1"], a1); p != vmm.PresencePresent {
		t.Fatalf("the VM of a1 is %v after w1 linked to a herd-api on another store, want present", p)
	}
}

// TestLinkKeepsARecordWhoseVMIsPresent (PLT-459): a record whose VM the herdlet's list leaves out is settled only on
// the herdlet's answer that the VM is gone. A herdlet process that does not name a VM its hypervisor still runs (here:
// a new process over the same hypervisor, whose list starts empty) leaves the actor RUNNING, and the VM running.
func TestLinkKeepsARecordWhoseVMIsPresent(t *testing.T) {
	v := fake.New(chCaps(famA), nil)
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 4, Backend: v})
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	a1 := mkActor(t, e, "a1", "tpl", nil)
	forgetInstance(t, e, "a1")

	e.Restart(t, "w1")
	time.Sleep(restartBound)
	if a, _ := getActor(t, e, "a1"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("actor a1, whose VM the hypervisor still runs, is %v after a herdlet that does not name it linked, want RUNNING (crash %q)",
			a.GetStatus().GetState(), a.GetStatus().GetCrash().GetMessage())
	}
	if p, err := v.Inspect(ctxT(t), a1.GetMetadata().GetUid()); err != nil || p != vmm.PresencePresent {
		t.Fatalf("the VM of a1 is %v (%v), want present", p, err)
	}
}
