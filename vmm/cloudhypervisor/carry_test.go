//go:build linux

package cloudhypervisor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/herdlet/machine"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
)

// chNode is one Linux host of a pool: Cloud Hypervisor over its own fake process table, its own vm-snapshots
// directory, VM state directory, image cache and shares (a -state of its own, as two herdlets on one host have), and
// the herdlet's machines over them.
type chNode struct {
	host  *fakeHost
	v     *VMM
	m     *machine.Machines
	snaps string
}

func newCHNode(t *testing.T, root, name string, reg *imagetest.Registry) chNode {
	t.Helper()
	h := newFakeHost(t)
	n := chNode{host: h, snaps: filepath.Join(root, name, "vm-snapshots")}
	n.v = New(Config{Binary: h.chBin, Virtiofsd: h.fsBin, StateDir: filepath.Join(root, name, "vms"), SnapshotDir: n.snaps,
		CPU: X86CPU{CPUID: tigerLake.cpuid}, Processes: h, ExitTimeout: 200 * time.Millisecond})
	m, err := machine.New(machine.Config{VMM: n.v, ShareDir: filepath.Join(root, name, "shares"), Base: vmm.Spec{Kernel: "/guest/bzImage"},
		Images: &image.Cache{Dir: filepath.Join(root, name, "images"), Source: reg, Builder: &imagetest.Builder{},
			Platform: image.Platform{OS: "linux", Architecture: "amd64"}}})
	if err != nil {
		t.Fatal(err)
	}
	n.m = m
	return n
}

