// Package vmmtest is the behavioural contract every vmm.VMM implementation must keep. The in-memory fake runs it in
// every `go test`; the Cloud Hypervisor and Virtualization.framework backends run it on real hosts.
package vmmtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/everdict/herd/vmm"
)

// Faults is implemented by backends that can be made to misbehave on purpose (the fake). The contract uses it to show
// that an operation that does not answer is reported as unknown, never folded into an answer.
type Faults interface {
	// Hang makes op block until its context ends.
	Hang(op vmm.Op)
	// Unreachable makes op fail at once as if the hypervisor did not answer.
	Unreachable(op vmm.Op)
	// LeaveOnDelete makes the hypervisor accept a delete and keep the VM.
	LeaveOnDelete()
	// Clear removes every fault.
	Clear()
}

// Harness supplies what the contract needs from one backend.
type Harness struct {
	// New returns a fresh VMM on one host.
	New func(t *testing.T) vmm.VMM
	// Elsewhere, if set, returns a VMM on a second host of a DIFFERENT restore family that reads the same snapshot
	// storage as the VMM New returned last.
	Elsewhere func(t *testing.T) vmm.VMM
	// Spec returns a bootable spec for a VM with the given id.
	Spec func(id string) vmm.Spec
	// Timeout bounds every operation that is expected to answer; zero means 30 s.
	Timeout time.Duration
}

func (h Harness) ctx(t *testing.T) context.Context {
	d := h.Timeout
	if d == 0 {
		d = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// Run runs the whole contract.
func Run(t *testing.T, h Harness) {
	t.Run("Capabilities", func(t *testing.T) { capabilities(t, h) })
	t.Run("BootInspectDelete", func(t *testing.T) { bootInspectDelete(t, h) })
	t.Run("BootRejects", func(t *testing.T) { bootRejects(t, h) })
	t.Run("SnapshotRestore", func(t *testing.T) { snapshotRestore(t, h) })
	t.Run("RestoreElsewhere", func(t *testing.T) { restoreElsewhere(t, h) })
	t.Run("CancelledContextIsUnknown", func(t *testing.T) { cancelled(t, h) })
	t.Run("Faults", func(t *testing.T) { faults(t, h) })
}

// checkPresence enforces the shape of every presence answer: unknown always carries the reason, gone never does.
func checkPresence(t *testing.T, what string, p vmm.Presence, err error) {
	t.Helper()
	if p == vmm.PresenceUnknown && err == nil {
		t.Errorf("%s: answered unknown without a reason", what)
	}
	if p == vmm.PresenceGone && err != nil {
		t.Errorf("%s: answered gone together with an error (%v); an unverified delete is unknown or present", what, err)
	}
}

func capabilities(t *testing.T, h Harness) {
	v := h.New(t)
	c, err := v.Capabilities(h.ctx(t))
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	switch c.SuspendMaxFidelity {
	case vmm.FidelityMemory:
		if c.RestoreFamily == "" || c.RestoreFamily == vmm.PortableRestoreFamily {
			t.Errorf("a MEMORY host must declare its own restore family, got %q", c.RestoreFamily)
		}
	case vmm.FidelityVolumes:
		if c.RestoreFamily != "" {
			t.Errorf("a VOLUMES host has no memory restore family, got %q", c.RestoreFamily)
		}
	default:
		t.Errorf("suspendMaxFidelity %v is neither VOLUMES nor MEMORY", c.SuspendMaxFidelity)
	}
	if c.Arch != "amd64" && c.Arch != "arm64" {
		t.Errorf("arch %q is neither amd64 nor arm64", c.Arch)
	}
}

func bootInspectDelete(t *testing.T, h Harness) {
	v, ctx := h.New(t), h.ctx(t)
	vm, err := v.Boot(ctx, h.Spec("a1"))
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if vm.ID != "a1" || vm.State != vmm.StateRunning {
		t.Errorf("Boot returned %+v, want a1 running", vm)
	}
	p, err := v.Inspect(ctx, "a1")
	checkPresence(t, "Inspect after Boot", p, err)
	if p != vmm.PresencePresent {
		t.Errorf("Inspect after Boot = %v (%v), want present", p, err)
	}
	p, err = v.Delete(ctx, "a1")
	checkPresence(t, "Delete", p, err)
	if p != vmm.PresenceGone || err != nil {
		t.Errorf("Delete = %v (%v), want gone", p, err)
	}
	p, err = v.Inspect(ctx, "a1")
	checkPresence(t, "Inspect after Delete", p, err)
	if p != vmm.PresenceGone {
		t.Errorf("Inspect after Delete = %v (%v), want gone", p, err)
	}
	p, err = v.Delete(ctx, "a1")
	checkPresence(t, "second Delete", p, err)
	if p != vmm.PresenceGone || err != nil {
		t.Errorf("Delete of a deleted VM = %v (%v), want gone: delete is idempotent", p, err)
	}
}

func bootRejects(t *testing.T, h Harness) {
	v, ctx := h.New(t), h.ctx(t)
	if _, err := v.Boot(ctx, h.Spec("b1")); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _, _ = v.Delete(context.Background(), "b1") })
	if _, err := v.Boot(ctx, h.Spec("b1")); !errors.Is(err, vmm.ErrExists) {
		t.Errorf("second Boot of b1 = %v, want ErrExists", err)
	}
	bad := h.Spec("")
	if _, err := v.Boot(ctx, bad); !errors.Is(err, vmm.ErrInvalidSpec) {
		t.Errorf("Boot with no id = %v, want ErrInvalidSpec", err)
	}
	if _, err := v.Snapshot(ctx, "nope", vmm.FidelityVolumes); !errors.Is(err, vmm.ErrNotFound) {
		t.Errorf("Snapshot of an unknown VM = %v, want ErrNotFound", err)
	}
}

