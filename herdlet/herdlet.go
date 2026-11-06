// Package herdlet is the node agent's link to herd-api: it dials out, registers what its host declares, and carries
// out work on its vmm.VMM, answering each piece of work with a three-valued outcome. It is the whole herdlet for the
// fake hypervisor; the microVM backends (R2) plug in as vmm.VMM implementations.
package herdlet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"time"

	"github.com/everdict/herd/herdlet/machine"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/snapstore/bundlecache"
	"github.com/everdict/herd/tunnel"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc"
)

// Agent is one herdlet.
type Agent struct {
	Worker    string
	Class     string
	Labels    map[string]string
	Slots     int
	Warm      map[string]int
	VMM       vmm.VMM
	Snapshots snapstore.Store
	// Bundles, if set, is this node's copy of the snapshot objects it wrote or fetched (PLT-227): a restore of one it
	// holds reads no store, and one it does not hold is read from the store once and kept.
	Bundles *bundlecache.Cache
	// HypervisorVersion and Reason are reported with the capabilities.
	HypervisorVersion string
	Reason            string
	// Guest names the guest build this herdlet boots (deploy.GuestIdentity), reported with the capabilities so that
	// herd-api can rebuild a golden snapshot an older guest made (DEFAUL-1606).
	Guest string
	// EgressAllowPrefixes are the operator's lab allowances on this host's herd-egress; they are reported with the
	// capabilities so a conformance report can say they were set.
	EgressAllowPrefixes []string
	// Rewrite, if set, replaces each outcome before it is sent; returning false sends nothing. It exists so
	// conformance suites can make a herdlet lie or fall silent.
	Rewrite func(link.Work, link.Outcome) (link.Outcome, bool)
	// Boot is the spec template for a boot or restore; ID is set per VM.
	Boot vmm.Spec
	// Tunnel, if set, is this herdlet's tunnel to herd-router: a suspend or delete cuts the VM's streams with the
	// reason, and a boot or restore lets the router reach the VM again.
	Tunnel *Tunnel
	// Recheck is how often the herdlet asks its VMM for its capabilities again and pushes a change to herd-api;
	// zero means DefaultRecheck, negative never.
	Recheck time.Duration
	// Wake, if set, signals that the host woke from sleep (WakeDetector): the herdlet invalidates what its VMM
	// learned by probing (a VMM with InvalidateProbe, e.g. vz) and rechecks at once.
	Wake <-chan struct{}
	// OnActor, if set, is told which actor a VM is before the VM is booted or restored (on a Mac:
	// egress.StackNetwork.Assign, so the VM's network is served under the actor's egress policy).
	OnActor func(vm string, a link.ActorRef)
	// Machines, if set, turns templates into VMs (ADR A-19): a boot builds the template's images and lays out its
	// share, a snapshot object is a bundle carrying the volume tree, a restore unpacks it. Without it the herdlet
	// boots Boot as is and a snapshot object is the VMM's record only.
	Machines *machine.Machines
	// Pool, if set (with Machines), keeps warm VMs per template: a boot takes one first, and Run refills it and
	// reports its counts to herd-api as capacity.
	Pool *machine.Pool
	// Watch is how often the herdlet checks that the VMs it runs for actors still run, and tells herd-api of one whose
	// hypervisor exited on its own, or (with Machines) in whose share herd-guest recorded that a container ended; zero
	// means DefaultWatch, negative never. Only a VMM that is a vmm.ExitObserver is watched for hypervisor exits.
	Watch time.Duration

	vms sync.Map // VM id → vmLock
	// leases are the deadlines of the work in flight, by work id.
	leases sync.Map

	runMu   sync.Mutex
	running map[string]bool // VM ids (as herd-api names them) of the VMs this herdlet runs for actors
}

// vmLock serializes work on one VM: a delete waits for a boot still in flight instead of racing it.
type vmLock chan struct{}

