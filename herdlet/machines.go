package herdlet

import (
	"context"
	"fmt"
	"os"

	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
)

// doMachine carries out work through Machines (and the warm pool). The caller holds the VM's lock.
func (a *Agent) doMachine(ctx context.Context, w link.Work) link.Outcome {
	m := a.Machines
	switch w.Op {
	case link.OpBoot:
		if w.Spec == nil {
			return link.Outcome{Result: link.ResultFailed, Reason: "boot names no template"}
		}
		// VM ids are actor uids: a VM already there under this id is this actor's, so a repeated start is done.
		if p, _ := m.Inspect(ctx, w.VM); p == vmm.PresencePresent {
			a.reopen(w.VM)
			return link.Outcome{Result: link.ResultDone}
		}
		if a.Pool != nil {
			if id, ok := a.Pool.Take(ctx, *w.Spec); ok {
				// The warm VM keeps its own id on the hypervisor: its network learns the actor under that id.
				if w.Actor != nil && a.OnActor != nil {
					a.OnActor(id, *w.Actor)
				}
				m.Adopt(w.VM, id)
				a.reopen(w.VM)
				return link.Outcome{Result: link.ResultDone}
			}
		}
		if err := m.Boot(ctx, w.VM, *w.Spec); err != nil {
			return classify(err)
		}
		a.reopen(w.VM)
		return link.Outcome{Result: link.ResultDone}
	case link.OpSnapshot:
		suspend := w.Cause == link.CauseSuspend
		if suspend {
			a.cut(w.VM, w.Cause, "snapshot for suspend")
		}
		o := a.machineSnapshot(ctx, w, suspend)
		if suspend && o.Result == link.ResultFailed {
			// No snapshot was recorded: the actor stays running, so a VM the snapshot stopped boots again.
			if err := m.Restart(ctx, w.VM); err != nil {
				o.Reason += "; restarting the VM: " + err.Error()
			}
			a.reopen(w.VM)
		}
		return o
	case link.OpRestore:
		if w.Snapshot == nil {
			return link.Outcome{Result: link.ResultFailed, Reason: "restore names no snapshot"}
		}
		if p, _ := m.Inspect(ctx, w.VM); p == vmm.PresencePresent {
			a.reopen(w.VM)
			return link.Outcome{Result: link.ResultDone}
		}
		if _, err := a.Snapshots.Verify(ctx, w.Snapshot.Key, w.Snapshot.Digest); err != nil {
			return classify(err)
		}
		rc, err := a.Snapshots.Open(ctx, w.Snapshot.Key)
		if err != nil {
			return classify(err)
		}
		defer rc.Close()
		if err := m.Restore(ctx, w.VM, rc); err != nil {
			return classify(err)
		}
		a.reopen(w.VM)
		return link.Outcome{Result: link.ResultDone}
	case link.OpDelete, link.OpInspect:
		var p vmm.Presence
		var err error
		if w.Op == link.OpDelete {
			a.cut(w.VM, w.Cause, "VM released")
			p, err = m.Delete(ctx, w.VM)
		} else {
			p, err = m.Inspect(ctx, w.VM)
		}
		return presenceOutcome(w.Op, p, err)
	}
	return link.Outcome{Result: link.ResultFailed, Reason: fmt.Sprintf("unknown op %q", w.Op)}
}

// machineSnapshot writes the bundle to a local file first and then puts it in the snapshot store, so a store that
// fails mid-way leaves no object and the outcome says so.
func (a *Agent) machineSnapshot(ctx context.Context, w link.Work, stop bool) link.Outcome {
	f, err := os.CreateTemp("", "herd-bundle-*")
	if err != nil {
		return link.Outcome{Result: link.ResultFailed, Reason: err.Error()}
	}
	defer os.Remove(f.Name())
	defer f.Close()
	snap, err := a.Machines.Snapshot(ctx, w.VM, w.Fidelity, stop, f)
	if err != nil {
		return classify(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return link.Outcome{Result: link.ResultFailed, Reason: err.Error()}
	}
	obj, err := a.Snapshots.Put(ctx, w.SnapshotKey, f)
	if err != nil {
		return classify(err)
	}
	return link.Outcome{Result: link.ResultDone, Snapshot: &link.SnapshotRef{
		Key: obj.Key, Digest: obj.Digest, Fidelity: snap.Fidelity, RestoreFamily: snap.RestoreFamily,
	}}
}

func presenceOutcome(op link.Op, p vmm.Presence, err error) link.Outcome {
	o := link.Outcome{Result: link.ResultDone}
	switch p {
	case vmm.PresenceGone:
		o.Presence = link.PresenceGone
	case vmm.PresencePresent:
		o.Presence = link.PresencePresent
		if op == link.OpDelete {
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
