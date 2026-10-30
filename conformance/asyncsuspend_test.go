package conformance_test

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/herdlet"
	"github.com/everdict/herd/herdlet/machine"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
	"github.com/everdict/herd/vmm/fakeguest"
)

// slowGuests is a host whose VMs run the real herd-guest, start serving it only GuestDelay after the hypervisor
// answered the boot (as Cloud Hypervisor does), flush slowly, and are watched for an exit.
type slowGuests struct {
	*fakeguest.VMM
	syncs, watched atomic.Int32
}

func (v *slowGuests) Exited(ctx context.Context, id string) (bool, string, error) {
	v.watched.Add(1)
	p, err := v.Inspect(ctx, id)
	if err != nil {
		return false, "", err
	}
	return p == vmm.PresenceGone, "powered off", nil
}

// slowGuestHost gives a herdlet Machines over v whose snapshots flush the guest through machine.GuestFlush, the
// production client of herd-guest's Sync, and watches its VMs every 10 ms.
func slowGuestHost(t *testing.T, v *slowGuests) func(*herdlet.Agent) {
	return func(a *herdlet.Agent) {
		dir := t.TempDir()
		m, err := machine.New(machine.Config{VMM: v, Guests: v, Flush: machine.GuestFlush{Guests: v, Port: fakeguest.GuestPort},
			ShareDir: filepath.Join(dir, "shares"), Base: vmm.Spec{Kernel: "vmlinux"},
			Images: &image.Cache{Dir: filepath.Join(dir, "images"), Source: images(), Builder: &imagetest.Builder{},
				Platform: image.Platform{OS: "linux", Architecture: "amd64"}}})
		if err != nil {
			t.Fatal(err)
		}
		a.Machines, a.Watch = m, 10*time.Millisecond
	}
}