func (a *Agent) acquire(ctx context.Context, vm string) (func(), error) {
	l, _ := a.vms.LoadOrStore(vm, make(vmLock, 1))
	select {
	case l.(vmLock) <- struct{}{}:
		return func() { <-l.(vmLock) }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("herdlet: waiting for earlier work on VM %s: %w", vm, ctx.Err())
	}
}

// Run opens the link to herd-api on cc (the herdlet dials; herd-api never does), registers, and serves work until ctx ends or the link breaks.
func (a *Agent) Run(ctx context.Context, cc grpc.ClientConnInterface) error {
	caps, err := a.VMM.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("herdlet: capabilities: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cs, err := cc.NewStream(ctx, &link.StreamDesc, link.LinkMethod, grpc.CallContentSubtype(link.CodecName))
	if err != nil {
		return fmt.Errorf("herdlet: link: %w", err)
	}
	st := link.NewGRPCStream(cs)
	var sendMu sync.Mutex
	send := func(u link.Up) error {
		b, err := json.Marshal(u)
		if err != nil {
			return err
		}
		sendMu.Lock()
		defer sendMu.Unlock()
		return st.Send(b)
	}
	reg := link.Register{
		Worker: a.Worker, Class: a.Class, Labels: a.Labels,
		Capabilities: a.declare(caps),
		Capacity:     link.Capacity{Slots: a.Slots, Warm: a.Warm},
	}
	if a.Pool != nil {
		reg.Capacity.Warm = a.Pool.Counts()
	}
	if err := send(link.Up{Register: &reg}); err != nil {
		return fmt.Errorf("herdlet: register: %w", err)
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	// The watch stops with the link (before wg.Wait, which waits for it).
	wctx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.watchCapabilities(wctx, reg.Capabilities, send)
	}()
	if _, ok := a.observer(); (ok || a.Machines != nil) && a.Watch >= 0 {
		// The watch stops with the link (before wg.Wait, which waits for it).
		xctx, stopExits := context.WithCancel(ctx)
		defer stopExits()
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.watchExits(xctx, send)
		}()
	}
	if a.Pool != nil {
		a.Pool.Changed = func(warm map[string]int) {
			_ = send(link.Up{Capacity: &link.Capacity{Slots: a.Slots, Warm: warm}})
		}
		// The pool stops with the link (before wg.Wait, which waits for it).
		pctx, stopPool := context.WithCancel(ctx)
		defer stopPool()
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.Pool.Run(pctx)
		}()
	}
	for {
		b, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("herdlet: link: %w", err)
		}
		var d link.Down
		if err := json.Unmarshal(b, &d); err != nil {
			return fmt.Errorf("herdlet: decode: %w", err)
		}
		if d.Extend != nil {
			a.extend(*d.Extend)
			continue
		}
		if d.Work == nil {
			continue
		}
		w := *d.Work
		wg.Add(1)
		go func() {
			defer wg.Done()
			wctx := withProgress(ctx, func(n int64) { _ = send(link.Up{Progress: &link.Progress{ID: w.ID, Bytes: n}}) })
			o := a.logged(wctx, w)
			o.ID = w.ID
			if a.Rewrite != nil {
				var ok bool
				if o, ok = a.Rewrite(w, o); !ok {
					return
				}
			}
			_ = send(link.Up{Outcome: &o})
		}()
	}
}

// classify turns a VMM error into an outcome: a call that hung or got no answer is unknown, anything else failed; a
// registry that kept refusing an image (image.ErrUnavailable) fails with CodeUnavailable.
func classify(err error) link.Outcome {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, vmm.ErrUnreachable) {
		return link.Outcome{Result: link.ResultUnknown, Reason: err.Error()}
	}
	if errors.Is(err, image.ErrUnavailable) {
		return link.Outcome{Result: link.ResultFailed, Reason: err.Error(), Code: link.CodeUnavailable}
	}
	return link.Outcome{Result: link.ResultFailed, Reason: err.Error()}
}

