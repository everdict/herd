package conformance_test

import (
	"strings"
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc/codes"
)

// TestDeleteConvergesOnVerifiedGone: a delete the herdlet verified removes the record, the VM and the snapshots.
func TestDeleteConvergesOnVerifiedGone(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	a := mkActor(t, e, "a1", "tpl", nil)
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "a1"),
		Options: &ateapi.DeleteOptions{Uid: a.GetMetadata().GetUid(), Version: a.GetMetadata().GetVersion()}}); err != nil {
		t.Fatalf("DeleteActor: %v", err)
	}
	_, err := e.Client.GetActor(ctx, &ateapi.GetActorRequest{Actor: oref(e, "a1")})
	wantCode(t, "GetActor after delete", err, codes.NotFound)
	if p := present(t, e.Herdlets["h1"], a); p != vmm.PresenceGone {
		t.Errorf("VM after delete: %v", p)
	}

	// A suspended actor's snapshot objects go with it.
	b := mkActor(t, e, "b1", "tpl", nil)
	s, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "b1")})
	if err != nil {
		t.Fatal(err)
	}
	uri := s.GetActor().GetStatus().GetSnapshots()[0].GetDurableSnapshot().GetObject().GetSnapshotUri()
	key, _, _ := strings.Cut(strings.TrimPrefix(uri, "herd-snapshot:///"), "?")
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "b1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Snapshots.Stat(ctx, key); err == nil {
		t.Errorf("snapshot %s survived its actor's delete", key)
	}
	_ = b
}

// TestDeleteNotVerifiedStaysOwed: a VM that survives, a herdlet that answers unknown or not at all, and a herdlet
// that says "done" without verifying the VM gone all keep the delete owed: the actor stays DELETING, visible with the
// reason, and Reconcile retries it until the herdlet verifies gone.
func TestDeleteNotVerifiedStaysOwed(t *testing.T) {
	cases := []struct {
		name   string
		inject func(h *conformance.Herdlet)
		reason string
	}{
		{"VM survives the delete", func(h *conformance.Herdlet) { h.VMM.LeaveOnDelete() }, "(present)"},
		{"unreachable hypervisor", func(h *conformance.Herdlet) { h.VMM.Unreachable(vmm.OpDelete) }, "(unknown)"},
		{"hung hypervisor (timeout)", func(h *conformance.Herdlet) { h.VMM.Hang(vmm.OpDelete) }, "(unknown)"},
		{"silent herdlet (timeout)", func(h *conformance.Herdlet) {
			h.Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) { return o, w.Op != link.OpDelete })
		}, "no answer"},
		{"accepted without verification", func(h *conformance.Herdlet) {
			h.Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) {
				if w.Op == link.OpDelete {
					return link.Outcome{ID: o.ID, Result: link.ResultDone, Reason: "accepted"}, true
				}
				return o, true
			})
		}, "accepted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
			ctx := ctxT(t)
			mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
			a := mkActor(t, e, "a1", "tpl", nil)
			h := e.Herdlets["h1"]
			c.inject(h)
			_, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "a1")})
			wantCode(t, "DeleteActor", err, codes.Unavailable)
			for i := 0; i < 2; i++ {
				got, owed := getActor(t, e, "a1")
				if got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_DELETING || got.GetMetadata().GetDeleteTime() == nil {
					t.Errorf("pass %d: state %v delete_time %v, want DELETING with delete_time", i, got.GetStatus().GetState(), got.GetMetadata().GetDeleteTime())
				}
				if !strings.HasPrefix(owed, "delete: ") || !strings.Contains(owed, c.reason) {
					t.Errorf("pass %d: owed %q, want the delete owed with %q", i, owed, c.reason)
				}
				// Retried and still not verified: still owed.
				if left, err := e.Server.Reconcile(ctx); err != nil || left != 1 {
					t.Errorf("pass %d: Reconcile = %d owed, %v; want 1", i, left, err)
				}
			}
			h.VMM.Clear()
			h.Rewrite(nil)
			if left, err := e.Server.Reconcile(ctx); err != nil || left != 0 {
				t.Fatalf("Reconcile after the herdlet recovered = %d, %v", left, err)
			}
			_, err = e.Client.GetActor(ctx, &ateapi.GetActorRequest{Actor: oref(e, "a1")})
			wantCode(t, "GetActor after converged delete", err, codes.NotFound)
			if p := present(t, h, a); p != vmm.PresenceGone {
				t.Errorf("VM: %v", p)
			}
		})
	}
}
