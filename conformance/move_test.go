package conformance_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/herdlet"
	"github.com/everdict/herd/herdlet/machine"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc/codes"
)

var (
	macCaps    = vmm.Capabilities{Platform: vmm.PlatformVZ, Arch: "arm64", SuspendMaxFidelity: vmm.FidelityVolumes}
	ubuntuCaps = chCaps(famA)
)

// images is a registry holding the two images of the test template for both architectures.
func images() *imagetest.Registry {
	reg := imagetest.NewRegistry()
	for _, repo := range []string{"everdict/agent", "everdict/sidecar"} {
		var ms []imagetest.Manifest
		for _, arch := range []string{"amd64", "arm64"} {
			ms = append(ms, imagetest.Manifest{Platform: image.Platform{OS: "linux", Architecture: arch},
				Config: image.Config{Cmd: []string{"/bin/run"}},
				Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "bin/run", Body: arch, Mode: 0o755})}})
		}
		reg.Push(repo, "1", ms...)
	}
	return reg
}

// machines gives a herdlet Machines over its own VMM, sharing the registry; it returns the Machines so the test can
// look at the host's shares as a guest would write them.
func machines(t *testing.T, reg *imagetest.Registry, out **machine.Machines) func(*herdlet.Agent) {
	return func(a *herdlet.Agent) {
		caps, err := a.VMM.Capabilities(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		m, err := machine.New(machine.Config{VMM: a.VMM, ShareDir: filepath.Join(dir, "shares"), Base: vmm.Spec{Kernel: "vmlinux"},
			Images: &image.Cache{Dir: filepath.Join(dir, "images"), Source: reg, Builder: &imagetest.Builder{},
				Platform: image.Platform{OS: "linux", Architecture: caps.Arch}}})
		if err != nil {
			t.Fatal(err)
		}
		a.Machines = m
		*out = m
	}
}

// twoContainerTemplate is an actor with two containers sharing one DurableDir volume.
func twoContainerTemplate(t *testing.T, e *conformance.Env, name string, f ateapi.SnapshotFidelity) *ateapi.ActorTemplate {
	t.Helper()
	tpl, err := e.Client.CreateActorTemplate(ctxT(t), &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: name},
		Containers: []*ateapi.Container{
			{Name: "agent", Image: "ghcr.io/everdict/agent:1", VolumeMounts: []*ateapi.VolumeMount{{Name: "work", MountPath: "/work"}}},
			{Name: "sidecar", Image: "ghcr.io/everdict/sidecar:1", Args: []string{"--watch", "/work"}, VolumeMounts: []*ateapi.VolumeMount{{Name: "work", MountPath: "/work"}}},
		},
		Volumes:        []*ateapi.Volume{{Name: "work", DurableDir: &ateapi.DurableDirVolumeSource{}}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: f},
	}})
	if err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	return tpl
}

func moveTo(t *testing.T, e *conformance.Env, name, host string) *ateapi.Actor {
	t.Helper()
	a, _ := getActor(t, e, name)
	a.WorkerSelector = &ateapi.Selector{MatchLabels: map[string]string{"host": host}}
	if _, err := e.Client.UpdateActor(ctxT(t), &ateapi.UpdateActorRequest{Actor: a}); err != nil {
		t.Fatalf("UpdateActor: %v", err)
	}
	r, err := e.Client.ResumeActor(ctxT(t), &ateapi.ResumeActorRequest{Actor: oref(e, name)})
	if err != nil {
		t.Fatalf("ResumeActor onto %s: %v", host, err)
	}
	if got := r.GetActor().GetStatus().GetAssignedNode(); got != host {
		t.Fatalf("resumed on %q, want %s", got, host)
	}
	return r.GetActor()
}

func suspend(t *testing.T, e *conformance.Env, name string) *ateapi.Snapshot {
	t.Helper()
	r, err := e.Client.SuspendActor(ctxT(t), &ateapi.SuspendActorRequest{Actor: oref(e, name)})
	if err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	if s := r.GetActor().GetStatus().GetState(); s != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("state %v after suspend", s)
	}
	snaps := r.GetActor().GetStatus().GetSnapshots()
	return snaps[len(snaps)-1]
}