func (a *Agent) spec(id string) vmm.Spec {
	s := a.Boot
	s.ID = id
	if s.Kernel == "" {
		s.Kernel = "herd-guest"
	}
	if s.VCPUs == 0 {
		s.VCPUs = 1
	}
	if s.MemoryMiB == 0 {
		s.MemoryMiB = 512
	}
	return s
}

func (a *Agent) do(ctx context.Context, w link.Work) link.Outcome {
	ctx, end := a.withLease(ctx, w)
	defer end()
	release, err := a.acquire(ctx, w.VM)
	if err != nil {
		return classify(err)
	}
	defer release()
	if w.Actor != nil && a.OnActor != nil && (w.Op == link.OpBoot || w.Op == link.OpRestore) {
		a.OnActor(w.VM, *w.Actor)
	}
	if a.Machines != nil {
		return a.doMachine(ctx, w)
	}
	switch w.Op {
	case link.OpBoot:
		// VM ids are actor uids: a VM already there under this id is this actor's, so a repeated start is done.
		if _, err := a.VMM.Boot(ctx, a.spec(w.VM)); err != nil && !errors.Is(err, vmm.ErrExists) {
			return classify(err)
		}
		a.reopen(w.VM)
		return link.Outcome{Result: link.ResultDone}
	case link.OpSnapshot:
		if w.Cause == link.CauseSuspend {
			// What a client sees after this point would not be in the snapshot: cut its streams now.
			a.cut(w.VM, w.Cause, "snapshot for suspend")
		}
		o := a.snapshot(ctx, w)
		if w.Cause == link.CauseSuspend && o.Result == link.ResultFailed {
			// No snapshot was taken and the actor stays running.
			a.reopen(w.VM)
		}
		return o
	case link.OpRestore:
		o := a.restore(ctx, w)
		if o.Result == link.ResultDone {
			a.reopen(w.VM)
		}
		return o
	case link.OpDelete, link.OpInspect:
		var p vmm.Presence
		var err error
		if w.Op == link.OpDelete {
			a.cut(w.VM, w.Cause, "VM released")
			p, err = a.VMM.Delete(ctx, w.VM)
		} else {
			p, err = a.VMM.Inspect(ctx, w.VM)
		}
		o := link.Outcome{Result: link.ResultDone}
		switch p {
		case vmm.PresenceGone:
			o.Presence = link.PresenceGone
		case vmm.PresencePresent:
			o.Presence = link.PresencePresent
			if w.Op == link.OpDelete {
				o.Result = link.ResultFailed
			}
		default:
			o.Result = link.ResultUnknown
		}
		if err != nil {
			o.Reason = err.Error()
		}
		return o
	}
	return link.Outcome{Result: link.ResultFailed, Reason: fmt.Sprintf("unknown op %q", w.Op)}
}

// cut ends the VM's tunnel streams with the reason the cause names, and refuses new ones. The VM is no longer
// watched for an exit: what follows (a stop after the snapshot, a delete) is asked for.
func (a *Agent) cut(vm string, c link.Cause, detail string) {
	a.watch(vm, false)
	if a.Tunnel == nil {
		return
	}
	r := tunnel.ReasonDeleted
	if c == link.CauseSuspend {
		r = tunnel.ReasonSuspended
	}
	a.Tunnel.Close(vm, r, detail)
}

// reopen lets the router reach the VM again and watches it for an exit.
func (a *Agent) reopen(vm string) {
	a.watch(vm, true)
	if a.Tunnel != nil {
		a.Tunnel.Reopen(vm)
	}
}

