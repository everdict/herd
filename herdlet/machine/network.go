package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/vmm"
)

// Network is a host's VM networks (ADR A-18, A-21): egress/tapnet's Network on Ubuntu (a TAP, its egress table and
// the actor's binding per VM), a fixed link into the VM's own userspace stack on a Mac. Machines leases each VM its
// link before it writes the plan, brings the host's side up before the VM boots or restores and takes it down,
// verified, once the VM is verified gone.
type Network interface {
	// Lease leases VM id its link, or answers the one it holds: the guest's half goes into herd-guest's plan, the NIC
	// (nil when the VMM attaches its own) into the VM's spec. want, if set, is the link a MEMORY snapshot was taken
	// on, which the restored memory keeps: the lease is exactly that link or fails.
	Lease(ctx context.Context, id string, want *initd.Network) (initd.Network, *vmm.NIC, error)
	// Up creates the host's side of the VM's link; before Boot and Restore.
	Up(ctx context.Context, id string) error
	// Down removes the host's side and observes it gone; the lease stays.
	Down(ctx context.Context, id string) (vmm.Presence, error)
	// Release is Down and frees the lease.
	Release(ctx context.Context, id string) (vmm.Presence, error)
}

// vmLink is what a VM's lease gave it.
type vmLink struct {
	guest initd.Network
	nic   *vmm.NIC
}

// attachChecker is a Network that can tell whether the hypervisor holds a VM's link open (tapnet: the TAP's
// carrier).
type attachChecker interface {
	Attached(ctx context.Context, id string) error
}

// checkAttached logs whether the VM just started holds its link: a TAP without carrier means the VM runs without a
// network, which no later step reports.
func (m *Machines) checkAttached(ctx context.Context, vmID string) {
	c, ok := m.cfg.Network.(attachChecker)
	if !ok {
		return
	}
	if err := c.Attached(ctx, vmID); err != nil {
		log.Printf("machine: %s started, but its link is not attached: %v", vmID, err)
		return
	}
	log.Printf("machine: %s started on its link", vmID)
}

// ErrGuestBoot is answered when herd-guest reported that the VM could not boot (initd.BootErrorPath, e.g. a network
// step it could not carry out); the guest's reason follows it.
var ErrGuestBoot = errors.New("machine: the guest could not boot")

// maxBootError bounds what is read of the guest's report.
const maxBootError = 4 << 10

// GuestBootError answers the boot failure herd-guest left in the share of the actor's VM, wrapping ErrGuestBoot, or
// nil when there is none. The share is the guest's to write, so it is read through os.Root.
func (m *Machines) GuestBootError(id string) error {
	root, err := os.OpenRoot(m.ShareDir(id))
	if err != nil {
		return nil
	}
	defer root.Close()
	f, err := root.Open(initd.BootErrorPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, maxBootError))
	return fmt.Errorf("%w: %s", ErrGuestBoot, strings.TrimSpace(string(b)))
}

// withGuestBootError puts the guest's own reason in front of a failed start's error.
func (m *Machines) withGuestBootError(vmID string, err error) error {
	if gerr := m.GuestBootError(vmID); gerr != nil {
		return fmt.Errorf("%w (%v)", gerr, err)
	}
	return err
}
