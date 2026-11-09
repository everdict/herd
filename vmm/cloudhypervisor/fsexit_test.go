//go:build linux

package cloudhypervisor

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/herdlet"
	"github.com/everdict/herd/vmm"
)

// exitBound is the bound herd states for an actor whose VM's virtiofsd exited to leave RUNNING (ADR A-47): the
// herdlet's next watch tick, plus that check's own time (it ends cloud-hypervisor within one tick), plus herd-api's
// write of CRASHED.
const exitBound = 2*herdlet.DefaultWatch + time.Second

// vmProcs is the fake processes of kind ("ch" or "fs") that VM vm's directory names.
func (h *fakeHost) vmProcs(vm, kind string) []*fakeProc {
	h.mu.Lock()
	defer h.mu.Unlock()
	var ps []*fakeProc
	for _, p := range h.procs {
		if p.kind == kind && filepath.Base(filepath.Dir(p.marker)) == vm {
			ps = append(ps, p)
		}
	}
	return ps
}

// TestExitedWhenVirtiofsdExits (criterion virtiofsd-reported, PLT-372): a VM whose virtiofsd exited while its
// cloud-hypervisor still runs is a VM that exited. Exited answers true with a why naming virtiofsd and its share, and
// the VM goes down: its cloud-hypervisor is ended rather than left with a share that blocks every access. The same
// holds for a VMM that finds the VM again from its pid files (a restarted herdlet). Today Exited watches only
// cloud-hypervisor and answers false: red.
func TestExitedWhenVirtiofsdExits(t *testing.T) {
	r := newRig(t)
	v := r.vmm("a", tigerLake)
	ctx := ctxT(t, 10*time.Second)
	for _, id := range []string{"x", "y"} {
		if _, err := v.Boot(ctx, r.spec(id)); err != nil {
			t.Fatalf("Boot %s: %v", id, err)
		}
		if gone, why, err := v.Exited(ctx, id); gone || err != nil {
			t.Fatalf("Exited %s of a running VM = %v, %q, %v", id, gone, why, err)
		}
	}
	for _, c := range []struct {
		id string
		v  *VMM
	}{{"x", v}, {"y", r.vmm("a", tigerLake)}} {
		fs := r.host.vmProcs(c.id, "fs")
		if len(fs) != 1 {
			t.Fatalf("VM %s has %d virtiofsd processes", c.id, len(fs))
		}
		r.host.mu.Lock()
		fs[0].status = "signal: killed"
		r.host.mu.Unlock()
		fs[0].exit() // kill -9 of the VM's virtiofsd
		cctx, cancel := context.WithTimeout(ctx, herdlet.DefaultWatch)
		gone, why, err := c.v.Exited(cctx, c.id)
		cancel()
		if err != nil || !gone {
			t.Errorf("Exited %s with its virtiofsd gone = %v, %q, %v; want true", c.id, gone, why, err)
			continue
		}
		if !strings.Contains(why, "virtiofsd") || !strings.Contains(why, "volumes") {
			t.Errorf("Exited %s: why %q names neither virtiofsd nor its share", c.id, why)
		}
		for _, ch := range r.host.vmProcs(c.id, "ch") {
			if ok, _ := ch.Exited(); !ok {
				t.Errorf("VM %s answered exited while its cloud-hypervisor (pid %d) still runs", c.id, ch.pid)
			}
		}
	}
	for _, id := range []string{"x", "y"} {
		if p, err := v.Delete(ctx, id); p != vmm.PresenceGone || err != nil {
			t.Errorf("Delete %s = %v, %v", id, p, err)
		}
	}
	if n := r.host.running(); n != 0 {
		t.Errorf("%d processes running after both VMs were deleted", n)
	}
}

// crashRig is herd-api and one herdlet on the Cloud Hypervisor backend over the fake host, with a template whose
// actors share a volume.
type crashRig struct {
	r    *rig
	e    *conformance.Env
	logs *lockedBuffer
}

func newCrashRig(t *testing.T, watch time.Duration) *crashRig {
	logs := &lockedBuffer{}
	prev := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	r := newRig(t)
	e := conformance.Start(t, conformance.HerdletSpec{Name: "ch-1", Slots: 4, Backend: r.vmm("a", tigerLake),
		Configure: func(a *herdlet.Agent) { a.Boot, a.Watch = r.spec("template"), watch }})
	if _, err := e.Client.CreateActorTemplate(ctxT(t, 10*time.Second), &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata:   &ateapi.ResourceMetadata{Atespace: e.Space, Name: "tpl"},
		Containers: []*ateapi.Container{{Name: "main", Image: "busybox:1.36"}},
		SnapshotConfig: &ateapi.SnapshotConfig{GoldenSnapshotConfig: &ateapi.GoldenSnapshotConfig{Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED},
			PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY},
	}}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	return &crashRig{r: r, e: e, logs: logs}
}

