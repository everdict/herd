package conformance_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/herdlet"
	"github.com/everdict/herd/herdlet/machine"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
)

// exitingVMM is the fake hypervisor with an exit observer whose answer per VM the test sets.
type exitingVMM struct {
	*fake.Fake
	mu      sync.Mutex
	answers map[string]error // VM id → nil: exited; an error: could not be observed; absent: running
}

func (v *exitingVMM) set(id string, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.answers[id] = err
}

func (v *exitingVMM) Exited(_ context.Context, id string) (bool, string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	err, ok := v.answers[id]
	if !ok {
		return false, "", nil
	}
	if err != nil {
		// A backend that half-answers: the contract says never true with an error, and the herdlet must not trust it.
		return true, "", err
	}
	return true, "powered off", nil
}

// TestExitUnobservedIsNotCrashed (unknown is not crashed): a herdlet that cannot observe an actor's VM reports
// nothing, and the actor stays RUNNING; once the VM is observed exited, the actor is CRASHED with the typed reason. A
// suspended actor is not a VM the herdlet watches: an exit answer for it changes nothing.
func TestExitUnobservedIsNotCrashed(t *testing.T) {
	v := &exitingVMM{Fake: fake.New(chCaps(famA), nil), answers: map[string]error{}}
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 3, Backend: v,
		Configure: func(a *herdlet.Agent) { a.Watch = 10 * time.Millisecond }})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	a1 := mkActor(t, e, "a1", "tpl", nil)
	a2 := mkActor(t, e, "a2", "tpl", nil)
	if _, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a2")}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}

	v.set(a1.GetMetadata().GetUid(), errors.New("observe pid 1234: permission denied"))
	v.set(a2.GetMetadata().GetUid(), nil)
	time.Sleep(300 * time.Millisecond)
	if a, _ := getActor(t, e, "a1"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("an actor whose VM could not be observed is %v, want RUNNING (unknown is not crashed)", a.GetStatus().GetState())
	}
	if a, _ := getActor(t, e, "a2"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("a suspended actor is %v after an exit answer for its old VM, want SUSPENDED", a.GetStatus().GetState())
	}

	v.set(a1.GetMetadata().GetUid(), nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		a, _ := getActor(t, e, "a1")
		if a.GetStatus().GetState() == ateapi.ActorState_ACTOR_STATE_CRASHED {
			if m := a.GetStatus().GetCrash().GetMessage(); !strings.HasPrefix(m, "hypervisor-exited:") || !strings.Contains(m, "powered off") {
				t.Errorf("crash message %q, want the typed reason and the herdlet's detail", m)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("an actor whose VM was observed exited is %v, want CRASHED", a.GetStatus().GetState())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("DeleteActor of the crashed actor: %v", err)
	}
	if p, _ := v.Inspect(ctx, a1.GetMetadata().GetUid()); p != vmm.PresenceGone {
		t.Errorf("the crashed actor's VM is %v after its delete, want gone", p)
	}
}

// TestContainerExitIsReportedCrashed (PLT-230): herd-guest records in the VM's share a container whose main process
// ended (initd.ExitPath: its status and last output); the herdlet reports it although the VM still runs — on a host
// whose hypervisor cannot tell exits too — and the actor is CRASHED with the container's status and output in its
// crash message. An actor whose share records nothing stays RUNNING, and the crashed one deletes.
func TestContainerExitIsReportedCrashed(t *testing.T) {
	reg := images()
	var m *machine.Machines
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 3, Caps: chCaps(famA),
		Configure: func(a *herdlet.Agent) { machines(t, reg, &m)(a); a.Watch = 10 * time.Millisecond }})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	a1 := mkActor(t, e, "a1", "tpl", nil)
	mkActor(t, e, "a2", "tpl", nil)

	exit := "container main exited with status 1; its last output:\n1:C 01 Nov 2026 04:40:00.000 # oO0OoO0OoO0Oo Redis is starting\nsu-exec: setgroups: Operation not permitted\n"
	p := filepath.Join(m.ShareDir(a1.GetMetadata().GetUid()), initd.ExitPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(exit), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		a, _ := getActor(t, e, "a1")
		if a.GetStatus().GetState() == ateapi.ActorState_ACTOR_STATE_CRASHED {
			m := a.GetStatus().GetCrash().GetMessage()
			if !strings.HasPrefix(m, "container-exited:") || !strings.Contains(m, "container main exited with status 1") ||
				!strings.Contains(m, "su-exec: setgroups: Operation not permitted") {
				t.Errorf("crash message %q, want the typed reason, the container's status and its last output", m)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("an actor whose container exited is %v, want CRASHED", a.GetStatus().GetState())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if a, _ := getActor(t, e, "a2"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("an actor whose containers run is %v, want RUNNING", a.GetStatus().GetState())
	}
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("DeleteActor of the crashed actor: %v", err)
	}
}