// TestAsyncSuspendWaitsForASlowGuest (PLT-200): an ASYNCHRONOUS SuspendActor right after the actor was created, on a
// host whose hypervisor answers the boot before herd-guest serves and whose guest flushes slowly, with the VM watcher
// running, answers SUSPENDING at once and ends SUSPENDED with a snapshot and no worker. The actor never reads RUNNING
// in between: a guest that is still starting is waited for (within the flush bound), not a failed snapshot.
func TestAsyncSuspendWaitsForASlowGuest(t *testing.T) {
	dir, err := os.MkdirTemp("", "pa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	v := &slowGuests{VMM: fakeguest.New(fake.New(ubuntuCaps, nil), dir, nil)}
	t.Cleanup(v.Close)
	v.GuestDelay = 250 * time.Millisecond
	v.SyncFS = func(*os.File) error {
		v.syncs.Add(1)
		time.Sleep(20 * time.Millisecond)
		return nil
	}
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 2, Backend: v, Configure: slowGuestHost(t, v)})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	// The watcher looks at the VM before the suspend comes (its guest is still starting).
	for v.watched.Load() == 0 {
		if ctx.Err() != nil {
			t.Fatal("the herdlet never checked the VM for an exit")
		}
		time.Sleep(time.Millisecond)
	}

	r, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a1"),
		Options: &ateapi.SuspendOptions{Mode: ateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS}})
	if err != nil {
		t.Fatalf("SuspendActor (ASYNCHRONOUS): %v", err)
	}
	if s := r.GetActor().GetStatus().GetState(); s != ateapi.ActorState_ACTOR_STATE_SUSPENDING {
		t.Fatalf("SuspendActor (ASYNCHRONOUS) answered %v, want SUSPENDING at once", s)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		a, owed := getActor(t, e, "a1")
		switch s := a.GetStatus().GetState(); s {
		case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
			if len(a.GetStatus().GetSnapshots()) != 1 || a.GetStatus().GetWorkerAssignment() != nil {
				t.Fatalf("SUSPENDED with snapshots %v and worker %v; want one snapshot and no worker",
					a.GetStatus().GetSnapshots(), a.GetStatus().GetWorkerAssignment())
			}
			if v.syncs.Load() == 0 {
				t.Fatalf("no guest flush: the suspend did not go through herd-guest's Sync")
			}
			return
		case ateapi.ActorState_ACTOR_STATE_SUSPENDING:
			// The harness's 400 ms call budget may run out on a loaded host while the guest is still starting;
			// the suspend is then owed, and Reconcile (as herd's loop does) takes it up again.
			if _, err := e.Server.Reconcile(ctx); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
		default:
			t.Fatalf("after an ASYNCHRONOUS suspend the actor is %v (owed %q), want SUSPENDING then SUSPENDED", s, owed)
		}
		if time.Now().After(deadline) {
			t.Fatalf("actor not SUSPENDED within 10s (owed %q)", owed)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// logBuffer collects what the process logs.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLog sends the process's log to a buffer until the test (and the Env started after it) ended.
func captureLog(t *testing.T) *logBuffer {
	l := &logBuffer{}
	out := log.Writer()
	log.SetOutput(io.MultiWriter(l, out))
	t.Cleanup(func() { log.SetOutput(out) })
	return l
}

func waitState(t *testing.T, e *conformance.Env, name string, want ateapi.ActorState) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		a, owed := getActor(t, e, name)
		if a.GetStatus().GetState() == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("actor %s is %v (owed %q), want %v", name, a.GetStatus().GetState(), owed, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWorkflowStepsAreLogged (PLT-200): herd-api logs each workflow's beginning and end and each herdlet call in it
// with the actor and the outcome, and the herdlet logs each piece of work with the VM and the outcome, so an
// asynchronous suspend that fails (here: herd-guest's Sync refuses) shows where, though no caller gets its error.
func TestWorkflowStepsAreLogged(t *testing.T) {
	logs := captureLog(t)
	dir, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	v := &slowGuests{VMM: fakeguest.New(fake.New(ubuntuCaps, nil), dir, nil)}
	t.Cleanup(v.Close)
	var refuse atomic.Bool
	v.SyncFS = func(*os.File) error {
		if refuse.Load() {
			return errors.New("input/output error")
		}
		return nil
	}
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 2, Backend: v, Configure: slowGuestHost(t, v)})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	uid := mkActor(t, e, "a1", "tpl", nil).GetMetadata().GetUid()
	async := &ateapi.SuspendActorRequest{Actor: oref(e, "a1"),
		Options: &ateapi.SuspendOptions{Mode: ateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS}}

	refuse.Store(true)
	if _, err := e.Client.SuspendActor(ctx, async); err != nil {
		t.Fatalf("SuspendActor (ASYNCHRONOUS): %v", err)
	}
	waitState(t, e, "a1", ateapi.ActorState_ACTOR_STATE_RUNNING)
	refuse.Store(false)
	if _, err := e.Client.SuspendActor(ctx, async); err != nil {
		t.Fatalf("SuspendActor (ASYNCHRONOUS): %v", err)
	}
	waitState(t, e, "a1", ateapi.ActorState_ACTOR_STATE_SUSPENDED)
	if _, err := e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("ResumeActor: %v", err)
	}
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("DeleteActor: %v", err)
	}
	e.Server.Wait()

	actor := "herd-api: actor " + e.Space + "/a1: "
	for _, want := range []string{
		actor + "create begins",
		actor + "boot of VM " + uid + " on herdlet w1: done after",
		actor + "create ends ACTOR_STATE_RUNNING",
		// The failed suspend: the herdlet call, its reason, and where it left the actor.
		actor + "asynchronous suspend begins (ACTOR_STATE_RUNNING)",
		actor + "snapshot (suspend) of VM " + uid + " on herdlet w1: failed",
		"flushing the guest",
		actor + "asynchronous suspend ends ACTOR_STATE_RUNNING (",
		// The suspend that succeeds: snapshot, release, SUSPENDED.
		actor + "snapshot (suspend) of VM " + uid + " on herdlet w1: done after",
		actor + "delete (suspend) of VM " + uid + " on herdlet w1: done, VM gone after",
		actor + "asynchronous suspend ends ACTOR_STATE_SUSPENDED",
		actor + "resume begins (ACTOR_STATE_SUSPENDED)",
		actor + "restore of VM " + uid + " on herdlet w1: done after",
		actor + "resume ends ACTOR_STATE_RUNNING",
		actor + "delete begins (ACTOR_STATE_RUNNING)",
		actor + "delete (delete) of VM " + uid + " on herdlet w1: done, VM gone after",
		actor + "delete ends: record deleted",
		// The herdlet's side of the same steps.
		"herdlet w1: boot of VM " + uid + " (actor " + e.Space + "/a1) begins",
		"herdlet w1: snapshot (suspend) of VM " + uid + " begins",
		"herdlet w1: snapshot (suspend) of VM " + uid + ": failed: ",
		"herdlet w1: snapshot (suspend) of VM " + uid + ": done after",
		"herdlet w1: restore of VM " + uid + " (actor " + e.Space + "/a1): done after",
		"herdlet w1: delete (delete) of VM " + uid + ": done, VM gone after",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log has no %q", want)
		}
	}
	if t.Failed() {
		t.Logf("the log:\n%s", logs)
	}
}