func (c *crashRig) ref(name string) *ateapi.ObjectRef {
	return &ateapi.ObjectRef{Atespace: c.e.Space, Name: name}
}

// actor creates a running actor and answers its uid, the VM's id.
func (c *crashRig) actor(t *testing.T, ctx context.Context, name string) string {
	t.Helper()
	a, err := c.e.Client.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{Atespace: c.e.Space, Name: name}, ActorTemplate: c.ref("tpl")}})
	if err != nil {
		t.Fatalf("CreateActor %s: %v", name, err)
	}
	if st := a.GetStatus().GetState(); st != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("CreateActor %s: %v", name, st)
	}
	return a.GetMetadata().GetUid()
}

func (c *crashRig) state(t *testing.T, ctx context.Context, name string) *ateapi.Actor {
	t.Helper()
	a, err := c.e.Client.GetActor(ctx, &ateapi.GetActorRequest{Actor: c.ref(name)})
	if err != nil {
		t.Fatalf("GetActor %s: %v", name, err)
	}
	return a
}

// leavesRunning waits up to d for the actor to leave RUNNING and answers it and how long that took.
func (c *crashRig) leavesRunning(t *testing.T, ctx context.Context, name string, d time.Duration) (*ateapi.Actor, time.Duration) {
	t.Helper()
	start := time.Now()
	for {
		a := c.state(t, ctx, name)
		if a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING || time.Since(start) > d {
			return a, time.Since(start)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// crashedNamingVirtiofsd checks that the actor is CRASHED with the typed reason, its VM and virtiofsd named, and that
// the VM's processes are all gone: the reported VM went down.
func (c *crashRig) crashedNamingVirtiofsd(t *testing.T, a *ateapi.Actor, took time.Duration, vm string) {
	t.Helper()
	if st := a.GetStatus().GetState(); st != ateapi.ActorState_ACTOR_STATE_CRASHED {
		t.Fatalf("actor whose virtiofsd exited is %v after %v, want ACTOR_STATE_CRASHED within %v", st, took.Round(time.Millisecond), exitBound)
	}
	msg := a.GetStatus().GetCrash().GetMessage()
	if !strings.HasPrefix(msg, "hypervisor-exited:") || !strings.Contains(msg, vm) || !strings.Contains(msg, "virtiofsd") {
		t.Errorf("crash message %q, want the typed reason hypervisor-exited, the VM id %s and virtiofsd", msg, vm)
	}
	for _, p := range append(c.r.host.vmProcs(vm, "ch"), c.r.host.vmProcs(vm, "fs")...) {
		if ok, _ := p.Exited(); !ok {
			t.Errorf("the crashed VM's %s process (pid %d) still runs", p.kind, p.pid)
		}
	}
	t.Logf("CRASHED %v after the fault: %s", took.Round(time.Millisecond), msg)
}

// TestActorCrashedWhenItsVirtiofsdExits (criterion virtiofsd-reported, PLT-372): kill -9 of one VM's virtiofsd while
// its actor runs, at the herdlet's default watch. herd-api moves that actor to CRASHED within exitBound with a reason
// naming virtiofsd, the VM's cloud-hypervisor is ended, the other actor stays RUNNING, and the crashed actor deletes
// with nothing left. Today the actor stays RUNNING (as on the real host, 3 of 3 past 60 s): red.
func TestActorCrashedWhenItsVirtiofsdExits(t *testing.T) {
	c := newCrashRig(t, 0)
	ctx := ctxT(t, 30*time.Second)
	vm := c.actor(t, ctx, "dies")
	c.actor(t, ctx, "lives")

	fs := c.r.host.vmProcs(vm, "fs")
	if len(fs) != 1 {
		t.Fatalf("VM %s has %d virtiofsd processes", vm, len(fs))
	}
	c.r.host.mu.Lock()
	fs[0].status = "signal: killed"
	c.r.host.mu.Unlock()
	fs[0].exit()

	a, took := c.leavesRunning(t, ctx, "dies", exitBound)
	c.crashedNamingVirtiofsd(t, a, took, vm)
	if st := c.state(t, ctx, "lives").GetStatus().GetState(); st != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("the actor whose VM still runs is %v, want RUNNING", st)
	}
	for _, name := range []string{"dies", "lives"} {
		if _, err := c.e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: c.ref(name)}); err != nil {
			t.Fatalf("DeleteActor %s: %v", name, err)
		}
	}
	if n := c.r.host.running(); n != 0 {
		t.Errorf("%d processes still running after both actors were deleted", n)
	}
}

