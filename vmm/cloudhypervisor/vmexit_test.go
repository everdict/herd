//go:build linux

package cloudhypervisor

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/herdlet"
)

// lockedBuffer collects the standard logger's output while herd-api and the herdlet write to it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestActorCrashedWhenItsVMExits (criterion vm-exit-noticed, PLT-194): herd-api and a herdlet on the Cloud Hypervisor
// backend, driving the fake Cloud Hypervisor API server, run two actors. One actor's cloud-hypervisor process exits
// on its own (nothing in herd asked it to). herd-api moves that actor out of RUNNING to CRASHED with a typed reason
// naming the VM, and logs the exit with the VM id; the other actor stays RUNNING, and the crashed actor can still be
// deleted, which stops what the dead VM left behind. A herdlet that never looks at its VMs again leaves the actor
// RUNNING: red.
func TestActorCrashedWhenItsVMExits(t *testing.T) {
	logs := &lockedBuffer{}
	prev := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	r := newRig(t)
	e := conformance.Start(t, conformance.HerdletSpec{Name: "ch-1", Slots: 2, Backend: r.vmm("a", tigerLake),
		Configure: func(a *herdlet.Agent) { a.Boot = r.spec("template") }})
	ctx := ctxT(t, 30*time.Second)
	if _, err := e.Client.CreateActorTemplate(ctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata:   &ateapi.ResourceMetadata{Atespace: e.Space, Name: "tpl"},
		Containers: []*ateapi.Container{{Name: "main", Image: "busybox:1.36"}},
		// The test counts the herdlet's slots: no golden actor takes one.
		SnapshotConfig: &ateapi.SnapshotConfig{GoldenSnapshotConfig: &ateapi.GoldenSnapshotConfig{Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED}},
	}}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	ref := func(name string) *ateapi.ObjectRef { return &ateapi.ObjectRef{Atespace: e.Space, Name: name} }
	uids := map[string]string{}
	for _, name := range []string{"dies", "lives"} {
		a, err := e.Client.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
			Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: name}, ActorTemplate: ref("tpl")}})
		if err != nil {
			t.Fatalf("CreateActor %s: %v", name, err)
		}
		if a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
			t.Fatalf("CreateActor %s: %v", name, a.GetStatus().GetState())
		}
		uids[name] = a.GetMetadata().GetUid()
	}
	vm := uids["dies"]

	// The VM's cloud-hypervisor process exits behind herd's back (the guest powered off, the VMM crashed, a kill).
	r.host.mu.Lock()
	var dead *fakeProc
	for _, p := range r.host.procs {
		if p.kind == "ch" && strings.Contains(p.marker, "/"+vm+"/") {
			dead = p
		}
	}
	r.host.mu.Unlock()
	if dead == nil {
		t.Fatalf("no cloud-hypervisor process for VM %s", vm)
	}
	dead.exit()

	var got *ateapi.Actor
	deadline := time.Now().Add(10 * time.Second)
	for {
		a, err := e.Client.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref("dies")})
		if err != nil {
			t.Fatalf("GetActor: %v", err)
		}
		got = a
		if a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := got.GetStatus().GetState(); st != ateapi.ActorState_ACTOR_STATE_CRASHED {
		t.Fatalf("actor whose cloud-hypervisor process exited is %v after 10 s, want ACTOR_STATE_CRASHED", st)
	}
	crash := got.GetStatus().GetCrash()
	if msg := crash.GetMessage(); !strings.HasPrefix(msg, "hypervisor-exited:") || !strings.Contains(msg, vm) {
		t.Errorf("crash message %q, want the typed reason hypervisor-exited and the VM id %s", msg, vm)
	}
	if crash.GetCrashTime() == nil {
		t.Errorf("crash has no crash_time")
	}
	if got.GetStatus().GetWorkerAssignment() != nil {
		t.Errorf("a CRASHED actor keeps its worker assignment %v", got.GetStatus().GetWorkerAssignment())
	}
	out := logs.String()
	if !strings.Contains(out, "VM "+vm+" exited") || !strings.Contains(out, "CRASHED") {
		t.Errorf("nothing logged the exit of VM %s and the move to CRASHED; log:\n%s", vm, out)
	}

	lives, err := e.Client.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref("lives")})
	if err != nil {
		t.Fatalf("GetActor lives: %v", err)
	}
	if st := lives.GetStatus().GetState(); st != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("the actor whose VM still runs is %v, want RUNNING", st)
	}

	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref("dies")}); err != nil {
		t.Fatalf("DeleteActor of the crashed actor: %v", err)
	}
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref("lives")}); err != nil {
		t.Fatalf("DeleteActor lives: %v", err)
	}
	if n := r.host.running(); n != 0 {
		t.Errorf("%d processes still running after both actors were deleted", n)
	}
}
