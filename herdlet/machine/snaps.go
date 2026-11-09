package machine

import (
	"context"
	"log"

	"github.com/everdict/herd/vmm"
)

// snapUse is what Machines knows of the VMM snapshots on its host (PLT-374), under Machines.mu.
type snapUse struct {
	// inUse counts the restores reading each snapshot id; removing holds a channel, closed when its removal ends,
	// for each id being removed. A restore waits for a removal of its snapshot to end, and a removal skips a snapshot in
	// use, so no restore finds a snapshot here and then loses it before the VMM restored from it.
	inUse    map[string]int
	removing map[string]chan struct{}
	// taken is the actor each snapshot was taken for here (a warm VM it adopted takes it under the warm VM's id).
	taken map[string]string
}

// remover is the VMM as a vmm.SnapshotRemover, when its snapshots may be removed from this host: only a VMM that
// is also a vmm.SnapshotCarrier, whose bundles in the snapshot store hold every file a restore needs (A-28).
func (m *Machines) remover() (vmm.SnapshotRemover, bool) {
	r, ok := m.cfg.VMM.(vmm.SnapshotRemover)
	_, carries := m.cfg.VMM.(vmm.SnapshotCarrier)
	return r, ok && carries
}

// RemovesSnapshots says whether the VMM's snapshots on this host may be removed: the snapshot store holds a copy of
// each one a restore needs.
func (m *Machines) RemovesSnapshots() bool {
	_, ok := m.remover()
	return ok
}

// LocalSnapshots lists the VMM snapshots on this host; none when they may not be removed.
func (m *Machines) LocalSnapshots(ctx context.Context) ([]string, error) {
	r, ok := m.remover()
	if !ok {
		return nil, nil
	}
	return r.LocalSnapshots(ctx)
}

// RemoveLeftovers removes what a snapshot or an import that never finished left on this host. It is for a start,
// before any work.
func (m *Machines) RemoveLeftovers(ctx context.Context) error {
	r, ok := m.remover()
	if !ok {
		return nil
	}
	return r.RemoveLeftovers(ctx)
}

// RemoveSnapshot removes the VMM snapshot id from this host. It answers false, and removes nothing, while a restore
// reads it or when the VMM's snapshots may not be removed.
func (m *Machines) RemoveSnapshot(ctx context.Context, id string) (bool, error) {
	r, ok := m.remover()
	if !ok {
		return false, nil
	}
	m.mu.Lock()
	if m.snaps.inUse[id] > 0 || m.snaps.removing[id] != nil {
		m.mu.Unlock()
		return false, nil
	}
	done := make(chan struct{})
	m.snaps.removing[id] = done
	m.mu.Unlock()
	err := r.RemoveSnapshot(context.WithoutCancel(ctx), id)
	m.mu.Lock()
	delete(m.snaps.removing, id)
	if err == nil {
		delete(m.snaps.taken, id)
	}
	m.mu.Unlock()
	close(done)
	return err == nil, err
}

// dropSnapshot removes the snapshot id from this host, if nothing reads it, and logs a removal that failed.
func (m *Machines) dropSnapshot(ctx context.Context, id string) {
	if id == "" {
		return
	}
	if _, err := m.RemoveSnapshot(ctx, id); err != nil {
		log.Printf("machine: snapshot %s: not removed from this host: %v", id, err)
	}
}

// useSnapshot marks the snapshot id in use by a restore, once no removal of it runs, until the answered func.
func (m *Machines) useSnapshot(id string) func() {
	m.mu.Lock()
	for m.snaps.removing[id] != nil {
		done := m.snaps.removing[id]
		m.mu.Unlock()
		<-done
		m.mu.Lock()
	}
	m.snaps.inUse[id]++
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.snaps.inUse[id]--; m.snaps.inUse[id] <= 0 {
			delete(m.snaps.inUse, id)
		}
	}
}

// taken records that the snapshot id was taken here for the actor id.
func (m *Machines) taken(snapID, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snaps.taken[snapID] = id
}

// ownSnapshot says whether s is a snapshot of the actor id itself (its suspend), rather than another actor's (a tag's
// or a golden's, which a fork restores from).
func (m *Machines) ownSnapshot(id string, s vmm.Snapshot) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return s.VMID == id || m.snaps.taken[s.ID] == id
}