// TestMemorySnapshotMovesToAnotherNodeAfterTheFirstLostIt (criteria cross-node, loss): an actor suspended at MEMORY on
// node-1 restores at MEMORY on node-2 of the same family from the bundle alone — after node-1's vm-snapshots
// directory is gone. Cloud Hypervisor's restore on node-2 reads the snapshot files node-1 wrote, carried in the bundle.
// Its counterexample is the bundle before PLT-212 (volumes only): node-2's restore fails with `snapshot ...: not in
// .../vm-snapshots: vmm: not found`, as fork2 did on the supervisor's two-VM Nomad walk.
func TestMemorySnapshotMovesToAnotherNodeAfterTheFirstLostIt(t *testing.T) {
	ctx := ctxT(t, 20*time.Second)
	root := shortDir(t)
	reg := imagetest.NewRegistry()
	reg.Push("team/agent", "1", imagetest.Manifest{Platform: image.Platform{OS: "linux", Architecture: "amd64"},
		Config: image.Config{Cmd: []string{"/bin/run"}},
		Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "bin/run", Body: "run", Mode: 0o755})}})
	tpl := link.Template{UID: "tpl-1", Volumes: []string{"work"}, Containers: []link.Container{{Name: "main", Image: "registry.example/team/agent:1",
		VolumeMounts: []link.VolumeMount{{Name: "work", MountPath: "/work"}}}}}
	node1, node2 := newCHNode(t, root, "node-1", reg), newCHNode(t, root, "node-2", reg)

	if err := node1.m.Boot(ctx, "a1", tpl); err != nil {
		t.Fatalf("boot on node-1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(node1.m.ShareDir("a1"), "work", "note"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	snap, err := node1.m.Snapshot(ctx, "a1", vmm.FidelityMemory, true, &bundle)
	if err != nil {
		t.Fatalf("MEMORY snapshot on node-1: %v", err)
	}
	if snap.Fidelity != vmm.FidelityMemory {
		t.Fatalf("snapshot at %v", snap.Fidelity)
	}
	// node-1 is lost: nothing of its state remains, its snapshot directory included.
	if err := os.RemoveAll(filepath.Join(root, "node-1")); err != nil {
		t.Fatal(err)
	}
	if err := node2.m.Restore(ctx, "a1", &bundle); err != nil {
		t.Fatalf("MEMORY restore on node-2 from the bundle: %v", err)
	}
	node2.host.mu.Lock()
	calls := slices.Clone(node2.host.calls)
	restored := node2.host.restored
	node2.host.mu.Unlock()
	if !slices.Contains(calls, epRestore) || slices.Contains(calls, epBoot) {
		t.Errorf("node-2's Cloud Hypervisor was called %v; want a vm.restore and no vm.boot", calls)
	}
	for f, want := range map[string]string{"memory-ranges": "mem", "state.json": "{}"} {
		if got := restored[f]; got != want {
			t.Errorf("node-2's restore read %s %q; want node-1's %q", f, got, want)
		}
	}
	// The actor's resume used node-2's copy up (PLT-374): the bundle in the store holds the same files.
	if left := snapshotDirs(t, node2.snaps); len(left) != 0 {
		t.Errorf("node-2's vm-snapshots after the actor resumed: %v", left)
	}
	if b, err := os.ReadFile(filepath.Join(node2.m.ShareDir("a1"), "work", "note")); err != nil || string(b) != "kept" {
		t.Errorf("volume on node-2: %q, %v", b, err)
	}
	if p, err := node2.m.Delete(ctx, "a1"); p != vmm.PresenceGone {
		t.Errorf("Delete on node-2 = %v (%v)", p, err)
	}
}

// TestImportPublishesOnlyTheReportedSnapshot: files whose record names another snapshot, or that carry no record,
// leave nothing a restore could find; the reported snapshot's files publish it, and a second import keeps it.
func TestImportPublishesOnlyTheReportedSnapshot(t *testing.T) {
	ctx := ctxT(t, 10*time.Second)
	r := newRig(t)
	here := r.vmm("a", tigerLake)
	if _, err := here.Boot(ctx, r.spec("m1")); err != nil {
		t.Fatal(err)
	}
	snap, err := here.Snapshot(ctx, "m1", vmm.FidelityMemory)
	if err != nil {
		t.Fatal(err)
	}
	files, err := here.ExportSnapshot(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	feed := func(fs []vmm.SnapshotFile) func() (vmm.SnapshotFile, io.Reader, error) {
		return func() (vmm.SnapshotFile, io.Reader, error) {
			if len(fs) == 0 {
				return vmm.SnapshotFile{}, nil, io.EOF
			}
			f := fs[0]
			fs = fs[1:]
			rc, err := f.Open()
			if err != nil {
				return f, nil, err
			}
			t.Cleanup(func() { _ = rc.Close() })
			return f, rc, nil
		}
	}
	there := New(Config{Binary: r.host.chBin, Virtiofsd: r.host.fsBin, StateDir: filepath.Join(r.root, "a"),
		SnapshotDir: filepath.Join(r.root, "there"), CPU: X86CPU{CPUID: tigerLake.cpuid}, Processes: r.host})
	other := snap
	other.VMID = "m2"
	var noRecord []vmm.SnapshotFile
	for _, f := range files {
		if f.Name != metaFile {
			noRecord = append(noRecord, f)
		}
	}
	for name, try := range map[string]func() error{
		"another snapshot's record": func() error { return there.ImportSnapshot(ctx, other, feed(files)) },
		"no record":                 func() error { return there.ImportSnapshot(ctx, snap, feed(noRecord)) },
	} {
		if err := try(); err == nil {
			t.Errorf("import with %s succeeded", name)
		}
		if ents, _ := os.ReadDir(filepath.Join(r.root, "there")); len(ents) != 0 {
			t.Errorf("import with %s left %v", name, ents)
		}
	}
	for i := range 2 {
		if err := there.ImportSnapshot(ctx, snap, feed(files)); err != nil {
			t.Fatalf("import %d: %v", i+1, err)
		}
	}
	if _, err := there.snapshotDir(snap); err != nil {
		t.Errorf("after the import: %v", err)
	}
}

// TestFailedRestoreKeepsCloudHypervisorsWords: a Cloud Hypervisor that dies during vm.restore (the supervisor's
// second Nomad walk saw only `vm.restore ... EOF`, and the VM's directory, with CH's log in it, was gone) leaves its
// words behind: the error says how the process ended and how its log ends, the herdlet's log holds the tail of every
// log of the VM (CH's stdout and stderr, virtiofsd's, the serial console), and only then is the directory removed.
func TestFailedRestoreKeepsCloudHypervisorsWords(t *testing.T) {
	ctx := ctxT(t, 10*time.Second)
	r := newRig(t)
	v := r.vmm("a", tigerLake)
	if _, err := v.Boot(ctx, r.spec("m1")); err != nil {
		t.Fatal(err)
	}
	snap, err := v.Snapshot(ctx, "m1", vmm.FidelityMemory)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := v.Delete(ctx, "m1"); p != vmm.PresenceGone {
		t.Fatalf("Delete = %v (%v)", p, err)
	}
	const words = "thread 'vmm' panicked at vmm/src/vm.rs:1234: restore of device virtio-fs failed"
	r.host.mu.Lock()
	r.host.die[epRestore] = words
	r.host.mu.Unlock()
	var herdletLog bytes.Buffer
	log.SetOutput(&herdletLog)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	_, err = v.Restore(ctx, snap, r.spec("m1"))
	log.SetOutput(os.Stderr)
	if err == nil {
		t.Fatal("restore by a Cloud Hypervisor that died succeeded")
	}
	for _, want := range []string{"signal: aborted", words} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("restore error %q does not say %q", err, want)
		}
	}
	if !strings.Contains(herdletLog.String(), "cloud-hypervisor.log ends:\n"+words) {
		t.Errorf("the herdlet log does not hold cloud-hypervisor.log:\n%s", herdletLog.String())
	}
	if _, err := os.Stat(v.vmDir("m1")); !os.IsNotExist(err) {
		t.Errorf("the failed VM's directory: %v, want removed after it was logged", err)
	}
}

