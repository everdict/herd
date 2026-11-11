package herdlet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/everdict/herd/snapstore"
)

// DefaultSnapshotSweep is how often a herdlet looks for VMM snapshots nothing restores from any more when
// Agent.SnapshotSweep is zero.
const DefaultSnapshotSweep = time.Minute

// snapRefs records, for each VMM snapshot on this host, the snapshot store's objects whose bundle holds it (PLT-374):
// the object a snapshot taken here was put under, and the object a restore here read it from. A copy whose objects
// are all gone is one no actor or tag can restore from (herd-api deletes an object with the last record naming it,
// A-45).
type snapRefs struct {
	mu   sync.Mutex
	keys map[string][]string // VMM snapshot id → object keys
}

// noteSnapshot records that the object key holds the VMM snapshot id.
func (a *Agent) noteSnapshot(id, key string) {
	if id == "" || key == "" || a.Machines == nil || !a.Machines.RemovesSnapshots() {
		return
	}
	a.refs.mu.Lock()
	defer a.refs.mu.Unlock()
	if a.refs.keys == nil {
		a.refs.keys = map[string][]string{}
	}
	if slices.Contains(a.refs.keys[id], key) {
		return
	}
	a.refs.keys[id] = append(a.refs.keys[id], key)
	a.saveRefs()
}

// saveRefs writes the records to Agent.SnapshotRefs, if set, so a restarted herdlet keeps the copies still held.
// The caller holds refs.mu.
func (a *Agent) saveRefs() {
	if a.SnapshotRefs == "" {
		return
	}
	b, err := json.Marshal(a.refs.keys)
	if err == nil {
		tmp := a.SnapshotRefs + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, a.SnapshotRefs)
		}
	}
	if err != nil {
		log.Printf("herdlet: snapshot records not written to %s: %v", a.SnapshotRefs, err)
	}
}

// loadRefs reads the records a herdlet before this one wrote.
func (a *Agent) loadRefs() error {
	keys := map[string][]string{}
	if a.SnapshotRefs != "" {
		b, err := os.ReadFile(a.SnapshotRefs)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err == nil {
			if err := json.Unmarshal(b, &keys); err != nil {
				return fmt.Errorf("%s: %w", a.SnapshotRefs, err)
			}
		}
	}
	a.refs.mu.Lock()
	defer a.refs.mu.Unlock()
	a.refs.keys = keys
	return nil
}

// SweepSnapshots removes the VMM snapshots on this host that no snapshot store object holds any more, so nothing
// can restore from them. A snapshot whose objects cannot be read (a store that does not answer) is kept. At a start
// (start), before any work, it also removes what a snapshot or an import that never finished left, and every
// snapshot no record names: one a herdlet took and never stored, or one taken before herdlets kept records. That never
// removes the only copy of a snapshot an actor or tag restores from: the herdlet removes VMM snapshots only where
// their bundles in the store carry their files (machine.Machines.RemovesSnapshots).
func (a *Agent) SweepSnapshots(ctx context.Context, start bool) error {
	m := a.Machines
	if m == nil || !m.RemovesSnapshots() {
		return nil
	}
	if start {
		if err := a.loadRefs(); err != nil {
			return fmt.Errorf("herdlet: snapshot records: %w", err)
		}
		if err := m.RemoveLeftovers(ctx); err != nil {
			return err
		}
	}
	a.refs.mu.Lock()
	recorded := make(map[string][]string, len(a.refs.keys))
	for id, keys := range a.refs.keys {
		recorded[id] = slices.Clone(keys)
	}
	a.refs.mu.Unlock()
	local, err := m.LocalSnapshots(ctx)
	if err != nil {
		return err
	}
	var gone []string // records of snapshots no longer here
	for id := range recorded {
		if !slices.Contains(local, id) {
			gone = append(gone, id)
		}
	}
	for _, id := range local {
		keys, ok := recorded[id]
		if !ok && !start {
			continue // taken or restored here just now, and not recorded yet
		}
		if ok && a.held(ctx, keys) {
			continue
		}
		removed, err := m.RemoveSnapshot(ctx, id)
		if err != nil {
			log.Printf("herdlet: VMM snapshot %s: not removed: %v", id, err)
			continue
		}
		if removed {
			log.Printf("herdlet: VMM snapshot %s removed from this host: no snapshot object holds it (%v)", id, keys)
			gone = append(gone, id)
		}
	}
	if len(gone) > 0 {
		a.refs.mu.Lock()
		for _, id := range gone {
			delete(a.refs.keys, id)
		}
		a.saveRefs()
		a.refs.mu.Unlock()
	}
	return nil
}

// held says whether any of the objects keys is in the snapshot store, or might be: only an answer of not found
// counts as gone.
func (a *Agent) held(ctx context.Context, keys []string) bool {
	for _, k := range keys {
		if _, err := a.Snapshots.Stat(ctx, k); !errors.Is(err, snapstore.ErrNotFound) {
			return true
		}
	}
	return false
}

// sweepSnapshots runs SweepSnapshots every SnapshotSweep until ctx ends.
func (a *Agent) sweepSnapshots(ctx context.Context) {
	every := a.SnapshotSweep
	if every == 0 {
		every = DefaultSnapshotSweep
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := a.SweepSnapshots(ctx, false); err != nil {
			log.Printf("herdlet: VMM snapshot sweep: %v", err)
		}
	}
}

// SnapshotRefsPath is where a herdlet with state directory stateDir keeps its snapshot records (Agent.SnapshotRefs).
func SnapshotRefsPath(stateDir string) string {
	return filepath.Join(stateDir, "vm-snapshot-refs.json")
}

// DefaultBundleSweep is how often a herdlet drops the bundles in its cache whose snapshot objects are all gone
// (bundlecache.Cache.Sweep, A-58).
const DefaultBundleSweep = 10 * time.Minute

// sweepBundles runs the bundle cache's Sweep every DefaultBundleSweep (or SnapshotSweep, if shorter) until ctx ends.
func (a *Agent) sweepBundles(ctx context.Context) {
	every := DefaultBundleSweep
	if a.SnapshotSweep > 0 && a.SnapshotSweep < every {
		every = a.SnapshotSweep
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := a.Bundles.Sweep(ctx, a.Snapshots); err != nil && ctx.Err() == nil {
			log.Printf("herdlet: bundle cache sweep: %v", err)
		}
	}
}
