package conformance_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/store"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// TestSuspendDurableBeforeRelease: by the time the herdlet is asked to release the VM, the actor record already holds
// the snapshot's key+digest and the snapshot store verifies the object against it.
func TestSuspendDurableBeforeRelease(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	a := mkActor(t, e, "a1", "tpl", nil)
	h := e.Herdlets["h1"]

	var mu sync.Mutex
	var released bool
	var atRelease []string
	h.Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) {
		if w.Op != link.OpDelete {
			return o, true
		}
		mu.Lock()
		defer mu.Unlock()
		released = true
		// Read what is durable at the moment of release, straight from the stores.
		r, err := e.Store.Get(context.Background(), store.Key{Kind: store.KindActor, Space: e.Space, Name: "a1"})
		if err != nil {
			atRelease = append(atRelease, "record: "+err.Error())
			return o, true
		}
		var rec ateapi.Actor
		if err := proto.Unmarshal(r.Data, &rec); err != nil || len(rec.GetStatus().GetSnapshots()) == 0 {
			atRelease = append(atRelease, "no snapshot recorded before release")
			return o, true
		}
		s := rec.GetStatus().GetSnapshots()[len(rec.GetStatus().GetSnapshots())-1]
		uri := s.GetDurableSnapshot().GetObject().GetSnapshotUri()
		key, digest, ok := strings.Cut(strings.TrimPrefix(uri, "herd-snapshot:///"), "?digest=")
		if !ok || s.GetDurableSnapshot().GetState() != ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED {
			atRelease = append(atRelease, "snapshot recorded without key+digest: "+uri)
			return o, true
		}
		if _, err := e.Snapshots.Verify(context.Background(), key, digest); err != nil {
			atRelease = append(atRelease, "snapshot object not durable: "+err.Error())
		}
		return o, true
	})
	resp, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a1")})
	if err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !released {
		t.Fatal("the worker was never released")
	}
	for _, p := range atRelease {
		t.Errorf("at release: %s", p)
	}
	got := resp.GetActor()
	if got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("state %v, want SUSPENDED", got.GetStatus().GetState())
	}
	snaps := got.GetStatus().GetSnapshots()
	if len(snaps) != 1 || snaps[0].GetFidelity() != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY || snaps[0].GetLocality() != famA ||
		snaps[0].GetOwner() != ateapi.SnapshotOwner_SNAPSHOT_OWNER_ACTOR {
		t.Errorf("snapshots = %v", snaps)
	}
	if got.GetStatus().GetWorkerAssignment() != nil {
		t.Errorf("a suspended actor still has a worker assignment")
	}
	if p := present(t, h, a); p != vmm.PresenceGone {
		t.Errorf("VM after suspend: %v, want gone", p)
	}
}

// TestSuspendUnknownStaysSuspending: a herdlet that answers unknown, does not answer in time, or falls silent leaves
// the actor SUSPENDING with the reason — never SUSPENDED — and its VM is not released.
func TestSuspendUnknownStaysSuspending(t *testing.T) {
	cases := []struct {
		name   string
		inject func(h *conformance.Herdlet)
		reason string
	}{
		{"unreachable hypervisor", func(h *conformance.Herdlet) { h.VMM.Unreachable(vmm.OpSnapshot) }, "did not answer"},
		{"hung hypervisor (timeout)", func(h *conformance.Herdlet) { h.VMM.Hang(vmm.OpSnapshot) }, "deadline"},
		{"silent herdlet (timeout)", func(h *conformance.Herdlet) {
			h.Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) { return o, w.Op != link.OpSnapshot })
		}, "no answer"},
		{"done without a snapshot", func(h *conformance.Herdlet) {
			h.Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) {
				if w.Op == link.OpSnapshot {
					o.Snapshot = nil
				}
				return o, true
			})
		}, "named no snapshot"},
		{"digest the store does not hold", func(h *conformance.Herdlet) {
			h.Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) {
				if w.Op == link.OpSnapshot && o.Snapshot != nil {
					o.Snapshot.Digest = "sha256:0000"
				}
				return o, true
			})
		}, "digest mismatch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
			ctx := ctxT(t)
			mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
			a := mkActor(t, e, "a1", "tpl", nil)
			h := e.Herdlets["h1"]
			c.inject(h)
			_, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a1")})
			wantCode(t, "SuspendActor", err, codes.Unavailable)
			got, owed := getActor(t, e, "a1")
			if got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDING {
				t.Errorf("state %v, want SUSPENDING", got.GetStatus().GetState())
			}
			if !strings.HasPrefix(owed, "suspend: ") || !strings.Contains(owed, c.reason) {
				t.Errorf("owed %q, want the suspend owed with %q", owed, c.reason)
			}
			if len(got.GetStatus().GetSnapshots()) != 0 {
				t.Errorf("an unverified snapshot was recorded: %v", got.GetStatus().GetSnapshots())
			}
			if p := present(t, h, a); p != vmm.PresencePresent {
				t.Errorf("VM %v after an unknown suspend; it must not be released", p)
			}
			// Once the herdlet answers, the owed suspend converges.
			h.VMM.Clear()
			h.Rewrite(nil)
			// One Reconcile converges it, on a loaded host too: Reconcile's calls are bounded by
			// conformance.ReconcileCallTimeout, not by the CallTimeout a loaded herdlet's snapshot (fsyncs) can
			// outlast on every retry; a herdlet still finishing the timed-out exchange holds the VM until then.
			stop := load(t, t.TempDir())
			left, err := e.Server.Reconcile(ctx)
			stop()
			if err != nil || left != 0 {
				_, owed := getActor(t, e, "a1")
				t.Fatalf("Reconcile = %d owed, %v; want 0 (owed %q)", left, err, owed)
			}
			got, owed = getActor(t, e, "a1")
			if got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED || owed != "" {
				t.Errorf("after reconcile: %v owed %q, want SUSPENDED", got.GetStatus().GetState(), owed)
			}
		})
	}
}

