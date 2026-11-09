// Package fake is an in-memory vmm.VMM. It keeps the same contract as the hardware backends (vmmtest.Run) and can be
// made to hang, to stop answering, or to accept a delete it does not carry out.
package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/everdict/herd/vmm"
)

// Store is snapshot storage several fakes can share, as hosts share a snapshot store.
type Store struct {
	mu    sync.Mutex
	snaps map[string]vmm.Snapshot
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{snaps: map[string]vmm.Snapshot{}} }

func (s *Store) put(snap vmm.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snaps[snap.ID] = snap
}

// Drop forgets every snapshot, as a host that lost its snapshot directory has.
func (s *Store) Drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snaps = map[string]vmm.Snapshot{}
}

func (s *Store) get(id string) (vmm.Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.snaps[id]
	return snap, ok
}

type fault int

const (
	faultHang fault = iota + 1
	faultUnreachable
)

// Fake is one fake host.
type Fake struct {
	caps  vmm.Capabilities
	store *Store

	mu            sync.Mutex
	vms           map[string]vmm.VM
	faults        map[vmm.Op]fault
	leaveOnDelete bool
	seq           int
}

// New returns a fake host with the given capabilities. A nil store gives it one of its own.
func New(caps vmm.Capabilities, store *Store) *Fake {
	if store == nil {
		store = NewStore()
	}
	return &Fake{caps: caps, store: store, vms: map[string]vmm.VM{}, faults: map[vmm.Op]fault{}}
}

// Hang makes op block until its context ends.
func (f *Fake) Hang(op vmm.Op) { f.setFault(op, faultHang) }

// Unreachable makes op fail with vmm.ErrUnreachable.
func (f *Fake) Unreachable(op vmm.Op) { f.setFault(op, faultUnreachable) }

// LeaveOnDelete makes Delete leave the VM in place.
func (f *Fake) LeaveOnDelete() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaveOnDelete = true
}

// Clear removes every fault.
func (f *Fake) Clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = map[vmm.Op]fault{}
	f.leaveOnDelete = false
}

func (f *Fake) setFault(op vmm.Op, ft fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[op] = ft
}

// enter applies the context and any injected fault to op.
func (f *Fake) enter(ctx context.Context, op vmm.Op) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("fake: %s: %w", op, err)
	}
	f.mu.Lock()
	ft := f.faults[op]
	f.mu.Unlock()
	switch ft {
	case faultHang:
		<-ctx.Done()
		return fmt.Errorf("fake: %s: %w", op, ctx.Err())
	case faultUnreachable:
		return fmt.Errorf("fake: %s: %w", op, vmm.ErrUnreachable)
	}
	return nil
}

func (f *Fake) Capabilities(ctx context.Context) (vmm.Capabilities, error) {
	if err := f.enter(ctx, vmm.OpCapabilities); err != nil {
		return vmm.Capabilities{}, err
	}
	return f.caps, nil
}

