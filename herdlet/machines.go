package herdlet

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/everdict/herd/herdlet/machine"
	"github.com/everdict/herd/image/registry"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/snapstore"
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
		if w.PullAuth != "" {
			// The boot's one-shot logins (image_pull_auth of a template's golden build or of a create that boots cold,
			// PLT-405): this boot's pulls only, ahead of -registry-auth.
			logins, err := registry.ParseLogins([]byte(w.PullAuth), "image_pull_auth")
			if err != nil {
				return link.Outcome{Result: link.ResultFailed, Reason: err.Error()}
			}
			ctx = registry.WithLogins(ctx, logins, "the boot's image_pull_auth")
		}
		if len(w.PlainHosts) > 0 {
			// The boot's one-shot plain-HTTP hosts (image_plain_http_hosts): this boot's pulls only.
			ctx = registry.WithPlainHosts(ctx, w.PlainHosts)
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
			// A restore repeated (its answer was lost): the fidelity this host restored the VM at, if it was this host.
			a.reopen(w.VM)
			return link.Outcome{Result: link.ResultDone, Restored: m.Restored(w.VM)}
		}
		rc, err := a.openSnapshot(ctx, *w.Snapshot)
		if err != nil {
			return classify(err)
		}
		defer rc.Close()
		snap, err := m.RestoreSnapshot(ctx, w.VM, rc)
		// An import kept the snapshot here even when the restore then failed: the object read holds it.
		a.noteSnapshot(snap.ID, w.Snapshot.Key)
		if err != nil {
			return classify(err)
		}
		a.reopen(w.VM)
		return link.Outcome{Result: link.ResultDone, Restored: m.Restored(w.VM)}
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
// fails mid-way leaves no object and the outcome says so. The bundle is hashed as it is written, so the store reads it
// once, to upload it (snapstore.Summed). One line logs the seconds and bytes of each phase (PLT-591).
func (a *Agent) machineSnapshot(ctx context.Context, w link.Work, stop bool) link.Outcome {
	begun := time.Now()
	create := func() (*os.File, error) { return os.CreateTemp("", "herd-bundle-*") }
	if a.Bundles != nil {
		// Beside the cache, so keeping the bundle there is a rename.
		create = a.Bundles.CreateTemp
	}
	f, err := create()
	if err != nil {
		return link.Outcome{Result: link.ResultFailed, Reason: err.Error()}
	}
	defer os.Remove(f.Name())
	defer f.Close()
	summer := snapstore.NewSummer(snapstore.PartSizeOf(a.Snapshots))
	sums := &timedWriter{w: summer}
	// herd-api waits for the bundle by its size (link.Progress): it learns the size as the bundle grows, and in full
	// before the upload.
	pw := &progressWriter{ctx: ctx, w: io.MultiWriter(f, sums), last: time.Now()}
	// The guest's flush comes first and reports nothing; it saves the containers' writable layers, which the VM's
	// memory bounds, into the bundle. herd-api learns that size now, so its call waits for the flush's bound
	// (machine.FlushTimeoutFor) and does not end it at its call timeout (PLT-378).
	if n, ok := a.Machines.Memory(w.VM); ok {
		progress(ctx, n)
	}
	// The snapshot is bounded by its lease's cancellation, which herd-api's Extend moves, never by the deadline the
	// lease shows now: the flush's own bound (context.WithTimeout over ctx) would otherwise freeze the call timeout
	// the lease had before that Extend landed, and cut the flush there.
	sctx, ph := machine.WithPhases(leaseDone{ctx})
	snap, err := a.Machines.Snapshot(sctx, w.VM, w.Fidelity, stop, pw)
	if err != nil {
		return classify(err)
	}
	progress(ctx, pw.n)
	if _, err := f.Seek(0, 0); err != nil {
		return link.Outcome{Result: link.ResultFailed, Reason: err.Error()}
	}
	uploaded := time.Now()
	obj, err := a.Snapshots.Put(ctx, w.SnapshotKey, snapstore.SummedFile(f, summer))
	if err != nil {
		// No object holds the snapshot, and herd-api records none: nothing restores from the host's copy.
		if _, rerr := a.Machines.RemoveSnapshot(ctx, snap.ID); rerr != nil {
			log.Printf("herdlet: VMM snapshot %s of a snapshot not stored: not removed: %v", snap.ID, rerr)
		}
		return classify(err)
	}
	upload := time.Since(uploaded)
	why := "fork"
	if stop {
		why = "suspend"
	}
	// The bundle's write and its hash run together; the bundle's phase is the write's part.
	log.Printf("herdlet %s: snapshot %s of %s (%s): flush %.3fs, vmm snapshot %.3fs, bundle %.3fs %d B, hash %.3fs %d B, upload %.3fs %d B, total %.3fs; %s, memory files %d B",
		a.Worker, obj.Key, w.VM, why, ph.Flush.Seconds(), ph.VMM.Seconds(), (ph.Bundle - sums.d).Seconds(), pw.n, sums.d.Seconds(), sums.n,
		upload.Seconds(), obj.Size, time.Since(begun).Seconds(), snap.Fidelity, ph.Memory)
	kept := false
	if a.Bundles != nil {
		// A snapshot this node took (a template's golden) is restored here from its own copy (PLT-227).
		if err := a.Bundles.KeepAs(f.Name(), obj.Key, obj.Digest); err != nil {
			log.Printf("herdlet: snapshot %s: not kept in the bundle cache: %v", obj.Key, err)
		} else {
			kept = true
		}
	}
	removed := false
	if kept && snap.ID != "" && a.Machines.RemovesSnapshots() {
		// One local copy (PLT-591): the bundle in the cache carries the VMM's snapshot files, and a restore here imports
		// them from it, so the VMM's own copy of the memory, as large as the VM's limit, goes now rather than at the
		// resume or the sweep.
		var err error
		if removed, err = a.Machines.RemoveSnapshot(ctx, snap.ID); err != nil {
			log.Printf("herdlet: VMM snapshot %s of %s: not removed beside its kept bundle: %v", snap.ID, obj.Key, err)
		}
	}
	if !removed {
		a.noteSnapshot(snap.ID, obj.Key)
	}
	return link.Outcome{Result: link.ResultDone, Snapshot: &link.SnapshotRef{
		Key: obj.Key, Digest: obj.Digest, Fidelity: snap.Fidelity, RestoreFamily: snap.RestoreFamily,
	}}
}

// timedWriter is w and how long its writes took, and how many bytes they took.
type timedWriter struct {
	w io.Writer
	d time.Duration
	n int64
}

func (t *timedWriter) Write(b []byte) (int, error) {
	start := time.Now()
	n, err := t.w.Write(b)
	t.d += time.Since(start)
	t.n += int64(n)
	return n, err
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