func snapshotRestore(t *testing.T, h Harness) {
	v, ctx := h.New(t), h.ctx(t)
	c, err := v.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if _, err := v.Boot(ctx, h.Spec("s1")); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	vol, err := v.Snapshot(ctx, "s1", vmm.FidelityVolumes)
	if err != nil {
		t.Fatalf("Snapshot(VOLUMES): %v", err)
	}
	if vol.Fidelity != vmm.FidelityVolumes || vol.RestoreFamily != vmm.PortableRestoreFamily || vol.VMID != "s1" {
		t.Errorf("VOLUMES snapshot = %+v, want fidelity VOLUMES in family %s", vol, vmm.PortableRestoreFamily)
	}
	mem, err := v.Snapshot(ctx, "s1", vmm.FidelityMemory)
	if c.SuspendMaxFidelity < vmm.FidelityMemory {
		if !errors.Is(err, vmm.ErrFidelityUnsupported) {
			t.Errorf("Snapshot(MEMORY) on a VOLUMES host = %v, want ErrFidelityUnsupported", err)
		}
	} else if err != nil {
		t.Fatalf("Snapshot(MEMORY): %v", err)
	} else if mem.Fidelity != vmm.FidelityMemory || mem.RestoreFamily != c.RestoreFamily {
		t.Errorf("MEMORY snapshot = %+v, want family %s", mem, c.RestoreFamily)
	}
	// Suspend = snapshot, then verified delete; resume = restore.
	if p, err := v.Delete(ctx, "s1"); p != vmm.PresenceGone {
		t.Fatalf("Delete after snapshot = %v (%v)", p, err)
	}
	snaps := []vmm.Snapshot{vol}
	if c.SuspendMaxFidelity >= vmm.FidelityMemory {
		snaps = append(snaps, mem)
	}
	for _, s := range snaps {
		vm, err := v.Restore(ctx, s, h.Spec("s1"))
		if err != nil {
			t.Errorf("Restore(%v): %v", s.Fidelity, err)
			continue
		}
		if vm.State != vmm.StateRunning {
			t.Errorf("Restore(%v) left the VM %v", s.Fidelity, vm.State)
		}
		if p, err := v.Inspect(ctx, "s1"); p != vmm.PresencePresent {
			t.Errorf("Inspect after Restore(%v) = %v (%v)", s.Fidelity, p, err)
		}
		if _, err := v.Restore(ctx, s, h.Spec("s1")); !errors.Is(err, vmm.ErrExists) {
			t.Errorf("Restore over a running VM = %v, want ErrExists", err)
		}
		if p, err := v.Delete(ctx, "s1"); p != vmm.PresenceGone {
			t.Errorf("Delete after Restore = %v (%v)", p, err)
		}
	}
	if _, err := v.Restore(ctx, vmm.Snapshot{ID: "never-taken", VMID: "s1", Fidelity: vmm.FidelityVolumes, RestoreFamily: vmm.PortableRestoreFamily}, h.Spec("s1")); !errors.Is(err, vmm.ErrNotFound) {
		t.Errorf("Restore of a snapshot never taken = %v, want ErrNotFound", err)
	}
}