// TestRewriteConfigKeepsWhatItDoesNotKnow: the rewrite sets the host paths and devices to the restoring VM's and keeps
// every other field Cloud Hypervisor saved; a snapshot with other devices than the VM is refused.
func TestRewriteConfigKeepsWhatItDoesNotKnow(t *testing.T) {
	saved := `{"payload":{"kernel":"/var/lib/nomad/alloc/1/herdlet/local/kernel","cmdline":"console=ttyS0"},` +
		`"memory":{"size":536870912,"shared":true,"hugepages":false},"rng":{"src":"/dev/urandom"},` +
		`"disks":[{"path":"/a/images/erofs/x.erofs","readonly":true,"id":"_disk0"}],` +
		`"fs":[{"tag":"herd","socket":"/a/vms/vms/v/fs-herd.sock","num_queues":1,"queue_size":1024,"id":"_fs1"}],` +
		`"net":[{"tap":"herd7","mac":"02:68:64:00:00:07","host_mac":"aa:bb:cc:dd:ee:ff","id":"_net2"}],` +
		`"vsock":{"cid":3,"socket":"/a/vms/vms/v/vsock.sock"},"serial":{"mode":"File","file":"/a/vms/vms/v/serial.log"}}`
	want := vmConfig{Payload: payloadConfig{Kernel: "/b/kernel", Initramfs: "/b/initrd"},
		Disks: []diskConfig{{Path: "/b/images/erofs/x.erofs"}}, Fs: []fsConfig{{Tag: "herd", Socket: "/b/vms/vms/v/fs-herd.sock"}},
		Net: []netConfig{{Tap: "herd7", MAC: "02:68:64:00:00:07"}}, Vsock: &vsockConfig{Socket: "/b/vms/vms/v/vsock.sock"},
		Serial: &consoleConfig{Mode: "File", File: "/b/vms/vms/v/serial.log"}}
	out, err := rewriteConfig([]byte(saved), want)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`"kernel":"/b/kernel"`, `"initramfs":"/b/initrd"`, `"cmdline":"console=ttyS0"`, `"size":536870912`,
		`"rng":{"src":"/dev/urandom"}`, `"path":"/b/images/erofs/x.erofs"`, `"id":"_disk0"`, `"socket":"/b/vms/vms/v/fs-herd.sock"`,
		`"host_mac":"aa:bb:cc:dd:ee:ff"`, `"socket":"/b/vms/vms/v/vsock.sock"`, `"file":"/b/vms/vms/v/serial.log"`} {
		if !strings.Contains(string(out), s) {
			t.Errorf("rewritten config lacks %s:\n%s", s, out)
		}
	}
	if strings.Contains(string(out), "/a/") {
		t.Errorf("rewritten config still names the taking host's paths:\n%s", out)
	}
	want.Disks = append(want.Disks, diskConfig{Path: "/b/other"})
	if _, err := rewriteConfig([]byte(saved), want); !errors.Is(err, vmm.ErrInvalidSpec) {
		t.Errorf("a VM with another disk set: %v, want ErrInvalidSpec", err)
	}
}