func readVolume(t *testing.T, m *machine.Machines, uid, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.ShareDir(uid), "work", file))
	if err != nil {
		t.Fatalf("volume file %s: %v", file, err)
	}
	return string(b)
}

func writeVolume(t *testing.T, m *machine.Machines, uid, file, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(m.ShareDir(uid), "work", file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMoveMacUbuntuAtVolumes is the move-at-volumes walk on two fake hosts of different families, a Mac (VZ, declares
// VOLUMES) and an Ubuntu host (Cloud Hypervisor, declares MEMORY), sharing only herd-api and the snapshot store: an
// actor with two containers and a volume is created on the Mac, suspended at the fidelity the Mac declared, resumed
// on the Ubuntu host, moved back, and deleted with its VMs and shares verified gone on both. Its counterexample
// half: a volume that does not make the move fails the walk.
func TestMoveMacUbuntuAtVolumes(t *testing.T) {
	reg := images()
	var mac, ubuntu *machine.Machines
	e := conformance.Start(t,
		conformance.HerdletSpec{Name: "mac", Slots: 2, Labels: map[string]string{"host": "mac"}, Caps: macCaps, Configure: machines(t, reg, &mac)},
		conformance.HerdletSpec{Name: "ubuntu", Slots: 2, Labels: map[string]string{"host": "ubuntu"}, Caps: ubuntuCaps, Configure: machines(t, reg, &ubuntu)})
	ctx := ctxT(t)
	twoContainerTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES)
	a := mkActor(t, e, "a1", "tpl", map[string]string{"host": "mac"})
	uid := a.GetMetadata().GetUid()
	if a.GetStatus().GetAssignedNode() != "mac" {
		t.Fatalf("created on %q", a.GetStatus().GetAssignedNode())
	}
	writeVolume(t, mac, uid, "turn-1.md", "said on the mac")

	if s := suspend(t, e, "a1"); s.GetFidelity() != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES || s.GetLocality() != vmm.PortableRestoreFamily {
		t.Fatalf("suspended on the Mac at %v/%s, want VOLUMES/%s", s.GetFidelity(), s.GetLocality(), vmm.PortableRestoreFamily)
	}
	if _, err := os.Lstat(mac.ShareDir(uid)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the Mac kept the share after the release: %v", err)
	}
	moveTo(t, e, "a1", "ubuntu")
	if got := readVolume(t, ubuntu, uid, "turn-1.md"); got != "said on the mac" {
		t.Fatalf("volume on Ubuntu holds %q", got)
	}
	writeVolume(t, ubuntu, uid, "turn-2.md", "said on ubuntu")

	if s := suspend(t, e, "a1"); s.GetFidelity() != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES {
		t.Fatalf("suspended on Ubuntu at %v, want the template's VOLUMES", s.GetFidelity())
	}
	moveTo(t, e, "a1", "mac")
	if readVolume(t, mac, uid, "turn-1.md") != "said on the mac" || readVolume(t, mac, uid, "turn-2.md") != "said on ubuntu" {
		t.Fatal("the volume lost a turn on the way back")
	}

	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("DeleteActor: %v", err)
	}
	_, err := e.Client.GetActor(ctx, &ateapi.GetActorRequest{Actor: oref(e, "a1")})
	wantCode(t, "GetActor after delete", err, codes.NotFound)
	for name, m := range map[string]*machine.Machines{"mac": mac, "ubuntu": ubuntu} {
		if p := present(t, e.Herdlets[name], a); p != vmm.PresenceGone {
			t.Errorf("%s: VM %v after delete", name, p)
		}
		if _, err := os.Lstat(m.ShareDir(uid)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: share still there after delete: %v", name, err)
		}
	}
}

// TestMemorySnapshotDoesNotCrossFamilies is the other move-at-volumes counterexample: an actor the Ubuntu host
// suspended at MEMORY (what it declared) cannot be resumed on the Mac; herd-api refuses the placement, the Mac's
// herdlet is never asked, and the actor stays suspended and resumable at home.
func TestMemorySnapshotDoesNotCrossFamilies(t *testing.T) {
	reg := images()
	var mac, ubuntu *machine.Machines
	e := conformance.Start(t,
		conformance.HerdletSpec{Name: "mac", Slots: 2, Labels: map[string]string{"host": "mac"}, Caps: macCaps, Configure: machines(t, reg, &mac)},
		conformance.HerdletSpec{Name: "ubuntu", Slots: 2, Labels: map[string]string{"host": "ubuntu"}, Caps: ubuntuCaps, Configure: machines(t, reg, &ubuntu)})
	twoContainerTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	a := mkActor(t, e, "a1", "tpl", map[string]string{"host": "ubuntu"})
	writeVolume(t, ubuntu, a.GetMetadata().GetUid(), "notes", "kept")
	if s := suspend(t, e, "a1"); s.GetFidelity() != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY || s.GetLocality() != famA {
		t.Fatalf("suspended at %v/%s, want MEMORY/%s", s.GetFidelity(), s.GetLocality(), famA)
	}
	asked := false
	e.Herdlets["mac"].Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) {
		if w.Op == link.OpRestore || w.Op == link.OpBoot {
			asked = true
		}
		return o, true
	})
	cur, _ := getActor(t, e, "a1")
	cur.WorkerSelector = &ateapi.Selector{MatchLabels: map[string]string{"host": "mac"}}
	if _, err := e.Client.UpdateActor(ctxT(t), &ateapi.UpdateActorRequest{Actor: cur}); err != nil {
		t.Fatal(err)
	}
	_, err := e.Client.ResumeActor(ctxT(t), &ateapi.ResumeActorRequest{Actor: oref(e, "a1")})
	if err == nil {
		t.Fatal("a MEMORY snapshot of the Ubuntu family resumed on the Mac")
	}
	if asked {
		t.Fatal("the Mac's herdlet was asked to start a MEMORY snapshot of another family")
	}
	if got, _ := getActor(t, e, "a1"); got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("state %v after the refused move", got.GetStatus().GetState())
	}
	moveTo(t, e, "a1", "ubuntu")
	if got := readVolume(t, ubuntu, a.GetMetadata().GetUid(), "notes"); got != "kept" {
		t.Fatalf("volume after the MEMORY resume at home: %q", got)
	}
}

