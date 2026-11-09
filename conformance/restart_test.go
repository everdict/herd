package conformance_test

import (
	"strings"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/link"
	"google.golang.org/grpc/codes"
)

// restartBound is how soon after a new herdlet process registers the test wants the actors whose VMs ended with the
// old one settled: herd-api settles them as the new link comes up, in the background (PLT-371).
const restartBound = 2 * time.Second

// silent makes a herdlet give no answer to op (the call ends unknown and the step is owed).
func silent(op link.Op) func(link.Work, link.Outcome) (link.Outcome, bool) {
	return func(w link.Work, o link.Outcome) (link.Outcome, bool) { return o, w.Op != op }
}

// awaitState waits up to d for the actor to read want, and returns it as last read.
func awaitState(t *testing.T, e *conformance.Env, name string, want ateapi.ActorState, d time.Duration) *ateapi.Actor {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		a, _ := getActor(t, e, name)
		if a.GetStatus().GetState() == want || time.Now().After(deadline) {
			return a
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func wantRestartCrash(t *testing.T, a *ateapi.Actor, worker string) {
	t.Helper()
	if st := a.GetStatus().GetState(); st != ateapi.ActorState_ACTOR_STATE_CRASHED {
		t.Fatalf("actor %s is %v %v after its herdlet restarted, want CRASHED (its VM ended with the old herdlet process)",
			a.GetMetadata().GetName(), st, restartBound)
	}
	if m := a.GetStatus().GetCrash().GetMessage(); !strings.HasPrefix(m, "herdlet-restarted:") || !strings.Contains(m, worker) {
		t.Errorf("crash message %q, want the reason herdlet-restarted naming worker %s", m, worker)
	}
}

// TestHerdletRestartCrashesItsActors (PLT-371): a herdlet restarted under its worker name does not adopt the VMs it
// left (ADR A-19), so an actor that was RUNNING on it is CRASHED, with the reason naming the restart, within
// restartBound of the new process's registration, with no Reconcile and no exec asking. A suspended actor holds no VM
// there and stays SUSPENDED; it resumes on the new process, and that actor and one created after the restart stay
// RUNNING.
func TestHerdletRestartCrashesItsActors(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 4, Caps: chCaps(famA)})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	mkActor(t, e, "a2", "tpl", nil)
	if _, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a2")}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}

	e.Restart(t, "w1")
	wantRestartCrash(t, awaitState(t, e, "a1", ateapi.ActorState_ACTOR_STATE_CRASHED, restartBound), "w1")
	if a, _ := getActor(t, e, "a2"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("a suspended actor is %v after its herdlet restarted, want SUSPENDED", a.GetStatus().GetState())
	}
	if _, err := e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "a2")}); err != nil {
		t.Fatalf("ResumeActor on the new herdlet process: %v", err)
	}
	mkActor(t, e, "a3", "tpl", nil)
	time.Sleep(300 * time.Millisecond)
	if _, err := e.Server.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, n := range []string{"a2", "a3"} {
		if a, _ := getActor(t, e, n); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
			t.Errorf("actor %s, started on the new herdlet process, is %v, want RUNNING", n, a.GetStatus().GetState())
		}
	}
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("DeleteActor of the crashed actor: %v", err)
	}
}

// TestLinkBlipCrashesNothing (PLT-371): a link that breaks and is dialled again by the same herdlet process leaves its
// VMs running, so nothing is crashed: a RUNNING actor stays RUNNING, and one whose suspend was owed across the blip
// suspends on the next Reconcile.
func TestLinkBlipCrashesNothing(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 4, Caps: chCaps(famA)})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	mkActor(t, e, "a2", "tpl", nil)
	e.Herdlets["w1"].Rewrite(silent(link.OpSnapshot))
	_, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a2")})
	wantCode(t, "SuspendActor with no answer to the snapshot", err, codes.Unavailable)
	e.Herdlets["w1"].Rewrite(nil)

	e.Reconnect(t, "w1")
	time.Sleep(restartBound)
	if a, _ := getActor(t, e, "a1"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("a running actor is %v after a link blip of the same herdlet process, want RUNNING (crash %q)",
			a.GetStatus().GetState(), a.GetStatus().GetCrash().GetMessage())
	}
	if a, _ := getActor(t, e, "a2"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDING {
		t.Fatalf("an actor whose suspend is owed is %v after a link blip, want SUSPENDING (crash %q)",
			a.GetStatus().GetState(), a.GetStatus().GetCrash().GetMessage())
	}
	if _, err := e.Server.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if a, _ := getActor(t, e, "a2"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("an owed suspend is %v after Reconcile across a link blip, want SUSPENDED", a.GetStatus().GetState())
	}
	if a, _ := getActor(t, e, "a1"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("a running actor is %v after Reconcile across a link blip, want RUNNING", a.GetStatus().GetState())
	}
}

// TestHerdletRestartDuringSuspend (PLT-371): a suspend whose snapshot got no answer before the herdlet restarted has
// no durable snapshot, and its VM ended with the old process. The actor is CRASHED with the restart's reason, and a
// Reconcile does not turn it back to RUNNING because the new process cannot snapshot a VM it never held.
func TestHerdletRestartDuringSuspend(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 4, Caps: chCaps(famA)})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	e.Herdlets["w1"].Rewrite(silent(link.OpSnapshot))
	_, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a1")})
	wantCode(t, "SuspendActor with no answer to the snapshot", err, codes.Unavailable)

	e.Restart(t, "w1")
	if _, err := e.Server.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	wantRestartCrash(t, awaitState(t, e, "a1", ateapi.ActorState_ACTOR_STATE_CRASHED, restartBound), "w1")
	if _, err := e.Server.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if a, _ := getActor(t, e, "a1"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_CRASHED {
		t.Fatalf("a crashed actor is %v after another Reconcile, want CRASHED", a.GetStatus().GetState())
	}
}

// TestHerdletRestartDuringResume (PLT-371): a resume whose restore got no answer before the herdlet restarted is not
// crashed: the start is owed, and Reconcile asks the new process, which holds no VM, and restores there. The actor is
// RUNNING on the new process and stays so.
func TestHerdletRestartDuringResume(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 4, Caps: chCaps(famA)})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	if _, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	e.Herdlets["w1"].Rewrite(silent(link.OpRestore))
	_, err := e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "a1")})
	wantCode(t, "ResumeActor with no answer to the restore", err, codes.Unavailable)

	e.Restart(t, "w1")
	time.Sleep(restartBound)
	if a, _ := getActor(t, e, "a1"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RESUMING {
		t.Fatalf("an actor whose resume is owed is %v after its herdlet restarted, want RESUMING (crash %q)",
			a.GetStatus().GetState(), a.GetStatus().GetCrash().GetMessage())
	}
	if _, err := e.Server.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := e.Server.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if a, _ := getActor(t, e, "a1"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("an owed resume is %v after Reconcile on the new herdlet process, want RUNNING (crash %q)",
			a.GetStatus().GetState(), a.GetStatus().GetCrash().GetMessage())
	}
}