// TestSilentDeathSaysSo: a Cloud Hypervisor killed by a signal during vm.restore writes nothing (the supervisor's third
// Nomad walk logged only virtiofsd's tail). The herdlet log and the error then say how the process ended, that its
// stdout and stderr were empty, and what the kernel logged about the pid; and a delete of the VM, the one way herd
// itself ends one, would have been logged.
func TestSilentDeathSaysSo(t *testing.T) {
	ctx := ctxT(t, 10*time.Second)
	r := newRig(t)
	v := r.vmm("a", tigerLake)
	if _, err := v.Boot(ctx, r.spec("m1")); err != nil {
		t.Fatal(err)
	}
	snap, err := v.Snapshot(ctx, "m1", vmm.FidelityMemory)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := v.Delete(ctx, "m1"); p != vmm.PresenceGone {
		t.Fatalf("Delete = %v (%v)", p, err)
	}
	var asked []int
	v.cfg.KernelLog = func(pid int) []string {
		asked = append(asked, pid)
		return []string{fmt.Sprintf("Out of memory: Killed process %d (cloud-hyperviso)", pid)}
	}
	r.host.mu.Lock()
	r.host.die[epRestore] = silentDeath
	r.host.mu.Unlock()
	var herdletLog bytes.Buffer
	log.SetOutput(&herdletLog)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	_, err = v.Restore(ctx, snap, r.spec("m1"))
	log.SetOutput(os.Stderr)
	if err == nil {
		t.Fatal("restore by a Cloud Hypervisor that died succeeded")
	}
	for _, want := range []string{"ended: signal: killed", "wrote nothing to its stdout or stderr", "kernel: Out of memory: Killed process"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("restore error %q does not say %q", err, want)
		}
	}
	for _, want := range []string{"ended: signal: killed", "cloud-hypervisor.log (its stdout and stderr) is empty", "the kernel logged:\nOut of memory"} {
		if !strings.Contains(herdletLog.String(), want) {
			t.Errorf("the herdlet log does not say %q:\n%s", want, herdletLog.String())
		}
	}
	if len(asked) != 1 {
		t.Errorf("the kernel log was read for pids %v, want the dead VMM's", asked)
	}
	if strings.Contains(herdletLog.String(), "delete m1 requested") {
		t.Errorf("the failed restore was logged as a delete:\n%s", herdletLog.String())
	}
}

// TestMemoryForkRestoresUnderItsOwnID: a MEMORY snapshot restores as a VM of another id while its source still runs
// (a fork), on the fork's own sockets, share and TAP: the config Cloud Hypervisor restores from names them, not the
// source's. Before PLT-212 round 3 a MEMORY snapshot restored only under the source's id, so a fork ran under its
// parent's VM id and work on the parent's id on that host reached the fork's VM.
func TestMemoryForkRestoresUnderItsOwnID(t *testing.T) {
	ctx := ctxT(t, 10*time.Second)
	r := newRig(t)
	v := r.vmm("a", tigerLake)
	src := r.spec("parent")
	src.NIC = &vmm.NIC{TAP: "herd7", MAC: "02:68:64:00:00:07"}
	if _, err := v.Boot(ctx, src); err != nil {
		t.Fatal(err)
	}
	snap, err := v.Snapshot(ctx, "parent", vmm.FidelityMemory)
	if err != nil {
		t.Fatal(err)
	}
	fork := r.spec("fork-1")
	fork.NIC = &vmm.NIC{TAP: "herd8", MAC: "02:68:64:00:00:07"}
	if _, err := v.Restore(ctx, snap, fork); err != nil {
		t.Fatalf("MEMORY restore as fork-1 while its source runs: %v", err)
	}
	r.host.mu.Lock()
	nets := slices.Clone(r.host.nets)
	r.host.mu.Unlock()
	if !slices.Contains(nets, "vm.restore tap=herd8,mac=02:68:64:00:00:07") {
		t.Errorf("Cloud Hypervisor restored with net devices %v, want the fork's TAP herd8", nets)
	}
	for _, id := range []string{"parent", "fork-1"} {
		if p, err := v.Inspect(ctx, id); p != vmm.PresencePresent {
			t.Errorf("%s: %v (%v), want both running", id, p, err)
		}
	}
}
