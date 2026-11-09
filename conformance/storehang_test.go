package conformance_test

import (
	"context"
	"strings"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc/codes"
)

// TestResumeAndDeleteAnswerWhileTheStoreHangs (PLT-375): while the snapshot store stops answering (MinIO frozen with
// docker pause), ResumeActor and DeleteActor of a suspended actor answer within their bound with Unavailable, the
// work owed, never a success; once the store answers again Reconcile finishes both. The bound is the call timeout
// per store call: a resume waits for the Stat of its snapshot, then for its herdlet's restore call (2 × CallTimeout);
// a delete for the delete of its first snapshot object (CallTimeout).
//
// Its counterexample is herd at c5ea26b: the store calls have no deadline, so neither call answers before the
// client's own deadline (10 s here; on the real host none answered in 330 s).
func TestResumeAndDeleteAnswerWhileTheStoreHangs(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	// Long enough to outlast both calls on today's code and still reconcile after them.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	a := mkActor(t, e, "a1", "tpl", nil)
	mkActor(t, e, "b1", "tpl", nil)
	for _, n := range []string{"a1", "b1"} {
		if _, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, n)}); err != nil {
			t.Fatalf("SuspendActor %s: %v", n, err)
		}
	}
	b, _ := getActor(t, e, "b1")
	uri := b.GetStatus().GetSnapshots()[0].GetDurableSnapshot().GetObject().GetSnapshotUri()
	bKey, _, _ := strings.Cut(strings.TrimPrefix(uri, "herd-snapshot:///"), "?")

	e.HangSnapshots(true)
	// The client waits far longer than the bound, so a call that does not answer within it is seen as such.
	const client = 10 * time.Second
	slack := 2 * time.Second
	call := func(what string, bound time.Duration, f func(context.Context) error) {
		t.Helper()
		cctx, cancel := context.WithTimeout(ctx, client)
		defer cancel()
		start := time.Now()
		err := f(cctx)
		took := time.Since(start)
		t.Logf("%s while the store hangs: %v after %v (bound %v)", what, err, took.Round(time.Millisecond), bound)
		wantCode(t, what, err, codes.Unavailable)
		if took > bound+slack {
			t.Errorf("%s answered after %v; want within %v (+%v slack)", what, took.Round(time.Millisecond), bound, slack)
		}
	}
	call("ResumeActor", 2*conformance.CallTimeout, func(ctx context.Context) error {
		_, err := e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "a1")})
		return err
	})
	call("DeleteActor", conformance.CallTimeout, func(ctx context.Context) error {
		_, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "b1")})
		return err
	})
	// What the records say during the hang is true: both are in progress, owed to Reconcile.
	if got, owed := getActor(t, e, "a1"); got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RESUMING || !strings.HasPrefix(owed, "resume: ") {
		t.Errorf("a1 while the store hangs: %v, owed %q; want RESUMING with the resume owed", got.GetStatus().GetState(), owed)
	}
	if got, owed := getActor(t, e, "b1"); got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_DELETING || !strings.HasPrefix(owed, "delete: ") {
		t.Errorf("b1 while the store hangs: %v, owed %q; want DELETING with the delete owed", got.GetStatus().GetState(), owed)
	}

	e.HangSnapshots(false)
	if left, err := e.Server.Reconcile(ctx); err != nil || left != 0 {
		t.Fatalf("Reconcile once the store answers = %d owed, %v; want 0", left, err)
	}
	if got, owed := getActor(t, e, "a1"); got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING || owed != "" {
		t.Errorf("a1 after Reconcile: %v, owed %q; want RUNNING", got.GetStatus().GetState(), owed)
	}
	if p := present(t, e.Herdlets["h1"], a); p != vmm.PresencePresent {
		t.Errorf("a1's VM after Reconcile: %v", p)
	}
	_, err := e.Client.GetActor(ctx, &ateapi.GetActorRequest{Actor: oref(e, "b1")})
	wantCode(t, "GetActor b1 after Reconcile", err, codes.NotFound)
	if _, err := e.Snapshots.Stat(ctx, bKey); err == nil {
		t.Errorf("b1's snapshot %s survived its delete", bKey)
	}
}
