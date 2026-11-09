//go:build linux

package cloudhypervisor

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/everdict/herd/herdlet"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/snapstore/disk"
	"github.com/everdict/herd/vmm"
)

func sweepTemplate() (*imagetest.Registry, link.Template) {
	reg := imagetest.NewRegistry()
	reg.Push("team/agent", "1", imagetest.Manifest{Platform: image.Platform{OS: "linux", Architecture: "amd64"},
		Config: image.Config{Cmd: []string{"/bin/run"}},
		Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "bin/run", Body: "run", Mode: 0o755})}})
	return reg, link.Template{UID: "tpl-1", Volumes: []string{"work"}, Containers: []link.Container{{Name: "main", Image: "registry.example/team/agent:1",
		VolumeMounts: []link.VolumeMount{{Name: "work", MountPath: "/work"}}}}}
}

func snapshotDirs(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// TestResumeLeavesNoSnapshotDirectory (PLT-374, criterion bounded): create, suspend at MEMORY, resume and delete, as
// the supervisor's 400 lifecycles on real KVM did, leaves vm-snapshots as it was. The suspend's directory is the
// actor's own and its resume has used it: the bundle in the store holds the same files (A-28), so nothing restores from
// the directory any more. Its counterexample is v0.1.16, where one directory per cycle stays (427 after 400 cycles).
func TestResumeLeavesNoSnapshotDirectory(t *testing.T) {
	ctx := ctxT(t, 20*time.Second)
	reg, tpl := sweepTemplate()
	n := newCHNode(t, shortDir(t), "node", reg)
	for cycle := 1; cycle <= 3; cycle++ {
		if err := n.m.Boot(ctx, "a1", tpl); err != nil {
			t.Fatalf("cycle %d: boot: %v", cycle, err)
		}
		var bundle bytes.Buffer
		if _, err := n.m.Snapshot(ctx, "a1", vmm.FidelityMemory, true, &bundle); err != nil {
			t.Fatalf("cycle %d: suspend: %v", cycle, err)
		}
		if p, err := n.m.Delete(ctx, "a1"); p != vmm.PresenceGone {
			t.Fatalf("cycle %d: release after the suspend = %v (%v)", cycle, p, err)
		}
		if err := n.m.Restore(ctx, "a1", &bundle); err != nil {
			t.Fatalf("cycle %d: resume: %v", cycle, err)
		}
		if p, err := n.m.Delete(ctx, "a1"); p != vmm.PresenceGone {
			t.Fatalf("cycle %d: delete = %v (%v)", cycle, p, err)
		}
		if left := snapshotDirs(t, n.snaps); len(left) != 0 {
			t.Errorf("cycle %d: vm-snapshots holds %d entries after create, suspend, resume, delete: %v", cycle, len(left), left)
		}
	}
}

// TestForkKeepsTheSnapshotItRestoredFrom (PLT-374, criterion bounded): a snapshot another actor restores from (a fork
// of a tag, a golden's session) is not its own: the directory stays after the fork's restore and delete, and the next
// fork restores from it.
func TestForkKeepsTheSnapshotItRestoredFrom(t *testing.T) {
	ctx := ctxT(t, 20*time.Second)
	reg, tpl := sweepTemplate()
	n := newCHNode(t, shortDir(t), "node", reg)
	if err := n.m.Boot(ctx, "parent", tpl); err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	snap, err := n.m.Snapshot(ctx, "parent", vmm.FidelityMemory, false, &bundle)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := n.m.Delete(ctx, "parent"); p != vmm.PresenceGone {
		t.Fatalf("delete parent = %v (%v)", p, err)
	}
	for _, fork := range []string{"fork-1", "fork-2"} {
		if err := n.m.Restore(ctx, fork, bytes.NewReader(bundle.Bytes())); err != nil {
			t.Fatalf("restore %s: %v", fork, err)
		}
		if p, err := n.m.Delete(ctx, fork); p != vmm.PresenceGone {
			t.Fatalf("delete %s = %v (%v)", fork, p, err)
		}
		if _, err := n.v.snapshotDir(snap); err != nil {
			t.Errorf("after %s: the snapshot a fork restored from is gone: %v", fork, err)
		}
	}
}

// TestRestartSweepKeepsOnlyWhatTheStoreHolds (PLT-374, criterion bounded): a herdlet's start removes from vm-snapshots
// what a snapshot or import cut short left (.partial-, .import-), the snapshot whose object is gone, and the snapshot
// no record names (taken by a herdlet before records, or never stored); it keeps the snapshot a stored object holds.
// The actors still resume afterwards from their bundles, the one whose copy was removed too.
func TestRestartSweepKeepsOnlyWhatTheStoreHolds(t *testing.T) {
	ctx := ctxT(t, 20*time.Second)
	root := shortDir(t)
	reg, tpl := sweepTemplate()
	n := newCHNode(t, root, "node", reg)
	store, err := disk.New(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	suspend := func(id string) (vmm.Snapshot, []byte) {
		t.Helper()
		if err := n.m.Boot(ctx, id, tpl); err != nil {
			t.Fatal(err)
		}
		var bundle bytes.Buffer
		snap, err := n.m.Snapshot(ctx, id, vmm.FidelityMemory, true, &bundle)
		if err != nil {
			t.Fatal(err)
		}
		if p, err := n.m.Delete(ctx, id); p != vmm.PresenceGone {
			t.Fatalf("release %s = %v (%v)", id, p, err)
		}
		return snap, bundle.Bytes()
	}
	kept, keptBundle := suspend("kept")
	gone, _ := suspend("gone")
	legacy, legacyBundle := suspend("legacy")
	if _, err := store.Put(ctx, "actors/kept", bytes.NewReader(keptBundle)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, "actors/legacy", bytes.NewReader(legacyBundle)); err != nil {
		t.Fatal(err)
	}
	for _, left := range []string{".partial-kept-0123456789abcdef", ".import-gone-0123456789abcdef-0011223344556677"} {
		if err := os.MkdirAll(filepath.Join(n.snaps, left), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(n.snaps, left, "memory-ranges"), []byte("mem"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// What the herdlet before the restart recorded: the object each snapshot was put under.
	refs := herdlet.SnapshotRefsPath(root)
	b, _ := json.Marshal(map[string][]string{kept.ID: {"actors/kept"}, gone.ID: {"actors/gone"}})
	if err := os.WriteFile(refs, b, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &herdlet.Agent{VMM: n.v, Machines: n.m, Snapshots: store, SnapshotRefs: refs}
	if err := a.SweepSnapshots(ctx, true); err != nil {
		t.Fatalf("start sweep: %v", err)
	}
	if left := snapshotDirs(t, n.snaps); !slices.Equal(left, []string{kept.ID}) {
		t.Errorf("vm-snapshots after a restart: %v; want only %s, the one a stored object holds (not %s, %s)", left, kept.ID, gone.ID, legacy.ID)
	}
	for id, bundle := range map[string][]byte{"kept": keptBundle, "legacy": legacyBundle} {
		if err := n.m.Restore(ctx, id, bytes.NewReader(bundle)); err != nil {
			t.Errorf("resume %s after the restart: %v", id, err)
			continue
		}
		if p, err := n.m.Delete(ctx, id); p != vmm.PresenceGone {
			t.Errorf("delete %s = %v (%v)", id, p, err)
		}
	}
}
