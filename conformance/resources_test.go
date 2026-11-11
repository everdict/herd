package conformance_test

import (
	"strings"
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/herdlet"
)

// sizedHerdlet reports 8 cores and 15 GiB it can allocate, VMs of 1 vCPU / 512 MiB by default and 512 MiB overhead,
// and no slot ceiling (PLT-452).
func sizedHerdlet(name string) conformance.HerdletSpec {
	return conformance.HerdletSpec{Name: name, Caps: chCaps(famA), Configure: func(a *herdlet.Agent) {
		a.MilliCPU, a.MemoryMiB, a.OverheadMiB = 8000, 15*1024, 512
	}}
}

func mkSizedTemplate(t *testing.T, e *conformance.Env, name string, milliCPU, memoryMiB int64) {
	t.Helper()
	_, err := e.Client.CreateActorTemplate(ctxT(t), &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata:       &ateapi.ResourceMetadata{Atespace: e.Space, Name: name},
		Containers:     []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
		Resources:      &ateapi.ResourceRequirements{Limits: &ateapi.Resources{CpuMillicores: milliCPU, MemoryBytes: memoryMiB << 20}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES, GoldenSnapshotConfig: coldBoot},
	}})
	if err != nil {
		t.Fatalf("CreateActorTemplate %s: %v", name, err)
	}
}

// TestPlacementByResources (PLT-452): herd-api places an actor by its template's cpu and memory against what each
// herdlet reported, spreads actors over the workers, refuses one no worker has room for naming the axis, and
// ListWorkers answers each worker's cpu and memory capacity and what its actors are sized at.
func TestPlacementByResources(t *testing.T) {
	e := conformance.Start(t, sizedHerdlet("h1"), sizedHerdlet("h2"))
	ctx := ctxT(t)
	// 2 cpu / 4 GiB: each VM takes 2000m and 4096+512 MiB, so a 15 GiB worker holds 3 by memory.
	mkSizedTemplate(t, e, "box", 2000, 4096)

	placed := map[string]int{}
	for _, n := range []string{"a1", "a2", "a3", "a4", "a5", "a6"} {
		a := mkActor(t, e, n, "box", nil)
		placed[a.GetStatus().GetWorkerAssignment().GetWorker().GetName()]++
		if n == "a2" && (placed["h1"] != 1 || placed["h2"] != 1) {
			t.Fatalf("two actors back to back: %v, want one on each worker", placed)
		}
	}
	if placed["h1"] != 3 || placed["h2"] != 3 {
		t.Fatalf("six 2cpu/4GiB actors over two 15 GiB workers: %v, want 3 and 3", placed)
	}
	// A seventh does not fit anywhere: 15360 - 3*4608 = 1536 MiB left on each.
	_, err := e.Client.CreateActor(ctx, actorReq(e, "a7", "box", nil))
	if err == nil || !strings.Contains(err.Error(), "memory") {
		t.Fatalf("seventh actor: %v, want a refusal naming memory", err)
	}

	resp := &ateapi.ListWorkersResponse{}
	if err := e.Client.Raw(ctx, "ListWorkers", &ateapi.ListWorkersRequest{}, resp); err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if len(resp.GetWorkers()) != 2 {
		t.Fatalf("ListWorkers = %v", resp)
	}
	for _, w := range resp.GetWorkers() {
		c, u := w.GetStatus().GetCapacity(), w.GetStatus().GetAllocated()
		if c.GetCpuMillicores() != 8000 || c.GetMemoryBytes() != 15<<30 || c.GetActors() != 0 {
			t.Errorf("%s capacity = %v, want 8000m, 15 GiB, no slot ceiling", w.GetMetadata().GetName(), c)
		}
		if u.GetCpuMillicores() != 6000 || u.GetMemoryBytes() != 3*4608<<20 || u.GetActors() != 3 {
			t.Errorf("%s allocated = %v, want 6000m, %d bytes, 3 actors", w.GetMetadata().GetName(), u, int64(3*4608)<<20)
		}
	}
}
