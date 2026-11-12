package conformance_test

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/egress/nft/nfttest"
	"github.com/everdict/herd/egress/tapnet"
	"github.com/everdict/herd/herdlet"
	"github.com/everdict/herd/herdlet/machine"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// restoredHeader is the response header (and trailer) herd answers the fidelity a start really restored at in.
const restoredHeader = "herd-restored-fidelity"

// netMachines gives a herdlet Machines whose VMs get TAP links (tapnet over a fake ip(8)/nft(8)), so a MEMORY
// restore keeps its snapshot's link and cannot have it while another VM on the host holds it.
func netMachines(t *testing.T, reg *imagetest.Registry) func(*herdlet.Agent) {
	return func(a *herdlet.Agent) {
		caps, err := a.VMM.Capabilities(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		n := &tapnet.Network{Leases: &tapnet.Leases{Path: filepath.Join(dir, "leases.json"), Pool: netip.MustParsePrefix("10.88.0.0/16")},
			Bindings: &tapnet.Bindings{Path: filepath.Join(dir, "egress-bindings.json")}, Host: &nfttest.Host{}, ProxyPort: 15001, DNSPort: 15053, TAPOwner: -1}
		m, err := machine.New(machine.Config{VMM: a.VMM, ShareDir: filepath.Join(dir, "shares"), Base: vmm.Spec{Kernel: "vmlinux"}, Network: n,
			Images: &image.Cache{Dir: filepath.Join(dir, "images"), Source: reg, Builder: &imagetest.Builder{},
				Platform: image.Platform{OS: "linux", Architecture: caps.Arch}}})
		if err != nil {
			t.Fatal(err)
		}
		a.Machines = m
	}
}

// restored is the herd-restored-fidelity value of an answer's header and trailer ("" when neither carries it).
func restored(t *testing.T, what string, header, trailer metadata.MD) string {
	t.Helper()
	h, tr := header.Get(restoredHeader), trailer.Get(restoredHeader)
	if len(h) > 0 && len(tr) > 0 && h[0] != tr[0] {
		t.Errorf("%s: header says %s, trailer %s", what, h[0], tr[0])
	}
	if len(h) > 0 {
		return h[0]
	}
	if len(tr) > 0 {
		return tr[0]
	}
	return ""
}

func resumeRestored(t *testing.T, e *conformance.Env, name string) string {
	t.Helper()
	var h, tr metadata.MD
	r := &ateapi.ResumeActorResponse{}
	if err := e.Client.Raw(ctxT(t), "ResumeActor", &ateapi.ResumeActorRequest{Actor: oref(e, name)}, r, grpc.Header(&h), grpc.Trailer(&tr)); err != nil {
		t.Fatalf("ResumeActor %s: %v", name, err)
	}
	return restored(t, "ResumeActor "+name, h, tr)
}

func forkRestored(t *testing.T, e *conformance.Env, name, tpl, tag string) string {
	t.Helper()
	var h, tr metadata.MD
	req := actorReq(e, name, tpl, nil)
	req.Actor.SourceTag = oref(e, tag)
	if err := e.Client.Raw(ctxT(t), "CreateActor", req, &ateapi.Actor{}, grpc.Header(&h), grpc.Trailer(&tr)); err != nil {
		t.Fatalf("CreateActor %s from tag %s: %v", name, tag, err)
	}
	return restored(t, "CreateActor "+name, h, tr)
}

func mkTag(t *testing.T, e *conformance.Env, name, actor string) {
	t.Helper()
	if _, err := e.Client.CreateTag(ctxT(t), &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: name}, SourceActor: oref(e, actor),
	}}); err != nil {
		t.Fatalf("CreateTag %s: %v", name, err)
	}
}

var (
	fidelityMemory  = ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY.String()
	fidelityVolumes = ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES.String()
)