// TestReleaseNotVerifiedKeepsSlot: when the VM survives the release, the snapshot is durable but the actor stays
// SUSPENDING with the release owed and the worker slot stays taken.
func TestReleaseNotVerifiedKeepsSlot(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 1})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	h := e.Herdlets["h1"]
	h.VMM.LeaveOnDelete()
	_, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a1")})
	wantCode(t, "SuspendActor", err, codes.Unavailable)
	got, owed := getActor(t, e, "a1")
	if got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDING || !strings.HasPrefix(owed, "release: ") || len(got.GetStatus().GetSnapshots()) != 1 {
		t.Errorf("state %v owed %q snapshots %d; want SUSPENDING, release owed, 1 durable snapshot", got.GetStatus().GetState(), owed, len(got.GetStatus().GetSnapshots()))
	}
	_, err = e.Client.CreateActor(ctx, actorReq(e, "a2", "tpl", nil))
	wantCode(t, "CreateActor while the slot is not verified free", err, codes.ResourceExhausted)
	h.VMM.Clear()
	if left, err := e.Server.Reconcile(ctx); err != nil || left != 0 {
		t.Fatalf("Reconcile = %d, %v", left, err)
	}
	mkActor(t, e, "a2", "tpl", nil)
}

// TestSuspendAsynchronous: DURABILITY_MODE_ASYNCHRONOUS answers SUSPENDING and converges in the background.
func TestSuspendAsynchronous(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	resp, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a1"),
		Options: &ateapi.SuspendOptions{Mode: ateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS}})
	if err != nil || resp.GetActor().GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDING {
		t.Fatalf("async SuspendActor = %v, %v; want SUSPENDING", resp.GetActor().GetStatus().GetState(), err)
	}
	e.Server.Wait()
	if got, _ := getActor(t, e, "a1"); got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("after the background suspend: %v", got.GetStatus().GetState())
	}
}

// TestResume: a suspended actor resumes from its durable snapshot; a VOLUMES-only host takes VOLUMES.
func TestResume(t *testing.T) {
	mac := conformance.HerdletSpec{Name: "mac", Slots: 2, Caps: vmm.Capabilities{Platform: vmm.PlatformVZ, Arch: "arm64", SuspendMaxFidelity: vmm.FidelityVolumes}}
	e := conformance.Start(t, mac)
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	a := mkActor(t, e, "a1", "tpl", nil)
	s, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a1")})
	if err != nil {
		t.Fatal(err)
	}
	if f := s.GetActor().GetStatus().GetSnapshots()[0].GetFidelity(); f != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES {
		t.Errorf("a VOLUMES-only host took %v", f)
	}
	r, err := e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "a1")})
	if err != nil || !r.GetResumed() || r.GetActor().GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("ResumeActor = %v, %v", r, err)
	}
	if p := present(t, e.Herdlets["mac"], a); p != vmm.PresencePresent {
		t.Errorf("VM after resume: %v", p)
	}
	r, err = e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "a1")})
	if err != nil || r.GetResumed() {
		t.Errorf("resuming a running actor: resumed=%v, %v", r.GetResumed(), err)
	}
}
