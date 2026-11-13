package herdlet

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/everdict/herd/herdlet/machine"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/snapstore/bundlecache"
	"github.com/everdict/herd/snapstore/disk"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
)

// diskVMM is the fake VMM with the snapshot files a real one leaves on its host: each snapshot's directory holds a
// memory-ranges file of the VM's whole memory, mostly zero as an idle guest's is, beside the fake's record.
type diskVMM struct {
	*fake.Fake
	dir    string
	memory int64
}

const memoryFile = "memory-ranges"

func (v *diskVMM) Snapshot(ctx context.Context, id string, f vmm.Fidelity) (vmm.Snapshot, error) {
	s, err := v.Fake.Snapshot(ctx, id, f)
	if err != nil {
		return s, err
	}
	b := make([]byte, v.memory)
	// 1 MiB of data the guest used, in its first pages.
	if _, err := rand.Read(b[:1<<20]); err != nil {
		return s, err
	}
	if err := os.MkdirAll(filepath.Join(v.dir, s.ID), 0o700); err != nil {
		return s, err
	}
	return s, os.WriteFile(filepath.Join(v.dir, s.ID, memoryFile), b, 0o600)
}

func (v *diskVMM) ExportSnapshot(ctx context.Context, s vmm.Snapshot) ([]vmm.SnapshotFile, error) {
	files, err := v.Fake.ExportSnapshot(ctx, s)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(v.dir, s.ID, memoryFile)
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	return append(files, vmm.SnapshotFile{Name: memoryFile, Size: fi.Size(), Open: func() (io.ReadCloser, error) { return os.Open(p) }}), nil
}

func (v *diskVMM) ImportSnapshot(ctx context.Context, s vmm.Snapshot, next func() (vmm.SnapshotFile, io.Reader, error)) error {
	return v.Fake.ImportSnapshot(ctx, s, func() (vmm.SnapshotFile, io.Reader, error) {
		for {
			f, r, err := next()
			if err != nil || f.Name != memoryFile {
				return f, r, err
			}
			if err := os.MkdirAll(filepath.Join(v.dir, s.ID), 0o700); err != nil {
				return f, nil, err
			}
			out, err := os.Create(filepath.Join(v.dir, s.ID, memoryFile))
			if err != nil {
				return f, nil, err
			}
			_, err = io.CopyN(out, r, f.Size)
			out.Close()
			if err != nil {
				return f, nil, err
			}
		}
	})
}

func (v *diskVMM) RemoveSnapshot(ctx context.Context, id string) error {
	if err := v.Fake.RemoveSnapshot(ctx, id); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(v.dir, id))
}

// diskBytes is how many bytes the files under dir take on disk.
func diskBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			n += fi.Size()
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return n
}

// TestSnapshotLeavesOneLocalCopy (PLT-591): once a MEMORY snapshot's bundle is in the store and kept in the node's
// bundle cache, the node holds its memory once: the VMM's own snapshot directory, as large as the VM's memory, is
// gone, and the snapshot restores here from the cached bundle, memory and all. It logs the bytes on disk for the
// snapshot.
func TestSnapshotLeavesOneLocalCopy(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	caps := vmm.Capabilities{Platform: vmm.PlatformCloudHypervisor, Arch: "amd64", SuspendMaxFidelity: vmm.FidelityMemory, RestoreFamily: "microvm-ch-amd64-test"}
	v := &diskVMM{Fake: fake.New(caps, nil), dir: filepath.Join(dir, "vmm-snapshots"), memory: 64 << 20}
	reg := imagetest.NewRegistry()
	reg.Push("team/agent", "1", imagetest.Manifest{Platform: image.Platform{OS: "linux", Architecture: "amd64"},
		Config: image.Config{Cmd: []string{"/bin/run"}}, Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "bin/run", Body: "run", Mode: 0o755})}})
	m, err := machine.New(machine.Config{VMM: v, ShareDir: filepath.Join(dir, "shares"), Base: vmm.Spec{Kernel: "vmlinux"},
		Images: &image.Cache{Dir: filepath.Join(dir, "images"), Source: reg, Builder: &imagetest.Builder{}, Platform: image.Platform{OS: "linux", Architecture: "amd64"}}})
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := disk.New(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	bundles, err := bundlecache.New(filepath.Join(dir, "bundles"), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{Worker: "w1", VMM: v, Machines: m, Snapshots: snaps, Bundles: bundles}
	tpl := link.Template{UID: "tpl-1", Containers: []link.Container{{Name: "agent", Image: "registry.example/team/agent:1"}}}
	if o := a.doMachine(ctx, link.Work{Op: link.OpBoot, VM: "vm-1", Spec: &tpl}); o.Result != link.ResultDone {
		t.Fatalf("boot: %+v", o)
	}
	o := a.doMachine(ctx, link.Work{Op: link.OpSnapshot, VM: "vm-1", Fidelity: vmm.FidelityMemory, Cause: link.CauseSuspend, SnapshotKey: "s/a1/1"})
	if o.Result != link.ResultDone || o.Snapshot == nil {
		t.Fatalf("snapshot: %+v", o)
	}
	vmmBytes, cacheBytes := diskBytes(t, v.dir), diskBytes(t, filepath.Join(dir, "bundles"))
	t.Logf("one MEMORY snapshot of a %d-byte VM on this node: VMM snapshot files %d B, bundle cache %d B", v.memory, vmmBytes, cacheBytes)
	if vmmBytes != 0 {
		t.Errorf("the VMM's snapshot files hold %d bytes beside the kept bundle, want none", vmmBytes)
	}
	if cacheBytes == 0 || cacheBytes > 4<<20 {
		t.Errorf("the bundle cache holds %d bytes, want the snapshot's bundle (1 MiB of data, compressed)", cacheBytes)
	}
	if ids, _ := v.LocalSnapshots(ctx); len(ids) != 0 {
		t.Errorf("the VMM still holds snapshots %v", ids)
	}
	// herd-api deletes the suspended VM, then resumes it here.
	if o := a.doMachine(ctx, link.Work{Op: link.OpDelete, VM: "vm-1", Cause: link.CauseSuspend}); o.Result != link.ResultDone {
		t.Fatalf("delete: %+v", o)
	}
	if o := a.doMachine(ctx, link.Work{Op: link.OpRestore, VM: "vm-1", Snapshot: o.Snapshot}); o.Result != link.ResultDone {
		t.Fatalf("restore from the kept bundle: %+v", o)
	}
}
