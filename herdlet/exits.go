package herdlet

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
)

// DefaultWatch is how often a herdlet checks its actors' VMs for an exit when Agent.Watch is zero.
const DefaultWatch = time.Second

// observer is the VMM that tells whether a VM exited, if the host's VMM can.
func (a *Agent) observer() (vmm.ExitObserver, bool) {
	v := a.VMM
	if a.Machines != nil {
		v = a.Machines.VMM()
	}
	o, ok := v.(vmm.ExitObserver)
	return o, ok
}

// watch starts (on) or stops watching the VM vm for an exit.
func (a *Agent) watch(vm string, on bool) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.running == nil {
		a.running = map[string]bool{}
	}
	if on {
		a.running[vm] = true
	} else {
		delete(a.running, vm)
	}
}

func (a *Agent) watched() []string {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	vms := make([]string, 0, len(a.running))
	for vm := range a.running {
		vms = append(vms, vm)
	}
	sort.Strings(vms)
	return vms
}

// watchExits checks every Watch that the VMs the herdlet runs for actors still run, and tells herd-api of each one
// whose hypervisor exited without being asked to, or (with Machines) in which herd-guest recorded that a container
// ended. A VM that could not be observed is checked again at the next tick: unknown is not an exit.
func (a *Agent) watchExits(ctx context.Context, send func(link.Up) error) {
	o, ok := a.observer()
	if !ok && a.Machines == nil {
		return
	}
	every := a.Watch
	if every == 0 {
		every = DefaultWatch
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, vm := range a.watched() {
			a.checkExit(ctx, o, vm, every, send)
		}
	}
}

// checkExit looks at one VM under its lock, so that no work on it is in flight: work that is (a suspend's stop, a
// delete) is asked for, and the VM is looked at again at the next tick. The exit is sent while the lock is held, so
// herd-api reads it before the outcome of any later work on the VM.
func (a *Agent) checkExit(ctx context.Context, o vmm.ExitObserver, vm string, bound time.Duration, send func(link.Up) error) {
	l, _ := a.vms.LoadOrStore(vm, make(vmLock, 1))
	select {
	case l.(vmLock) <- struct{}{}:
		defer func() { <-l.(vmLock) }()
	default:
		return
	}
	a.runMu.Lock()
	still := a.running[vm]
	a.runMu.Unlock()
	if !still {
		return
	}
	id := vm
	if a.Machines != nil {
		id = a.Machines.VMID(vm)
	}
	var (
		exited bool
		why    string
		err    error
	)
	if o != nil {
		cctx, cancel := context.WithTimeout(ctx, bound)
		exited, why, err = o.Exited(cctx, id)
		cancel()
	}
	reason := link.ExitHypervisorExited
	switch {
	case err == nil && exited:
	case a.Machines == nil:
		return
	default:
		// The VM runs (or could not be observed), but a container in it may have ended: herd-guest recorded it.
		cerr := a.Machines.ContainerExit(vm)
		if cerr == nil {
			return
		}
		reason, why = link.ExitContainerExited, cerr.Error()
	}
	if reason == link.ExitHypervisorExited && a.Machines != nil {
		// herd-guest left why its boot failed (a Cloud Hypervisor boot answers before the guest runs).
		if gerr := a.Machines.GuestBootError(vm); gerr != nil {
			why = gerr.Error() + "; " + why
		}
		if cerr := a.Machines.ContainerExit(vm); cerr != nil {
			why = cerr.Error() + "; " + why
		}
	}
	name := vm
	if id != vm {
		name += " (hypervisor id " + id + ")"
	}
	log.Printf("herdlet %s: VM %s: %s: %s", a.Worker, name, reason, why)
	if err := send(link.Up{Exited: &link.Exited{VM: vm, Reason: reason, Detail: why}}); err != nil {
		log.Printf("herdlet %s: reporting the exit of VM %s: %v (tried again)", a.Worker, vm, err)
		return
	}
	a.watch(vm, false)
}
