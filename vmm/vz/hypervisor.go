package vz

import (
	"context"
	"fmt"
	"net"
	"os"

	"github.com/everdict/herd/vmm"
)

// Hypervisor is the hardware boundary: everything the backend asks of Virtualization.framework. Native (darwin,
// arm64, cgo) implements it through Code-Hex/vz; vzfake implements it in memory so the backend's logic runs and is
// tested on every platform.
type Hypervisor interface {
	// NewMachine builds a VM from cfg without starting it. The returned machine is stopped.
	NewMachine(cfg MachineConfig) (Machine, error)
}

// MachineConfig is what one Virtualization.framework VM is built from.
type MachineConfig struct {
	Kernel    string
	Initrd    string
	Cmdline   string
	VCPUs     int
	MemoryMiB int
	// Disks are virtio-blk devices in order (the guest sees vda, vdb, ...).
	Disks []Disk
	// Shares are virtio-fs devices, one per tag.
	Shares []vmm.Share
	// Network, if set, is one end of a datagram socket pair: the VM's NIC is a file-handle attachment on it, and
	// herd-egress's userspace stack holds the other end. Nil means no NIC.
	Network *os.File
	// NetworkMAC, if set, is the NIC's MAC (xx:xx:xx:xx:xx:xx); empty lets the framework pick one.
	NetworkMAC string
	// Console, if set, is a file the guest's hvc0 console is appended to.
	Console string
	// MachineID is the generic platform's machine identifier (opaque bytes). A save file restores only into a VM
	// with the same identifier, so a MEMORY snapshot records it. Nil asks for a fresh one.
	MachineID []byte
}

// Disk is a raw disk image on the host.
type Disk struct {
	Path     string
	ReadOnly bool
}

// MachineState is Virtualization.framework's VZVirtualMachineState.
type MachineState int

const (
	MachineStopped MachineState = iota
	MachineRunning
	MachinePaused
	MachineError
	MachineStarting
	MachinePausing
	MachineResuming
	MachineStopping
	MachineSaving
	MachineRestoring
)

func (s MachineState) String() string {
	switch s {
	case MachineStopped:
		return "stopped"
	case MachineRunning:
		return "running"
	case MachinePaused:
		return "paused"
	case MachineError:
		return "error"
	case MachineStarting:
		return "starting"
	case MachinePausing:
		return "pausing"
	case MachineResuming:
		return "resuming"
	case MachineStopping:
		return "stopping"
	case MachineSaving:
		return "saving"
	case MachineRestoring:
		return "restoring"
	}
	return fmt.Sprintf("MachineState(%d)", int(s))
}

// Machine is one Virtualization.framework VM. Every method may block (Virtualization.framework answers through
// completion handlers on the VM's dispatch queue); the backend never calls one without a context bounding it.
type Machine interface {
	// Start boots a stopped machine.
	Start() error
	// Pause pauses a running machine.
	Pause() error
	// Resume resumes a paused machine.
	Resume() error
	// Stop stops the machine at once (no guest shutdown).
	Stop() error
	// State is the machine's current state as Virtualization.framework last reported it.
	State() (MachineState, error)
	// ConnectVsock opens a stream to a port the guest listens on over the native virtio-vsock device.
	ConnectVsock(port uint32) (net.Conn, error)
	// ValidateSaveRestore is validateSaveRestoreSupport for this machine's configuration.
	ValidateSaveRestore() (bool, error)
	// SaveState writes a paused machine's state to path (saveMachineStateTo).
	SaveState(path string) error
	// RestoreState loads a stopped machine's state from path and leaves it paused (restoreMachineStateFrom).
	RestoreState(path string) error
	// MachineID is the generic platform machine identifier the machine was built with.
	MachineID() []byte
}

// callValue runs fn and waits for it or for ctx, whichever ends first. A Virtualization.framework call that does
// not return leaves its goroutine behind; the caller gets ctx's error and answers unknown, never a guess.
func callValue[T any](ctx context.Context, what string, fn func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, fmt.Errorf("vz: %s: %w", what, err)
	}
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := fn()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return zero, fmt.Errorf("vz: %s: %w", what, r.err)
		}
		return r.v, nil
	case <-ctx.Done():
		return zero, fmt.Errorf("vz: %s: %w", what, ctx.Err())
	}
}

// call is callValue for a call that answers only an error.
func call(ctx context.Context, what string, fn func() error) error {
	_, err := callValue(ctx, what, func() (struct{}, error) { return struct{}{}, fn() })
	return err
}

// stateOf asks a machine its state within ctx.
func stateOf(ctx context.Context, m Machine) (MachineState, error) {
	return callValue(ctx, "state", m.State)
}
