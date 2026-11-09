//go:build linux

package cloudhypervisor

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/link"
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
