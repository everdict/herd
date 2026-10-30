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
	"sync"
	"time"

	"github.com/everdict/herd/link"
	"github.com/everdict/herd/snapstore"
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
	// HypervisorVersion and Reason are reported with the capabilities.
	HypervisorVersion string
	Reason            string
	// Rewrite, if set, replaces each outcome before it is sent; returning false sends nothing. It exists so
	// conformance suites can make a herdlet lie or fall silent.
	Rewrite func(link.Work, link.Outcome) (link.Outcome, bool)
	// Boot is the spec template for a boot or restore; ID is set per VM.
	Boot vmm.Spec

	vms sync.Map // VM id → vmLock
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
		Capabilities: link.Capabilities{
			Platform: caps.Platform, Arch: caps.Arch, SuspendMaxFidelity: caps.SuspendMaxFidelity,
			RestoreFamily: caps.RestoreFamily, PortableRestoreFamily: vmm.PortableRestoreFamily,
			HypervisorVersion: a.HypervisorVersion, Reason: a.Reason,
		},
		Capacity: link.Capacity{Slots: a.Slots, Warm: a.Warm},
	}
	if err := send(link.Up{Register: &reg}); err != nil {
		return fmt.Errorf("herdlet: register: %w", err)
	}
	var wg sync.WaitGroup
	defer wg.Wait()
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
		if d.Work == nil {
			continue
		}
		w := *d.Work
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := a.do(ctx, w)
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

// classify turns a VMM error into an outcome: a call that hung or got no answer is unknown, anything else failed.
func classify(err error) link.Outcome {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, vmm.ErrUnreachable) {
		return link.Outcome{Result: link.ResultUnknown, Reason: err.Error()}
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
	if w.DeadlineUnixMilli != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.UnixMilli(w.DeadlineUnixMilli))
		defer cancel()
	}
	release, err := a.acquire(ctx, w.VM)
	if err != nil {
		return classify(err)
	}
	defer release()
	switch w.Op {
	case link.OpBoot:
		// VM ids are actor uids: a VM already there under this id is this actor's, so a repeated start is done.
		if _, err := a.VMM.Boot(ctx, a.spec(w.VM)); err != nil && !errors.Is(err, vmm.ErrExists) {
			return classify(err)
		}
		return link.Outcome{Result: link.ResultDone}
	case link.OpSnapshot:
		return a.snapshot(ctx, w)
	case link.OpRestore:
		return a.restore(ctx, w)
	case link.OpDelete, link.OpInspect:
		var p vmm.Presence
		var err error
		if w.Op == link.OpDelete {
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
	if _, err := a.Snapshots.Verify(ctx, w.Snapshot.Key, w.Snapshot.Digest); err != nil {
		return classify(err)
	}
	rc, err := a.Snapshots.Open(ctx, w.Snapshot.Key)
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