// TestGuestRestartReportedAtVirtiofsdsExit (criterion restart-prompt, PLT-370): the guest of a VM with a share
// restarts. Cloud Hypervisor resets the VM, virtiofsd shuts down at its client's disconnect, and the VMM would wait 60
// s for a vhost-user connection, answering nothing, before it exits. herd reports the actor CRASHED within the same
// exitBound, naming virtiofsd, and ends the VMM rather than waiting out its timeout. Today the exit is seen only when
// the VMM gives up (61.3 s on the real host): red.
func TestGuestRestartReportedAtVirtiofsdsExit(t *testing.T) {
	c := newCrashRig(t, 0)
	ctx := ctxT(t, 30*time.Second)
	vm := c.actor(t, ctx, "restarts")
	ch := c.r.host.vmProcs(vm, "ch")
	if len(ch) != 1 {
		t.Fatalf("VM %s has %d cloud-hypervisor processes", vm, len(ch))
	}
	ch[0].ch.restart()

	a, took := c.leavesRunning(t, ctx, "restarts", exitBound)
	c.crashedNamingVirtiofsd(t, a, took, vm)
	if _, err := c.e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: c.ref("restarts")}); err != nil {
		t.Fatalf("DeleteActor: %v", err)
	}
	if n := c.r.host.running(); n != 0 {
		t.Errorf("%d processes still running after the actor was deleted", n)
	}
}

// TestOwnTeardownIsNoExit (criterion restart-prompt): what herd itself does to a VM with a share is not an exit, at a
// watch fast enough to look inside every step. A tag of a running actor (a snapshot that leaves the VM running), a
// suspend (snapshot, then the VM stopped with its virtiofsd), a resume (a new virtiofsd for the restored VM) and a
// delete report no exit, and no actor is CRASHED. The resumed actor's new virtiofsd is the one watched: its exit
// crashes the actor. Neutralized (cut no longer stops watching, herdlet.go), herd's own suspend and delete read as
// exits: red.
func TestOwnTeardownIsNoExit(t *testing.T) {
	c := newCrashRig(t, 5*time.Millisecond)
	ctx := ctxT(t, 30*time.Second)
	vm := c.actor(t, ctx, "a")
	other := c.actor(t, ctx, "b")
	settle := func() { time.Sleep(20 * 5 * time.Millisecond) }

	if _, err := c.e.Client.CreateTag(ctx, &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: c.e.Space, Name: "t1"}, SourceActor: c.ref("a")}}); err != nil {
		t.Fatalf("CreateTag of the running actor: %v", err)
	}
	settle()
	if r, err := c.e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: c.ref("a")}); err != nil ||
		r.GetActor().GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("SuspendActor = %v, %v", r.GetActor().GetStatus().GetState(), err)
	}
	settle()
	if r, err := c.e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: c.ref("a")}); err != nil ||
		r.GetActor().GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("ResumeActor = %v, %v", r.GetActor().GetStatus().GetState(), err)
	}
	settle()
	if _, err := c.e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: c.ref("b")}); err != nil {
		t.Fatalf("DeleteActor b: %v", err)
	}
	settle()
	if a := c.state(t, ctx, "a"); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("after herd's own tag, suspend and resume the actor is %v (%s), want RUNNING", a.GetStatus().GetState(), a.GetStatus().GetCrash().GetMessage())
	}
	out := c.logs.String()
	for _, id := range []string{vm, other} {
		if strings.Contains(out, "reports VM "+id+" exited") {
			t.Errorf("herd's own work on VM %s was reported as an exit; log:\n%s", id, out)
		}
	}

	var live []*fakeProc
	for _, p := range c.r.host.vmProcs(vm, "fs") {
		if ok, _ := p.Exited(); !ok {
			live = append(live, p)
		}
	}
	if len(live) != 1 {
		t.Fatalf("the resumed VM %s has %d running virtiofsd processes, want 1", vm, len(live))
	}
	live[0].exit()
	a, took := c.leavesRunning(t, ctx, "a", exitBound)
	c.crashedNamingVirtiofsd(t, a, took, vm)
}