func (a *Agent) snapshot(ctx context.Context, w link.Work) link.Outcome {
	snap, err := a.VMM.Snapshot(ctx, w.VM, w.Fidelity)
	if err != nil {
		return classify(err)
	}
	// The object is the snapshot's manifest. Real backends also upload the snapshot's files under the same key
	// prefix (R2); for the store it is one object with one digest either way.
	b, err := json.Marshal(snap)
	if err != nil {
		return link.Outcome{Result: link.ResultFailed, Reason: err.Error()}
	}
	obj, err := a.Snapshots.Put(ctx, w.SnapshotKey, bytes.NewReader(b))
	if err != nil {
		return classify(err)
	}
	return link.Outcome{Result: link.ResultDone, Snapshot: &link.SnapshotRef{
		Key: obj.Key, Digest: obj.Digest, Fidelity: snap.Fidelity, RestoreFamily: snap.RestoreFamily,
	}}
}

func (a *Agent) restore(ctx context.Context, w link.Work) link.Outcome {
	if w.Snapshot == nil {
		return link.Outcome{Result: link.ResultFailed, Reason: "restore names no snapshot"}
	}
	rc, err := a.openSnapshot(ctx, *w.Snapshot)
	if err != nil {
		return classify(err)
	}
	defer rc.Close()
	var snap vmm.Snapshot
	if err := json.NewDecoder(rc).Decode(&snap); err != nil {
		return link.Outcome{Result: link.ResultFailed, Reason: fmt.Sprintf("snapshot manifest: %v", err)}
	}
	if _, err := a.VMM.Restore(ctx, snap, a.spec(w.VM)); err != nil && !errors.Is(err, vmm.ErrExists) {
		return classify(err)
	}
	return link.Outcome{Result: link.ResultDone}
}

// openSnapshot returns the bytes of the snapshot object ref names, verified to be its digest: through the bundle
// cache when the herdlet has one, else verified in the store and then read from it.
func (a *Agent) openSnapshot(ctx context.Context, ref link.SnapshotRef) (io.ReadCloser, error) {
	if a.Bundles != nil {
		return a.Bundles.Open(ctx, a.Snapshots, ref.Key, ref.Digest)
	}
	if _, err := a.Snapshots.Verify(ctx, ref.Key, ref.Digest); err != nil {
		return nil, err
	}
	return a.Snapshots.Open(ctx, ref.Key)
}

// DefaultRecheck is how often a herdlet re-reads its capabilities when Agent.Recheck is zero.
const DefaultRecheck = 30 * time.Second

// declare is what the herdlet registers for the VMM's capabilities.
func (a *Agent) declare(caps vmm.Capabilities) link.Capabilities {
	reason := caps.Reason
	if reason == "" {
		reason = a.Reason
	}
	return link.Capabilities{
		Platform: caps.Platform, Arch: caps.Arch, SuspendMaxFidelity: caps.SuspendMaxFidelity,
		RestoreFamily: caps.RestoreFamily, PortableRestoreFamily: vmm.PortableRestoreFamily,
		HypervisorVersion: a.HypervisorVersion, Reason: reason, EgressAllowPrefixes: a.EgressAllowPrefixes,
		Guest: a.Guest,
	}
}

// probeInvalidator is a VMM whose capabilities rest on a probe the host's sleep may have made stale (vz.VMM).
type probeInvalidator interface{ InvalidateProbe() }

// watchCapabilities re-reads the VMM's capabilities every Recheck and on each wake, and sends herd-api every change,
// so the worker record and the scheduler see what the host declares now (a Mac that locked is VOLUMES at once). A
// read that fails keeps the last declaration and is tried again at the next tick.
func (a *Agent) watchCapabilities(ctx context.Context, last link.Capabilities, send func(link.Up) error) {
	every := a.Recheck
	if every == 0 {
		every = DefaultRecheck
	}
	var tick <-chan time.Time
	if every > 0 {
		t := time.NewTicker(every)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		case <-a.Wake:
			if p, ok := a.VMM.(probeInvalidator); ok {
				p.InvalidateProbe()
			}
		}
		caps, err := a.VMM.Capabilities(ctx)
		if err != nil {
			continue
		}
		now := a.declare(caps)
		if reflect.DeepEqual(now, last) {
			continue
		}
		if err := send(link.Up{Capabilities: &now}); err != nil {
			return
		}
		last = now
	}
}
