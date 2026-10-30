package conformance_test

import (
	"strings"
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestFork: CreateTag{source_actor} then CreateActor{template, source_tag} gives a second running actor restored from
// the first one's snapshot, which keeps running.
func TestFork(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	ctx := ctxT(t)
	tpl := mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	src := mkActor(t, e, "src", "tpl", nil)
	tag, err := e.Client.CreateTag(ctx, &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "t1"}, SourceActor: oref(e, "src"),
	}})
	if err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	snap := tag.GetStatus().GetSnapshot()
	if snap.GetOwner() != ateapi.SnapshotOwner_SNAPSHOT_OWNER_TAG || tag.GetStatus().GetActorTemplateUid() != tpl.GetMetadata().GetUid() ||
		snap.GetDurableSnapshot().GetState() != ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED {
		t.Errorf("tag status = %v", tag.GetStatus())
	}
	req := actorReq(e, "fork", "tpl", nil)
	req.Actor.SourceTag = oref(e, "t1")
	fork, err := e.Client.CreateActor(ctx, req)
	if err != nil {
		t.Fatalf("CreateActor from tag: %v", err)
	}
	if fork.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING || fork.GetSourceTag().GetName() != "t1" || fork.GetMetadata().GetUid() == src.GetMetadata().GetUid() {
		t.Errorf("fork = %v", fork)
	}
	h := e.Herdlets["h1"]
	if present(t, h, fork) != vmm.PresencePresent || present(t, h, src) != vmm.PresencePresent {
		t.Errorf("after fork: fork VM %v, source VM %v; want both present", present(t, h, fork), present(t, h, src))
	}
	if g, err := e.Client.GetTag(ctx, &ateapi.GetTagRequest{Tag: oref(e, "t1")}); err != nil || g.GetSourceActor().GetName() != "src" {
		t.Errorf("GetTag = %v, %v", g, err)
	}
	// The tag outlives the source actor, and a tag can be taken from a suspended actor.
	if _, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "src")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Client.CreateTag(ctx, &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "t2"}, SourceActor: oref(e, "src"),
	}}); err != nil {
		t.Fatalf("CreateTag from a suspended actor: %v", err)
	}
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "src")}); err != nil {
		t.Fatal(err)
	}
	req = actorReq(e, "fork2", "tpl", nil)
	req.Actor.SourceTag = oref(e, "t2")
	if _, err := e.Client.CreateActor(ctx, req); err != nil {
		t.Errorf("CreateActor from a tag whose source is deleted: %v", err)
	}
	if _, err := e.Client.DeleteTag(ctx, &ateapi.DeleteTagRequest{Tag: oref(e, "t1")}); err != nil {
		t.Errorf("DeleteTag: %v", err)
	}
	// A tag of another template does not fork into this one.
	mkTemplate(t, e, "tpl2", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	req = actorReq(e, "wrong", "tpl2", nil)
	req.Actor.SourceTag = oref(e, "t2")
	_, err = e.Client.CreateActor(ctx, req)
	wantCode(t, "fork into another template", err, codes.FailedPrecondition)
}

// TestSchedulerSlots: a 4-slot herdlet takes 4 actors and refuses the 5th with a typed reason.
func TestSchedulerSlots(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	for _, n := range []string{"a1", "a2", "a3", "a4"} {
		mkActor(t, e, n, "tpl", nil)
	}
	_, err := e.Client.CreateActor(ctx, actorReq(e, "a5", "tpl", nil))
	wantCode(t, "5th actor on a 4-slot herdlet", err, codes.ResourceExhausted)
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "no free slot") {
		t.Errorf("refusal reason %q does not name the slots", msg)
	}
	list, err := e.Client.ListActors(ctx, &ateapi.ListActorsRequest{Atespace: e.Space})
	if err != nil || len(list.GetActors()) != 4 {
		t.Errorf("ListActors = %d actors, %v; want 4", len(list.GetActors()), err)
	}
	// A suspended actor frees its slot.
	if _, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatal(err)
	}
	mkActor(t, e, "a5", "tpl", nil)
	_, err = e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "a1")})
	wantCode(t, "resume onto a full herdlet", err, codes.ResourceExhausted)
	if got, _ := getActor(t, e, "a1"); got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("a refused resume left the actor %v", got.GetStatus().GetState())
	}
}

// TestSchedulerRestoreFamily: a MEMORY snapshot resumes only on a host of its restore family; when that family is
// full it is refused, not placed elsewhere. A VOLUMES snapshot resumes anywhere.
func TestSchedulerRestoreFamily(t *testing.T) {
	e := conformance.Start(t,
		conformance.HerdletSpec{Name: "tiger", Slots: 1, Labels: map[string]string{"host": "tiger"}, Caps: chCaps(famA)},
		conformance.HerdletSpec{Name: "lunar", Slots: 4, Labels: map[string]string{"host": "lunar"}, Caps: chCaps(famB)})
	ctx := ctxT(t)
	mkTemplate(t, e, "mem", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkTemplate(t, e, "vol", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES)

	m := mkActor(t, e, "m", "mem", map[string]string{"host": "tiger"})
	if n := m.GetStatus().GetAssignedNode(); n != "tiger" {
		t.Fatalf("selector placed m on %q", n)
	}
	if _, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "m")}); err != nil {
		t.Fatal(err)
	}
	// Take tiger's only slot; m's MEMORY snapshot (family tiger) must not go to lunar.
	mkActor(t, e, "filler", "vol", map[string]string{"host": "tiger"})
	// Drop m's selector so only the restore family keeps it off lunar.
	cur, _ := getActor(t, e, "m")
	if _, err := e.Client.UpdateActor(ctx, &ateapi.UpdateActorRequest{Actor: &ateapi.Actor{Metadata: cur.GetMetadata()}}); err != nil {
		t.Fatal(err)
	}
	_, err := e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "m")})
	wantCode(t, "MEMORY resume with its family full", err, codes.ResourceExhausted)
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "MEMORY snapshot of family") {
		t.Errorf("refusal reason %q does not name the restore family", msg)
	}
	if p := present(t, e.Herdlets["lunar"], m); p != vmm.PresenceGone {
		t.Errorf("m's VM is on lunar")
	}
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "filler")}); err != nil {
		t.Fatal(err)
	}
	r, err := e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "m")})
	if err != nil || r.GetActor().GetStatus().GetAssignedNode() != "tiger" {
		t.Fatalf("MEMORY resume with its family free = %v, %v; want tiger", r.GetActor().GetStatus().GetAssignedNode(), err)
	}

	// VOLUMES: suspended on tiger, resumed on lunar while tiger is full.
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "m")}); err != nil {
		t.Fatal(err)
	}
	mkActor(t, e, "v", "vol", map[string]string{"host": "tiger"})
	if _, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "v")}); err != nil {
		t.Fatal(err)
	}
	mkActor(t, e, "filler2", "vol", map[string]string{"host": "tiger"})
	cur, _ = getActor(t, e, "v")
	if f := cur.GetStatus().GetSnapshots()[0].GetFidelity(); f != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES {
		t.Fatalf("vol template snapshot at %v", f)
	}
	if _, err := e.Client.UpdateActor(ctx, &ateapi.UpdateActorRequest{Actor: &ateapi.Actor{Metadata: cur.GetMetadata()}}); err != nil {
		t.Fatal(err)
	}
	r, err = e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "v")})
	if err != nil || r.GetActor().GetStatus().GetAssignedNode() != "lunar" {
		t.Errorf("VOLUMES resume = %v, %v; want lunar", r.GetActor().GetStatus().GetAssignedNode(), err)
	}
}