// TestResumeAnswersRestoredFidelity (PLT-496, I10): a ResumeActor and a CreateActor that restores from a tag answer, in
// the herd-restored-fidelity header, the fidelity the herdlet really restored: MEMORY when the memory resumed, VOLUMES
// when the herdlet cold-booted from the bundle's volume tree because the memory's link is another VM's on the host
// (machine's errLinkTaken path). Before PLT-496 neither answer carries the header, and a cold boot looks like a
// memory resume to the caller.
func TestResumeAnswersRestoredFidelity(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4, Caps: ubuntuCaps, Configure: netMachines(t, images())})
	twoContainerTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)

	// A memory suspend and resume on the actor's own host, its link free: the memory resumes.
	if s := suspend(t, e, "a1"); s.GetFidelity() != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
		t.Fatalf("suspended at %v, want MEMORY", s.GetFidelity())
	}
	if got := resumeRestored(t, e, "a1"); got != fidelityMemory {
		t.Errorf("ResumeActor of a memory-suspended actor restored at memory answers %s %q, want %s", restoredHeader, got, fidelityMemory)
	}

	// A fork of a MEMORY tag while its source runs here: the source holds the link, so the fork cold-boots.
	mkTag(t, e, "t1", "a1")
	if got := forkRestored(t, e, "fork-1", "tpl", "t1"); got != fidelityVolumes {
		t.Errorf("CreateActor from a MEMORY tag whose link the source holds answers %s %q, want %s (a cold boot)", restoredHeader, got, fidelityVolumes)
	}

	// The source suspended (its link released), a fork of a1's next MEMORY tag takes the link with the memory; a1's
	// resume then finds its link taken and cold-boots from its bundle.
	suspend(t, e, "a1")
	mkTag(t, e, "t2", "a1")
	if got := forkRestored(t, e, "fork-2", "tpl", "t2"); got != fidelityMemory {
		t.Errorf("CreateActor from a MEMORY tag on a free link answers %s %q, want %s", restoredHeader, got, fidelityMemory)
	}
	if got := resumeRestored(t, e, "a1"); got != fidelityVolumes {
		t.Errorf("ResumeActor whose memory's link fork-2 holds (a cold boot from the bundle) answers %s %q, want %s", restoredHeader, got, fidelityVolumes)
	}
	// GetActor answers what the actor's running VM was restored at too.
	var h, tr metadata.MD
	if err := e.Client.Raw(ctxT(t), "GetActor", &ateapi.GetActorRequest{Actor: oref(e, "a1")}, &ateapi.Actor{}, grpc.Header(&h), grpc.Trailer(&tr)); err != nil {
		t.Fatal(err)
	}
	if got := restored(t, "GetActor a1", h, tr); got != fidelityVolumes {
		t.Errorf("GetActor after a cold-boot resume answers %s %q, want %s", restoredHeader, got, fidelityVolumes)
	}
}

// TestResumeOwedAnswersRestoredFidelityOnGetActor (PLT-496): a resume whose restore herd-api did not hear the end of
// (the herdlet's answer lost: owed) learns the fidelity when the resume is settled, and from then GetActor answers it.
func TestResumeOwedAnswersRestoredFidelityOnGetActor(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4, Caps: ubuntuCaps, Configure: netMachines(t, images())})
	twoContainerTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	suspend(t, e, "a1")
	h := e.Herdlets["h1"]
	h.Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) { return o, w.Op != link.OpRestore })
	if _, err := e.Client.ResumeActor(ctxT(t), &ateapi.ResumeActorRequest{Actor: oref(e, "a1")}); err == nil {
		t.Fatal("ResumeActor whose restore answer is lost answered done")
	}
	h.Rewrite(nil)
	// The retried resume finds the VM there: the herdlet answers what it restored the VM at.
	if got := resumeRestored(t, e, "a1"); got != fidelityMemory {
		t.Errorf("settled ResumeActor answers %s %q, want %s", restoredHeader, got, fidelityMemory)
	}
	var hd, tr metadata.MD
	if err := e.Client.Raw(ctxT(t), "GetActor", &ateapi.GetActorRequest{Actor: oref(e, "a1")}, &ateapi.Actor{}, grpc.Header(&hd), grpc.Trailer(&tr)); err != nil {
		t.Fatal(err)
	}
	if got := restored(t, "GetActor a1", hd, tr); got != fidelityMemory {
		t.Errorf("GetActor after the settled resume answers %s %q, want %s", restoredHeader, got, fidelityMemory)
	}
}

// TestRestoredFidelityIsTheHerdletsAnswer (PLT-496): herd-api answers the fidelity the herdlet said it restored, in
// Substrate's names: a herdlet that answers ROOTFS gives ROOTFS (herd itself takes no ROOTFS snapshot), and one that
// answers none (a herdlet older than PLT-496) gives no header, which the caller reads as VOLUMES.
func TestRestoredFidelityIsTheHerdletsAnswer(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4, Caps: ubuntuCaps})
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	mkActor(t, e, "a2", "tpl", nil)
	suspend(t, e, "a1")
	suspend(t, e, "a2")
	h := e.Herdlets["h1"]
	h.Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) {
		if w.Op == link.OpRestore && o.Result == link.ResultDone {
			o.Restored = 2 // SNAPSHOT_FIDELITY_ROOTFS
		}
		return o, true
	})
	if got := resumeRestored(t, e, "a1"); got != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS.String() {
		t.Errorf("ResumeActor of a ROOTFS restore answers %s %q, want SNAPSHOT_FIDELITY_ROOTFS", restoredHeader, got)
	}
	h.Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) {
		if w.Op == link.OpRestore {
			o.Restored = 0
		}
		return o, true
	})
	if got := resumeRestored(t, e, "a2"); got != "" {
		t.Errorf("ResumeActor whose herdlet answered no fidelity answers %s %q, want none", restoredHeader, got)
	}
}
