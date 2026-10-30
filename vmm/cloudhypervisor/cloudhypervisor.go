//go:build linux

package cloudhypervisor

import (
	"context"

	"github.com/everdict/herd/vmm"
)

const backend = "cloud-hypervisor"

// Config locates the Cloud Hypervisor installation and declares the host's CPU feature class.
type Config struct {
	// Binary is the cloud-hypervisor executable.
	Binary string
	// Virtiofsd is the virtiofsd executable.
	Virtiofsd string
	// StateDir holds API sockets, vsock sockets and snapshots.
	StateDir string
	// CPUClass is the CPU feature class of this host; MEMORY snapshots restore only within it.
	CPUClass string
}

// VMM is the Cloud Hypervisor backend.
type VMM struct {
	cfg Config
}

var _ vmm.VMM = (*VMM)(nil)

// New returns the backend for cfg.
func New(cfg Config) *VMM { return &VMM{cfg: cfg} }

func notImplemented(op vmm.Op) error { return &vmm.NotImplementedError{Backend: backend, Op: op} }

func (v *VMM) Capabilities(context.Context) (vmm.Capabilities, error) {
	return vmm.Capabilities{}, notImplemented(vmm.OpCapabilities)
}

func (v *VMM) Boot(context.Context, vmm.Spec) (vmm.VM, error) {
	return vmm.VM{}, notImplemented(vmm.OpBoot)
}

func (v *VMM) Snapshot(context.Context, string, vmm.Fidelity) (vmm.Snapshot, error) {
	return vmm.Snapshot{}, notImplemented(vmm.OpSnapshot)
}

func (v *VMM) Restore(context.Context, vmm.Snapshot, vmm.Spec) (vmm.VM, error) {
	return vmm.VM{}, notImplemented(vmm.OpRestore)
}

func (v *VMM) Inspect(context.Context, string) (vmm.Presence, error) {
	return vmm.PresenceUnknown, notImplemented(vmm.OpInspect)
}

func (v *VMM) Delete(context.Context, string) (vmm.Presence, error) {
	return vmm.PresenceUnknown, notImplemented(vmm.OpDelete)
}