// TestWarmVMHandedToCreateActor: a herdlet declares one warm VM of the template; after the template's first boot
// taught it the spec, it keeps one warm VM, the next CreateActor takes it (no new boot), and the pool refills.
func TestWarmVMHandedToCreateActor(t *testing.T) {
	reg := images()
	e := conformance.Start(t)
	tpl := twoContainerTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	var m *machine.Machines
	var pool *machine.Pool
	e.AddHerdlet(t, conformance.HerdletSpec{Name: "ubuntu", Slots: 4, Caps: ubuntuCaps, Configure: func(a *herdlet.Agent) {
		machines(t, reg, &m)(a)
		pool = &machine.Pool{Machines: m, Health: machine.GuestHealth{VMM: a.VMM}, Interval: 20 * time.Millisecond}
		// Declared by count only: the spec comes with the template's first boot.
		pool.Set([]link.Template{{UID: tpl.GetMetadata().GetUid(), Warm: 1}})
		a.Pool = pool
	}})
	cold := mkActor(t, e, "a0", "tpl", nil)
	if share := m.ShareDir(cold.GetMetadata().GetUid()); filepath.Base(share) != cold.GetMetadata().GetUid() {
		t.Fatalf("the first actor got %s before the pool knew the template", share)
	}
	deadline := time.Now().Add(5 * time.Second)
	for pool.Counts()[tpl.GetMetadata().GetUid()] != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the pool never warmed a VM")
		}
		time.Sleep(10 * time.Millisecond)
	}
	a := mkActor(t, e, "a1", "tpl", nil)
	if p, _ := m.Inspect(context.Background(), a.GetMetadata().GetUid()); p != vmm.PresencePresent {
		t.Fatalf("the actor's VM is %v", p)
	}
	if share := m.ShareDir(a.GetMetadata().GetUid()); filepath.Base(share)[:5] != "warm-" {
		t.Fatalf("the actor got a new VM (%s), not the warm one", share)
	}
	for pool.Counts()[tpl.GetMetadata().GetUid()] != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the pool did not refill")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
