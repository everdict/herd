package vmmtest

import (
	"context"
	"errors"
	"testing"

	"github.com/everdict/herd/vmm"
)

// NotImplemented checks a stub backend: every operation answers a typed *vmm.NotImplementedError naming the backend
// and the operation, and the presence answers are unknown, never gone.
func NotImplemented(t *testing.T, v vmm.VMM, backend string) {
	t.Helper()
	ctx := context.Background()
	spec := vmm.Spec{ID: "x1", Kernel: "/k", VCPUs: 1, MemoryMiB: 256}
	check := func(op vmm.Op, err error) {
		t.Helper()
		var ni *vmm.NotImplementedError
		if !errors.Is(err, vmm.ErrNotImplemented) || !errors.As(err, &ni) {
			t.Errorf("%s: %v, want a *vmm.NotImplementedError", op, err)
			return
		}
		if ni.Backend != backend || ni.Op != op {
			t.Errorf("%s: NotImplementedError{%q, %q}, want {%q, %q}", op, ni.Backend, ni.Op, backend, op)
		}
	}
	_, err := v.Capabilities(ctx)
	check(vmm.OpCapabilities, err)
	_, err = v.Boot(ctx, spec)
	check(vmm.OpBoot, err)
	_, err = v.Snapshot(ctx, "x1", vmm.FidelityVolumes)
	check(vmm.OpSnapshot, err)
	_, err = v.Restore(ctx, vmm.Snapshot{ID: "s", VMID: "x1", Fidelity: vmm.FidelityVolumes}, spec)
	check(vmm.OpRestore, err)
	p, err := v.Inspect(ctx, "x1")
	check(vmm.OpInspect, err)
	if p != vmm.PresenceUnknown {
		t.Errorf("Inspect answered %v; a stub cannot know", p)
	}
	p, err = v.Delete(ctx, "x1")
	check(vmm.OpDelete, err)
	if p != vmm.PresenceUnknown {
		t.Errorf("Delete answered %v; a stub cannot know", p)
	}
}
