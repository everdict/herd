package conformance_test

import (
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc/codes"
)

func compat(w *ateapi.Worker) map[string]string {
	out := map[string]string{}
	for _, a := range w.GetStatus().GetSandboxClasses()[0].GetDefaultRuntime().GetVersion().GetAttributes() {
		out[a.GetKey()] = a.GetValue()
	}
	return out
}

// TestListWorkers (PLT-219): Control.ListWorkers answers Substrate's Worker for every worker herd-api has a record of:
// its name, the sandbox class it runs, the fidelity and restore family it declared (as the class's runtime compat
// attributes), its labels, its capacity and what herd-api holds on it. A worker whose link is down stays listed,
// UNAVAILABLE. It pages like ListActors, and only a caller with a role in some atespace may call it.
func TestListWorkers(t *testing.T) {
	mac := vmm.Capabilities{Platform: vmm.PlatformVZ, Arch: "arm64", SuspendMaxFidelity: vmm.FidelityVolumes}
	e := conformance.Start(t,
		conformance.HerdletSpec{Name: "h1", Slots: 2, Labels: map[string]string{"pool": "ubuntu"}, Caps: chCaps(famA)},
		conformance.HerdletSpec{Name: "m1", Slots: 3, Labels: map[string]string{"pool": "mac"}, Caps: mac})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", map[string]string{"pool": "ubuntu"})

	ro := conformance.NewClient(e.Addr, e.Token(t, map[string]auth.Role{e.Space: auth.RoleRead}, false))
	defer ro.Close()
	list := func(c *conformance.Client, opts *ateapi.ListOptions) (*ateapi.ListWorkersResponse, error) {
		resp := &ateapi.ListWorkersResponse{}
		return resp, c.Raw(ctx, "ListWorkers", &ateapi.ListWorkersRequest{Options: opts}, resp)
	}
	resp, err := list(ro, nil)
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if len(resp.GetWorkers()) != 2 || resp.GetNextPageToken() != "" {
		t.Fatalf("ListWorkers = %v, want h1 and m1 on one page", resp)
	}
	want := []struct {
		name, pool, fidelity, family, platform, arch string
		slots, used                                  int32
	}{
		{"h1", "ubuntu", "SNAPSHOT_FIDELITY_MEMORY", famA, string(vmm.PlatformCloudHypervisor), "amd64", 2, 1},
		{"m1", "mac", "SNAPSHOT_FIDELITY_VOLUMES", "", string(vmm.PlatformVZ), "arm64", 3, 0},
	}
	for i, w := range resp.GetWorkers() {
		x := want[i]
		md := w.GetMetadata()
		if md.GetName() != x.name || md.GetAtespace() != "" || md.GetUid() == "" || md.GetVersion() == 0 {
			t.Errorf("worker %d metadata = %v, want global %s with a uid and version", i, md, x.name)
			continue
		}
		if w.GetLabels()["pool"] != x.pool {
			t.Errorf("%s labels = %v", x.name, w.GetLabels())
		}
		st := w.GetStatus()
		if st.GetState() != ateapi.WorkerState_WORKER_STATE_ACTIVE {
			t.Errorf("%s state = %v, want ACTIVE", x.name, st.GetState())
		}
		if st.GetCapacity().GetActors() != x.slots || st.GetAllocated().GetActors() != x.used {
			t.Errorf("%s capacity %v allocated %v, want %d actors with %d held", x.name, st.GetCapacity(), st.GetAllocated(), x.slots, x.used)
		}
		if len(st.GetSandboxClasses()) != 1 || st.GetSandboxClasses()[0].GetSandboxClass() != "microvm" {
			t.Errorf("%s sandbox classes = %v, want one: microvm", x.name, st.GetSandboxClasses())
			continue
		}
		v := st.GetSandboxClasses()[0].GetDefaultRuntime().GetVersion()
		attrs := compat(w)
		if v.GetSchemaVersion() == "" || attrs["suspend_max_fidelity"] != x.fidelity || attrs["restore_family"] != x.family ||
			attrs["platform"] != x.platform || attrs["architecture"] != x.arch || attrs["portable_restore_family"] != vmm.PortableRestoreFamily {
			t.Errorf("%s runtime version = %v", x.name, v)
		}
	}

	// Pages of one, by name.
	p1, err := list(ro, &ateapi.ListOptions{PageSize: 1})
	if err != nil || len(p1.GetWorkers()) != 1 || p1.GetWorkers()[0].GetMetadata().GetName() != "h1" || p1.GetNextPageToken() == "" {
		t.Fatalf("first page = %v, %v", p1, err)
	}
	p2, err := list(ro, &ateapi.ListOptions{PageSize: 1, PageToken: p1.GetNextPageToken()})
	if err != nil || len(p2.GetWorkers()) != 1 || p2.GetWorkers()[0].GetMetadata().GetName() != "m1" || p2.GetNextPageToken() != "" {
		t.Errorf("second page = %v, %v", p2, err)
	}

	// The record outlives the link: a worker that is not connected is listed UNAVAILABLE with what it declared.
	e.Herdlets["m1"].Disconnect()
	resp, err = list(ro, nil)
	if err != nil || len(resp.GetWorkers()) != 2 {
		t.Fatalf("ListWorkers after m1 left = %v, %v", resp, err)
	}
	if m := resp.GetWorkers()[1]; m.GetStatus().GetState() != ateapi.WorkerState_WORKER_STATE_UNAVAILABLE ||
		compat(m)["suspend_max_fidelity"] != "SNAPSHOT_FIDELITY_VOLUMES" {
		t.Errorf("m1 after its link closed = %v, want UNAVAILABLE at VOLUMES", m)
	}

	// The Control API's authentication: no token, a herdlet credential, a token with no role in any atespace.
	for _, c := range []struct {
		what, token string
		code        codes.Code
	}{
		{"no token", "", codes.Unauthenticated},
		{"a herdlet credential", e.TokenFor(t, "h1", nil, true), codes.PermissionDenied},
		{"a token with no atespace", e.Token(t, nil, false), codes.PermissionDenied},
	} {
		cl := conformance.NewClient(e.Addr, c.token)
		_, err := list(cl, nil)
		cl.Close()
		wantCode(t, "ListWorkers with "+c.what, err, c.code)
	}
}