func restoreElsewhere(t *testing.T, h Harness) {
	if h.Elsewhere == nil {
		t.Skip("harness has no second host")
	}
	here, ctx := h.New(t), h.ctx(t)
	there := h.Elsewhere(t)
	c, err := here.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := here.Boot(ctx, h.Spec("e1")); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	vol, err := here.Snapshot(ctx, "e1", vmm.FidelityVolumes)
	if err != nil {
		t.Fatalf("Snapshot(VOLUMES): %v", err)
	}
	if _, err := there.Restore(ctx, vol, h.Spec("e1")); err != nil {
		t.Errorf("VOLUMES restore on another host = %v; VOLUMES restores anywhere", err)
	}
	if c.SuspendMaxFidelity < vmm.FidelityMemory {
		return
	}
	mem, err := here.Snapshot(ctx, "e1", vmm.FidelityMemory)
	if err != nil {
		t.Fatalf("Snapshot(MEMORY): %v", err)
	}
	_, _ = there.Delete(ctx, "e1")
	if _, err := there.Restore(ctx, mem, h.Spec("e1")); !errors.Is(err, vmm.ErrRestoreFamilyMismatch) {
		t.Errorf("MEMORY restore into another restore family = %v, want ErrRestoreFamilyMismatch", err)
	}
	if p, err := there.Inspect(ctx, "e1"); p != vmm.PresenceGone {
		t.Errorf("a refused restore left e1 %v (%v)", p, err)
	}
}

func cancelled(t *testing.T, h Harness) {
	v := h.New(t)
	if _, err := v.Boot(h.ctx(t), h.Spec("c1")); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, err := v.Inspect(ctx, "c1")
	checkPresence(t, "Inspect with a cancelled context", p, err)
	if p != vmm.PresenceUnknown || !errors.Is(err, context.Canceled) {
		t.Errorf("Inspect with a cancelled context = %v (%v), want unknown with context.Canceled", p, err)
	}
	p, err = v.Delete(ctx, "c1")
	checkPresence(t, "Delete with a cancelled context", p, err)
	if p != vmm.PresenceUnknown || !errors.Is(err, context.Canceled) {
		t.Errorf("Delete with a cancelled context = %v (%v), want unknown with context.Canceled", p, err)
	}
	if s, err := v.Snapshot(ctx, "c1", vmm.FidelityVolumes); err == nil || s != (vmm.Snapshot{}) {
		t.Errorf("Snapshot with a cancelled context = %+v (%v), want no snapshot and an error", s, err)
	}
	if p, err := v.Inspect(h.ctx(t), "c1"); p != vmm.PresencePresent {
		t.Errorf("c1 after the cancelled calls = %v (%v), want still present", p, err)
	}
	_, _ = v.Delete(h.ctx(t), "c1")
}

func faults(t *testing.T, h Harness) {
	v := h.New(t)
	f, ok := v.(Faults)
	if !ok {
		t.Skip("backend does not inject faults")
	}
	t.Cleanup(f.Clear)
	if _, err := v.Boot(h.ctx(t), h.Spec("f1")); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	short := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		t.Cleanup(cancel)
		return ctx
	}

	t.Run("DeleteTimesOut", func(t *testing.T) {
		f.Clear()
		f.Hang(vmm.OpDelete)
		p, err := v.Delete(short(), "f1")
		checkPresence(t, "hung Delete", p, err)
		if p != vmm.PresenceUnknown || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("hung Delete = %v (%v), want unknown with context.DeadlineExceeded", p, err)
		}
	})
	t.Run("InspectUnreachable", func(t *testing.T) {
		f.Clear()
		f.Unreachable(vmm.OpInspect)
		p, err := v.Inspect(h.ctx(t), "f1")
		checkPresence(t, "unreachable Inspect", p, err)
		if p != vmm.PresenceUnknown || !errors.Is(err, vmm.ErrUnreachable) {
			t.Errorf("unreachable Inspect = %v (%v), want unknown with ErrUnreachable", p, err)
		}
	})
	t.Run("DeleteNotVerified", func(t *testing.T) {
		f.Clear()
		f.LeaveOnDelete()
		p, err := v.Delete(h.ctx(t), "f1")
		checkPresence(t, "leaky Delete", p, err)
		if p != vmm.PresencePresent || !errors.Is(err, vmm.ErrDeleteNotVerified) {
			t.Errorf("Delete the hypervisor did not carry out = %v (%v), want present with ErrDeleteNotVerified", p, err)
		}
	})
	t.Run("SnapshotTimesOutIsNotDurable", func(t *testing.T) {
		f.Clear()
		f.Hang(vmm.OpSnapshot)
		s, err := v.Snapshot(short(), "f1", vmm.FidelityVolumes)
		if !errors.Is(err, context.DeadlineExceeded) || s != (vmm.Snapshot{}) {
			t.Errorf("hung Snapshot = %+v (%v), want no snapshot and context.DeadlineExceeded", s, err)
		}
	})
	t.Run("StillPresentAfterFaults", func(t *testing.T) {
		f.Clear()
		if p, err := v.Inspect(h.ctx(t), "f1"); p != vmm.PresencePresent {
			t.Errorf("f1 after the faults = %v (%v): an unknown answer was folded into absent", p, err)
		}
		if p, err := v.Delete(h.ctx(t), "f1"); p != vmm.PresenceGone {
			t.Errorf("Delete once healthy = %v (%v)", p, err)
		}
	})
}