func (f *Fake) Boot(ctx context.Context, spec vmm.Spec) (vmm.VM, error) {
	if err := f.enter(ctx, vmm.OpBoot); err != nil {
		return vmm.VM{}, err
	}
	if err := spec.Validate(); err != nil {
		return vmm.VM{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.start(spec.ID)
}

func (f *Fake) start(id string) (vmm.VM, error) {
	if _, ok := f.vms[id]; ok {
		return vmm.VM{}, fmt.Errorf("fake: %s: %w", id, vmm.ErrExists)
	}
	vm := vmm.VM{ID: id, State: vmm.StateRunning}
	f.vms[id] = vm
	return vm, nil
}

func (f *Fake) Snapshot(ctx context.Context, id string, fid vmm.Fidelity) (vmm.Snapshot, error) {
	if err := f.enter(ctx, vmm.OpSnapshot); err != nil {
		return vmm.Snapshot{}, err
	}
	snap := vmm.Snapshot{VMID: id, Fidelity: fid}
	switch {
	case fid == vmm.FidelityVolumes:
		snap.RestoreFamily = vmm.PortableRestoreFamily
	case fid == vmm.FidelityMemory && f.caps.SuspendMaxFidelity >= vmm.FidelityMemory:
		snap.RestoreFamily = f.caps.RestoreFamily
	default:
		return vmm.Snapshot{}, fmt.Errorf("fake: snapshot %s at %v: %w", id, fid, vmm.ErrFidelityUnsupported)
	}
	f.mu.Lock()
	if _, ok := f.vms[id]; !ok {
		f.mu.Unlock()
		return vmm.Snapshot{}, fmt.Errorf("fake: snapshot %s: %w", id, vmm.ErrNotFound)
	}
	f.seq++
	snap.ID = fmt.Sprintf("%s-%s-%d", f.caps.RestoreFamily, id, f.seq)
	f.mu.Unlock()
	snap.Location = "fake://" + snap.ID
	f.store.put(snap)
	return snap, nil
}

func (f *Fake) Restore(ctx context.Context, s vmm.Snapshot, spec vmm.Spec) (vmm.VM, error) {
	if err := f.enter(ctx, vmm.OpRestore); err != nil {
		return vmm.VM{}, err
	}
	if err := spec.Validate(); err != nil {
		return vmm.VM{}, err
	}
	stored, ok := f.store.get(s.ID)
	if !ok || stored != s {
		return vmm.VM{}, fmt.Errorf("fake: restore %s: snapshot %q: %w", spec.ID, s.ID, vmm.ErrNotFound)
	}
	if s.Fidelity == vmm.FidelityMemory && (f.caps.SuspendMaxFidelity < vmm.FidelityMemory || s.RestoreFamily != f.caps.RestoreFamily) {
		return vmm.VM{}, fmt.Errorf("fake: restore %s: snapshot of %s on a %q host: %w", spec.ID, s.RestoreFamily, f.caps.RestoreFamily, vmm.ErrRestoreFamilyMismatch)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.start(spec.ID)
}

func (f *Fake) Inspect(ctx context.Context, id string) (vmm.Presence, error) {
	if err := f.enter(ctx, vmm.OpInspect); err != nil {
		return vmm.PresenceUnknown, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.vms[id]; ok {
		return vmm.PresencePresent, nil
	}
	return vmm.PresenceGone, nil
}

func (f *Fake) Delete(ctx context.Context, id string) (vmm.Presence, error) {
	if err := f.enter(ctx, vmm.OpDelete); err != nil {
		return vmm.PresenceUnknown, err
	}
	f.mu.Lock()
	if !f.leaveOnDelete {
		delete(f.vms, id)
	}
	f.mu.Unlock()
	// Verify the way a real backend must: ask again rather than trust the delete call.
	p, err := f.Inspect(ctx, id)
	switch {
	case err != nil:
		return vmm.PresenceUnknown, err
	case p == vmm.PresencePresent:
		return vmm.PresencePresent, fmt.Errorf("fake: delete %s: %w", id, vmm.ErrDeleteNotVerified)
	}
	return p, nil
}

var _ vmm.SnapshotCarrier = (*Fake)(nil)

// carriedFile is the one file a fake snapshot carries: its record.
const carriedFile = "fake-snapshot.json"

// ExportSnapshot gives the snapshot's record as its one file, when the fake's store holds it.
func (f *Fake) ExportSnapshot(ctx context.Context, s vmm.Snapshot) ([]vmm.SnapshotFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("fake: export: %w", err)
	}
	stored, ok := f.store.get(s.ID)
	if !ok || stored != s {
		return nil, fmt.Errorf("fake: export: snapshot %q: %w", s.ID, vmm.ErrNotFound)
	}
	b, err := json.Marshal(stored)
	if err != nil {
		return nil, err
	}
	return []vmm.SnapshotFile{{Name: carriedFile, Size: int64(len(b)),
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}}, nil
}

// ImportSnapshot puts the carried record in the fake's store, when it names s.
func (f *Fake) ImportSnapshot(ctx context.Context, s vmm.Snapshot, next func() (vmm.SnapshotFile, io.Reader, error)) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("fake: import: %w", err)
	}
	if stored, ok := f.store.get(s.ID); ok && stored == s {
		return nil
	}
	var got *vmm.Snapshot
	for {
		file, r, err := next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("fake: import %s: %w", s.ID, err)
		}
		if file.Name != carriedFile {
			return fmt.Errorf("fake: import %s: unexpected file %q", s.ID, file.Name)
		}
		var rec vmm.Snapshot
		if err := json.NewDecoder(io.LimitReader(r, file.Size)).Decode(&rec); err != nil {
			return fmt.Errorf("fake: import %s: %w", s.ID, err)
		}
		got = &rec
	}
	if got == nil || *got != s {
		return fmt.Errorf("fake: import %s: the files carry %+v", s.ID, got)
	}
	f.store.put(s)
	return nil
}

// IDs lists the snapshots the store holds.
func (s *Store) IDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.snaps))
	for id := range s.snaps {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

var _ vmm.SnapshotRemover = (*Fake)(nil)

// LocalSnapshots lists the snapshots in the fake's store.
func (f *Fake) LocalSnapshots(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("fake: snapshots: %w", err)
	}
	return f.store.IDs(), nil
}

// RemoveSnapshot forgets the snapshot id.
func (f *Fake) RemoveSnapshot(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("fake: remove snapshot: %w", err)
	}
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	delete(f.store.snaps, id)
	return nil
}

// RemoveLeftovers has nothing to remove: a fake snapshot is published whole or not at all.
func (f *Fake) RemoveLeftovers(ctx context.Context) error { return ctx.Err() }
