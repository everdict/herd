package conformance_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/herdlet"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm/vz"
	"github.com/everdict/herd/vmm/vz/vzfake"
)

func workerRecord(t *testing.T, e *conformance.Env, name string) *herdext.WorkerCapabilities {
	t.Helper()
	c := &herdext.WorkerCapabilities{}
	if err := e.Client.Conn().Invoke(ctxT(t), "/herd.ext.v1.Capabilities/GetWorkerCapabilities", &herdext.GetWorkerCapabilitiesRequest{Worker: name}, c); err != nil {
		t.Fatalf("GetWorkerCapabilities: %v", err)
	}
	return c
}

// waitRecord waits until herd-api's worker record declares f.
func waitRecord(t *testing.T, e *conformance.Env, name string, f ateapi.SnapshotFidelity) *herdext.WorkerCapabilities {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c := workerRecord(t, e, name)
		if c.GetSuspendMaxFidelity() == f {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker %s still declares %v (reason %q), want %v", name, c.GetSuspendMaxFidelity(), c.GetReason(), f)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMacLockStateReachesTheWorkerRecord (criterion lock-state): a Mac herdlet (the vz backend on the fake
// hypervisor, a fake console) whose probe passed is MEMORY in herd-api's worker record while its console is
// unlocked. When the screen locks, the record turns VOLUMES with the reason and a suspend of a MEMORY template is
// taken at VOLUMES; unlocked, it is MEMORY again; after a wake on which the probe fails, it is VOLUMES. A herdlet
// that only declares at registration leaves a locked Mac advertised as MEMORY: red.
func TestMacLockStateReachesTheWorkerRecord(t *testing.T) {
	hv := vzfake.New()
	con := vzfake.UnlockedConsole()
	mac, err := vz.New(vz.Config{StateDir: t.TempDir(), Host: "nahyun-imac", Session: con, StopSettle: 50 * time.Millisecond,
		Probe: vz.ProbeConfig{Kernel: "/guest/Image", Initrd: "/guest/initramfs"}}, hv)
	if err != nil {
		t.Fatal(err)
	}
	wake := make(chan struct{}, 1)
	var mu sync.Mutex
	assigned := map[string]link.ActorRef{}
	e := conformance.Start(t, conformance.HerdletSpec{Name: "mac", Slots: 2, Backend: mac,
		Configure: func(a *herdlet.Agent) {
			a.Recheck, a.Wake = 20*time.Millisecond, wake
			a.OnActor = func(vm string, r link.ActorRef) { mu.Lock(); defer mu.Unlock(); assigned[vm] = r }
		}})
	ctx := ctxT(t)

	c := workerRecord(t, e, "mac")
	if c.GetSuspendMaxFidelity() != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY || c.GetRestoreFamily() != "microvm-vz-arm64-nahyun-imac" ||
		c.GetPlatform() != herdext.Platform_PLATFORM_DARWIN_VZ {
		t.Fatalf("unlocked Mac registered %v", c)
	}
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	a1 := mkActor(t, e, "a1", "tpl", nil)
	mu.Lock()
	if got := assigned[a1.GetMetadata().GetUid()]; got.Space != e.Space || got.Name != "a1" || got.UID != a1.GetMetadata().GetUid() {
		t.Errorf("herdlet was told VM %s is %+v, want %s/a1 (the macOS egress binding needs it); told %v", a1.GetMetadata().GetUid(), got, e.Space, assigned)
	}
	mu.Unlock()

	con.Lock(true)
	c = waitRecord(t, e, "mac", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES)
	if c.GetRestoreFamily() != "" || !strings.Contains(c.GetReason(), "locked") {
		t.Errorf("locked Mac's record: %v, want no restore family and the reason", c)
	}
	s, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a1")})
	if err != nil {
		t.Fatalf("SuspendActor on a locked Mac: %v", err)
	}
	snaps := s.GetActor().GetStatus().GetSnapshots()
	if len(snaps) == 0 || snaps[len(snaps)-1].GetFidelity() != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES {
		t.Errorf("suspend on a locked Mac kept %v, want VOLUMES", snaps)
	}
	if _, err := e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("ResumeActor: %v", err)
	}

	con.Lock(false)
	waitRecord(t, e, "mac", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)

	// The Mac sleeps and wakes; this time the framework refuses to save.
	hv.Fail(vzfake.CallSave, vzfake.ErrPermissionDenied)
	wake <- struct{}{}
	c = waitRecord(t, e, "mac", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES)
	if !strings.Contains(c.GetReason(), "permission denied") {
		t.Errorf("after the failed probe the record says %q, want the probe's reason", c.GetReason())
	}
}
