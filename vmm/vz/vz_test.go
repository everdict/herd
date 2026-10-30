package vz_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/vmmtest"
	"github.com/everdict/herd/vmm/vz"
	"github.com/everdict/herd/vmm/vz/vzfake"
)

// probeGuest is a probe configuration; the fake does not read the kernel.
var probeGuest = vz.ProbeConfig{Kernel: "/guest/Image", Initrd: "/guest/initramfs"}

func newVMM(t *testing.T, hv *vzfake.Hypervisor, cfg vz.Config) *vz.VMM {
	t.Helper()
	if cfg.StateDir == "" {
		cfg.StateDir = t.TempDir()
	}
	if cfg.Host == "" {
		cfg.Host = "mac-a"
	}
	if cfg.StopSettle == 0 {
		cfg.StopSettle = 50 * time.Millisecond
	}
	if cfg.Session == nil {
		cfg.Session = vzfake.UnlockedConsole()
	}
	v, err := vz.New(cfg, hv)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

// specIn returns a spec whose virtio-fs share is a directory under root.
func specIn(root string) func(id string) vmm.Spec {
	return func(id string) vmm.Spec {
		share := filepath.Join(root, "volumes", id)
		_ = os.MkdirAll(share, 0o700)
		return vmm.Spec{ID: id, Kernel: "/guest/Image", Initrd: "/guest/initramfs", VCPUs: 1, MemoryMiB: 512,
			Shares: []vmm.Share{{Tag: "herd", HostPath: share}}}
	}
}

func ctxT(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// faulty puts vmmtest's operation faults on the fake hypervisor's calls.
type faulty struct {
	*vz.VMM
	hv *vzfake.Hypervisor
}

func callFor(op vmm.Op) vzfake.Call {
	switch op {
	case vmm.OpDelete:
		return vzfake.CallStop
	case vmm.OpInspect:
		return vzfake.CallState
	case vmm.OpSnapshot:
		return vzfake.CallPause
	case vmm.OpRestore:
		return vzfake.CallRestore
	case vmm.OpCapabilities:
		return vzfake.CallValidate
	}
	return vzfake.CallStart
}

func (f faulty) Hang(op vmm.Op)        { f.hv.Hang(callFor(op)) }
func (f faulty) Unreachable(op vmm.Op) { f.hv.Unreachable(callFor(op)) }
func (f faulty) LeaveOnDelete()        { f.hv.LeaveRunningOnStop() }
func (f faulty) Clear()                { f.hv.Clear() }

var _ vmmtest.Faults = faulty{}

func contract(t *testing.T, probe vz.ProbeConfig) {
	root := t.TempDir()
	var snaps string
	vmmtest.Run(t, vmmtest.Harness{
		New: func(t *testing.T) vmm.VMM {
			snaps = t.TempDir()
			hv := vzfake.New()
			return faulty{newVMM(t, hv, vz.Config{Host: "mac-a", SnapshotDir: snaps, Probe: probe}), hv}
		},
		Elsewhere: func(t *testing.T) vmm.VMM {
			return newVMM(t, vzfake.New(), vz.Config{Host: "mac-b", SnapshotDir: snaps, Probe: probe})
		},
		Spec:    specIn(root),
		Timeout: 10 * time.Second,
	})
}

// A Mac with no probe guest, or whose probe did not pass, keeps the contract at VOLUMES.
func TestContractVolumesMac(t *testing.T) { contract(t, vz.ProbeConfig{}) }

// A Mac whose probe passed keeps the contract at MEMORY, and its save files restore only on it.
func TestContractMemoryMac(t *testing.T) { contract(t, probeGuest) }

func TestNewRejectsBadConfig(t *testing.T) {
	for name, cfg := range map[string]vz.Config{
		"no state dir": {Host: "mac-a"},
		"bad host":     {StateDir: t.TempDir(), Host: "Mac A"},
	} {
		if _, err := vz.New(cfg, vzfake.New()); !errors.Is(err, vmm.ErrInvalidSpec) {
			t.Errorf("%s: New = %v, want ErrInvalidSpec", name, err)
		}
	}
	if _, err := vz.New(vz.Config{StateDir: t.TempDir(), Host: "mac-a"}, nil); !errors.Is(err, vmm.ErrInvalidSpec) {
		t.Errorf("New without a hypervisor = %v, want ErrInvalidSpec", err)
	}
}

// Criterion vz-fidelity-declared: validateSaveRestoreSupport = true alone is not MEMORY. The R0 spike saw exactly
// this: validated, then saveMachineStateTo refused with "permission denied".
func TestFidelityValidateAloneIsNotMemory(t *testing.T) {
	hv := vzfake.New()
	hv.Fail(vzfake.CallSave, vzfake.ErrPermissionDenied)
	v := newVMM(t, hv, vz.Config{Probe: probeGuest})
	c, err := v.Capabilities(ctxT(t, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if c.SuspendMaxFidelity != vmm.FidelityVolumes || c.RestoreFamily != "" {
		t.Errorf("validated but unsavable Mac declared %v %q, want VOLUMES with no restore family", c.SuspendMaxFidelity, c.RestoreFamily)
	}
	r := v.Probe(ctxT(t, time.Second))
	if !r.Ran || !r.Validated || r.RoundTrip || !strings.Contains(r.Reason, "permission denied") {
		t.Errorf("probe = %+v, want validated, no round trip, reason permission denied", r)
	}
	spec := specIn(t.TempDir())("p1")
	if _, err := v.Boot(ctxT(t, 5*time.Second), spec); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Snapshot(ctxT(t, 5*time.Second), "p1", vmm.FidelityMemory); !errors.Is(err, vmm.ErrFidelityUnsupported) {
		t.Errorf("Snapshot(MEMORY) on a VOLUMES Mac = %v, want ErrFidelityUnsupported", err)
	}
}

// Criterion vz-fidelity-declared: a full round trip declares MEMORY in this Mac's own family.
func TestFidelityRoundTripDeclaresMemory(t *testing.T) {
	v := newVMM(t, vzfake.New(), vz.Config{Host: "nahyun-imac", Probe: probeGuest})
	c, err := v.Capabilities(ctxT(t, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if c.SuspendMaxFidelity != vmm.FidelityMemory || c.RestoreFamily != "microvm-vz-arm64-nahyun-imac" || c.Platform != vmm.PlatformVZ || c.Arch != "arm64" {
		t.Errorf("Capabilities = %+v, want MEMORY in microvm-vz-arm64-nahyun-imac", c)
	}
	if r := v.Probe(ctxT(t, time.Second)); !r.Passed() {
		t.Errorf("probe = %+v, want passed", r)
	}
}

// Criterion vz-fidelity-declared: every failed or unknown probe stays at VOLUMES.
func TestFidelityFailedOrUnknownProbeIsVolumes(t *testing.T) {
	cases := map[string]struct {
		probe vz.ProbeConfig
		fault func(hv *vzfake.Hypervisor)
	}{
		"no probe guest":         {vz.ProbeConfig{}, func(*vzfake.Hypervisor) {}},
		"validate false":         {probeGuest, func(hv *vzfake.Hypervisor) { hv.SetSaveRestoreValid(false) }},
		"validate unreachable":   {probeGuest, func(hv *vzfake.Hypervisor) { hv.Unreachable(vzfake.CallValidate) }},
		"start refused":          {probeGuest, func(hv *vzfake.Hypervisor) { hv.Fail(vzfake.CallStart, errors.New("refused")) }},
		"restore refused":        {probeGuest, func(hv *vzfake.Hypervisor) { hv.Fail(vzfake.CallRestore, vzfake.ErrPermissionDenied) }},
		"save hangs (unknown)":   {probeGuest, func(hv *vzfake.Hypervisor) { hv.Hang(vzfake.CallSave) }},
		"resume hangs (unknown)": {probeGuest, func(hv *vzfake.Hypervisor) { hv.Hang(vzfake.CallResume) }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			hv := vzfake.New()
			t.Cleanup(hv.Clear)
			tc.fault(hv)
			v := newVMM(t, hv, vz.Config{Probe: tc.probe, ProbeTimeout: 200 * time.Millisecond})
			c, err := v.Capabilities(ctxT(t, 5*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if c.SuspendMaxFidelity != vmm.FidelityVolumes || c.RestoreFamily != "" {
				t.Errorf("declared %v %q, want VOLUMES", c.SuspendMaxFidelity, c.RestoreFamily)
			}
			if r := v.Probe(ctxT(t, time.Second)); r.Passed() || r.Reason == "" {
				t.Errorf("probe = %+v, want not passed with a reason", r)
			}
		})
	}
}

// Criterion vz-backend: Boot answers only once the guest answers on its vsock port.
func TestBootWaitsForGuestVsock(t *testing.T) {
	hv := vzfake.New()
	hv.SetGuestDelay(150 * time.Millisecond)
	v := newVMM(t, hv, vz.Config{})
	if _, err := v.Boot(ctxT(t, 5*time.Second), specIn(t.TempDir())("g1")); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	returned := time.Now()
	ms := hv.Machines()
	if len(ms) != 1 {
		t.Fatalf("%d machines, want 1", len(ms))
	}
	answered := ms[0].Answered()
	if answered.IsZero() || answered.After(returned) {
		t.Errorf("Boot returned at %v, guest answered on vsock at %v: Boot returned before the guest answered", returned, answered)
	}
	cfg := ms[0].Config()
	if len(cfg.Shares) != 1 || cfg.Shares[0].Tag != "herd" || cfg.Console == "" {
		t.Errorf("machine config %+v, want the herd virtio-fs share and a console", cfg)
	}
}

// Criterion vz-backend: a guest that never answers is not booted, and the VM is cleaned up verifiably.
func TestBootFailsWhenGuestNeverAnswers(t *testing.T) {
	hv := vzfake.New()
	hv.SetGuestSilent(true)
	// The context ends only once the machine runs and its guest has refused a connection, so the silent-guest wait
	// is the only thing Boot can fail on, however slowly the box builds and starts the machine.
	ctx, cancel := context.WithCancel(ctxT(t, 10*time.Second))
	var refused atomic.Bool
	hv.OnGuestRefused(func(*vzfake.Machine) {
		refused.Store(true)
		cancel()
	})
	v := newVMM(t, hv, vz.Config{})
	_, err := v.Boot(ctx, specIn(t.TempDir())("g2"))
	if !refused.Load() {
		t.Fatalf("Boot = %v before the guest was ever asked on vsock", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Boot with a silent guest = %v, want context.Canceled", err)
	}
	if p, err := v.Inspect(ctxT(t, time.Second), "g2"); p != vmm.PresenceGone {
		t.Errorf("after the failed Boot g2 is %v (%v), want gone", p, err)
	}
	for i, m := range hv.Machines() {
		if st := m.CurrentState(); st != vz.MachineStopped {
			t.Errorf("the failed Boot left machine %d %s", i, st)
		}
	}
}

// Criterion vz-backend: a Virtualization.framework call that hangs is unknown, never present or gone.
func TestHungCallIsUnknown(t *testing.T) {
	hv := vzfake.New()
	t.Cleanup(hv.Clear)
	v := newVMM(t, hv, vz.Config{})
	if _, err := v.Boot(ctxT(t, 5*time.Second), specIn(t.TempDir())("h1")); err != nil {
		t.Fatal(err)
	}
	hv.Hang(vzfake.CallState)
	p, err := v.Inspect(ctxT(t, 50*time.Millisecond), "h1")
	if p != vmm.PresenceUnknown || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Inspect while the state call hangs = %v (%v), want unknown with context.DeadlineExceeded", p, err)
	}
	p, err = v.Delete(ctxT(t, 50*time.Millisecond), "h1")
	if p != vmm.PresenceUnknown || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Delete while the state call hangs = %v (%v), want unknown with context.DeadlineExceeded", p, err)
	}
	hv.Clear()
	hv.Hang(vzfake.CallStop)
	p, err = v.Delete(ctxT(t, 50*time.Millisecond), "h1")
	if p != vmm.PresenceUnknown || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Delete while stop hangs = %v (%v), want unknown with context.DeadlineExceeded", p, err)
	}
	hv.Clear()
	if p, err := v.Inspect(ctxT(t, time.Second), "h1"); p != vmm.PresencePresent {
		t.Errorf("h1 once the hypervisor answers = %v (%v), want present", p, err)
	}
}

// Criterion vz-verified-delete: a VM still running after the stop is present with ErrDeleteNotVerified, and its
// files stay.
func TestDeleteNotGoneWhileRunning(t *testing.T) {
	hv := vzfake.New()
	t.Cleanup(hv.Clear)
	state := t.TempDir()
	v := newVMM(t, hv, vz.Config{StateDir: state})
	if _, err := v.Boot(ctxT(t, 5*time.Second), specIn(t.TempDir())("d1")); err != nil {
		t.Fatal(err)
	}
	hv.LeaveRunningOnStop()
	p, err := v.Delete(ctxT(t, 5*time.Second), "d1")
	if p != vmm.PresencePresent || !errors.Is(err, vmm.ErrDeleteNotVerified) {
		t.Errorf("Delete of a VM still running = %v (%v), want present with ErrDeleteNotVerified", p, err)
	}
	if _, err := os.Stat(filepath.Join(state, "vms", "d1")); err != nil {
		t.Errorf("the files of a running VM were removed: %v", err)
	}
	if p, err := v.Inspect(ctxT(t, time.Second), "d1"); p != vmm.PresencePresent {
		t.Errorf("Inspect after the unverified Delete = %v (%v), want present", p, err)
	}
	hv.Clear()
	if p, err := v.Delete(ctxT(t, 5*time.Second), "d1"); p != vmm.PresenceGone || err != nil {
		t.Errorf("Delete once the stop takes = %v (%v), want gone", p, err)
	}
	if _, err := os.Stat(filepath.Join(state, "vms", "d1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("gone, but the files are still there: %v", err)
	}
}

// Criterion vz-verified-delete: a stopped VM whose files cannot be removed is not gone.
func TestDeleteNotGoneWhileFilesRemain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes read-only directories")
	}
	hv := vzfake.New()
	state := t.TempDir()
	v := newVMM(t, hv, vz.Config{StateDir: state})
	if _, err := v.Boot(ctxT(t, 5*time.Second), specIn(t.TempDir())("d2")); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(state, "vms", "d2", "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	p, err := v.Delete(ctxT(t, 5*time.Second), "d2")
	if p != vmm.PresencePresent || !errors.Is(err, vmm.ErrDeleteNotVerified) {
		t.Errorf("Delete whose files remain = %v (%v), want present with ErrDeleteNotVerified", p, err)
	}
	if st := hv.Machines()[0].CurrentState(); st != vz.MachineStopped {
		t.Errorf("machine %s, want stopped", st)
	}
	if err := os.Chmod(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if p, err := v.Delete(ctxT(t, 5*time.Second), "d2"); p != vmm.PresenceGone || err != nil {
		t.Errorf("Delete once the files can go = %v (%v), want gone", p, err)
	}
}

// Criterion vz-backend: VOLUMES pauses the VM, flushes the share, records the snapshot durably and resumes.
func TestSnapshotVolumesRecordsAndResumes(t *testing.T) {
	hv := vzfake.New()
	v := newVMM(t, hv, vz.Config{})
	spec := specIn(t.TempDir())("v1")
	if err := os.WriteFile(filepath.Join(spec.Shares[0].HostPath, "data"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Boot(ctxT(t, 5*time.Second), spec); err != nil {
		t.Fatal(err)
	}
	s, err := v.Snapshot(ctxT(t, 5*time.Second), "v1", vmm.FidelityVolumes)
	if err != nil {
		t.Fatal(err)
	}
	if s.Fidelity != vmm.FidelityVolumes || s.RestoreFamily != vmm.PortableRestoreFamily || s.VMID != "v1" {
		t.Errorf("snapshot %+v", s)
	}
	if _, err := os.Stat(filepath.Join(s.Location, "snapshot.json")); err != nil {
		t.Errorf("snapshot record: %v", err)
	}
	if st := hv.Machines()[0].CurrentState(); st != vz.MachineRunning {
		t.Errorf("after Snapshot the VM is %s, want running", st)
	}
}

// A snapshot that fails part-way leaves no snapshot behind and the VM running.
func TestSnapshotFailureLeavesNothing(t *testing.T) {
	hv := vzfake.New()
	snaps := t.TempDir()
	v := newVMM(t, hv, vz.Config{SnapshotDir: snaps, Probe: probeGuest})
	if _, err := v.Capabilities(ctxT(t, 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Boot(ctxT(t, 5*time.Second), specIn(t.TempDir())("m1")); err != nil {
		t.Fatal(err)
	}
	hv.Fail(vzfake.CallSave, errors.New("disk full"))
	if s, err := v.Snapshot(ctxT(t, 5*time.Second), "m1", vmm.FidelityMemory); err == nil || s != (vmm.Snapshot{}) {
		t.Errorf("failed MEMORY snapshot = %+v (%v), want no snapshot and an error", s, err)
	}
	if ents, _ := os.ReadDir(snaps); len(ents) != 0 {
		t.Errorf("a failed snapshot left %d entries in the snapshot directory", len(ents))
	}
	ms := hv.Machines()
	if st := ms[len(ms)-1].CurrentState(); st != vz.MachineRunning {
		t.Errorf("after the failed snapshot the VM is %s, want running", st)
	}
}

// PLT-199: TestReal's MEMORY sequence on the fake (boot with a share, the guest's file lands in the share, MEMORY
// snapshot, verified delete, restore). The restored machine is built with the share of the snapshot-time spec, and
// that directory still holds the file: the host side re-attaches the tree as it was, so a restored guest that cannot
// read the file is not explained by the share's host directory.
func TestMemoryRestoreAttachesSnapshotTimeShare(t *testing.T) {
	hv := vzfake.New()
	v := newVMM(t, hv, vz.Config{Probe: probeGuest})
	spec := specIn(t.TempDir())("real1")
	share := spec.Shares[0].HostPath
	if _, err := v.Boot(ctxT(t, 5*time.Second), spec); err != nil {
		t.Fatal(err)
	}
	// What the guest's write through virtio-fs leaves on the host.
	if err := os.WriteFile(filepath.Join(share, "out.txt"), []byte("from-guest"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := v.Snapshot(ctxT(t, 5*time.Second), "real1", vmm.FidelityMemory)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := v.Delete(ctxT(t, 5*time.Second), "real1"); p != vmm.PresenceGone || err != nil {
		t.Fatalf("Delete = %v (%v)", p, err)
	}
	if _, err := v.Restore(ctxT(t, 5*time.Second), s, spec); err != nil {
		t.Fatal(err)
	}
	ms := hv.Machines()
	got := ms[len(ms)-1].Config().Shares
	if len(got) != 1 || got[0] != spec.Shares[0] {
		t.Fatalf("restored machine's shares = %+v, want the snapshot-time %+v", got, spec.Shares)
	}
	if b, err := os.ReadFile(filepath.Join(got[0].HostPath, "out.txt")); err != nil || string(b) != "from-guest" {
		t.Errorf("the restored machine's share holds out.txt = %q (%v), want from-guest", b, err)
	}
}
