//go:build linux

package cloudhypervisor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/everdict/herd/vmm"
)

// fakeCgroupfs is a cgroup v2 hierarchy in a temp dir: the root offers controllers, and rmdir removes a cgroup's
// control files with it unless its cgroup.procs names a process (EBUSY), as cgroupfs does.
func fakeCgroupfs(t *testing.T, controllers string) (string, func(string) error) {
	root := t.TempDir()
	if controllers != "" {
		if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte(controllers+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rmdir := func(dir string) error {
		if procs, _ := os.ReadFile(filepath.Join(dir, "cgroup.procs")); len(strings.TrimSpace(string(procs))) > 0 {
			return syscall.EBUSY
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return syscall.ENOENT
			}
			return err
		}
		for _, e := range ents {
			if e.IsDir() {
				return syscall.EBUSY
			}
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
		return syscall.Rmdir(dir)
	}
	return root, rmdir
}

func readTrim(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// TestMemoryRestoreUnderAConfinedHerdlet is PLT-214: the herdlet's own cgroup is limited to 300 MB (Nomad's default
// task memory), and a MEMORY restore of a 512 MiB guest charges all of its memory at once. The VM's cloud-hypervisor
// and virtiofsd start in a herd-owned cgroup of the VM's own, limited to its guest memory and overhead, so the restore
// is charged there and the herdlet's limit never sees it. Started in the herdlet's cgroup (the base), the memcg OOM
// killer SIGKILLs Cloud Hypervisor mid vm.restore.
func TestMemoryRestoreUnderAConfinedHerdlet(t *testing.T) {
	r := newRig(t)
	r.host.herdletMemMax = 300 << 20
	root, rmdir := fakeCgroupfs(t, "cpuset cpu io memory pids")
	cg := CgroupV2{Root: root, Parent: "herd", rmdir: rmdir}
	v := New(Config{Binary: r.host.chBin, Virtiofsd: r.host.fsBin, StateDir: filepath.Join(r.root, "a"),
		SnapshotDir: filepath.Join(r.root, "snapshots"), CPU: X86CPU{CPUID: tigerLake.cpuid}, Processes: r.host,
		ExitTimeout: 200 * time.Millisecond, Cgroups: cg})
	ctx := ctxT(t, 10*time.Second)
	if _, err := v.Boot(ctx, r.spec("src")); err != nil {
		t.Fatal(err)
	}
	snap, err := v.Snapshot(ctx, "src", vmm.FidelityMemory)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := v.Delete(ctx, "src"); p != vmm.PresenceGone {
		t.Fatalf("delete src: %v (%v)", p, err)
	}
	if _, err := v.Restore(ctx, snap, r.spec("fork")); err != nil {
		t.Fatalf("a MEMORY restore under a herdlet confined to 300 MB failed: %v", err)
	}
	if gone, why, err := v.Exited(ctx, "fork"); gone || err != nil {
		t.Fatalf("the restored VM exited: %s (%v)", why, err)
	}
	r.host.mu.Lock()
	inHerdlet := r.host.charged[""]
	cgs := map[string][]string{}
	for k, s := range r.host.startCgroups {
		cgs[k] = append([]string(nil), s...)
	}
	r.host.mu.Unlock()
	if inHerdlet != 0 {
		t.Errorf("%d bytes of VM memory are charged to the herdlet's cgroup", inHerdlet)
	}
	for bin, want := range map[string][]string{
		r.host.chBin: {filepath.Join(root, "herd", "src"), filepath.Join(root, "herd", "fork")},
		r.host.fsBin: {filepath.Join(root, "herd", "src"), filepath.Join(root, "herd", "fork")},
	} {
		if strings.Join(cgs[bin], ",") != strings.Join(want, ",") {
			t.Errorf("%s started in cgroups %q, want each VM's own %q", filepath.Base(bin), cgs[bin], want)
		}
	}
	// The VM's cgroup accounts for it and limits it to its guest memory and the overhead.
	if got, want := readTrim(t, filepath.Join(root, "herd", "fork", "memory.max")), strconv.Itoa((512+DefaultMemoryOverheadMiB)<<20); got != want {
		t.Errorf("fork's memory.max = %s, want %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(root, "herd", "src")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the deleted VM's cgroup is still there (%v)", err)
	}
	if p, err := v.Delete(ctx, "fork"); p != vmm.PresenceGone {
		t.Fatalf("delete fork: %v (%v)", p, err)
	}
	if _, err := os.Stat(filepath.Join(root, "herd", "fork")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the deleted VM's cgroup is still there (%v)", err)
	}
}

// TestFailedStartRemovesItsCgroup: a VM whose start fails leaves no cgroup behind, and one whose cgroup cannot be
// made is not started at all.
func TestFailedStartRemovesItsCgroup(t *testing.T) {
	r := newRig(t)
	r.host.die[epBoot] = "boom"
	root, rmdir := fakeCgroupfs(t, "memory")
	v := New(Config{Binary: r.host.chBin, Virtiofsd: r.host.fsBin, StateDir: filepath.Join(r.root, "a"),
		CPU: X86CPU{CPUID: tigerLake.cpuid}, Processes: r.host, ExitTimeout: 200 * time.Millisecond,
		Cgroups: CgroupV2{Root: root, rmdir: rmdir}})
	ctx := ctxT(t, 10*time.Second)
	if _, err := v.Boot(ctx, r.spec("x")); err == nil {
		t.Fatal("boot succeeded")
	}
	if _, err := os.Stat(filepath.Join(root, DefaultCgroupParent, "x")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the failed VM's cgroup is still there (%v)", err)
	}

	r2 := newRig(t)
	noV2 := New(Config{Binary: r2.host.chBin, Virtiofsd: r2.host.fsBin, StateDir: filepath.Join(r2.root, "a"),
		CPU: X86CPU{CPUID: tigerLake.cpuid}, Processes: r2.host, ExitTimeout: 200 * time.Millisecond,
		Cgroups: CgroupV2{Root: t.TempDir()}})
	if _, err := noV2.Boot(ctx, r2.spec("y")); err == nil || !strings.Contains(err.Error(), "cgroup v2") {
		t.Fatalf("boot without a cgroup v2 hierarchy: %v", err)
	}
	if _, starts := r2.host.callCount(); starts != 0 {
		t.Errorf("%d processes started without a cgroup for them", starts)
	}
}

// TestCgroupV2 drives CgroupV2 over a fake cgroupfs: the memory controller is enabled down to herd's cgroup, each VM's
// cgroup gets its limit, a leftover is taken again, and removal waits out a cgroup still populated.
func TestCgroupV2(t *testing.T) {
	root, rmdir := fakeCgroupfs(t, "cpuset cpu io memory hugetlb pids")
	c := CgroupV2{Root: root, Parent: "herd", rmdir: rmdir}
	dir, err := c.Create("vm-1", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(root, "herd", "vm-1") {
		t.Errorf("dir = %s", dir)
	}
	for f, want := range map[string]string{
		filepath.Join(root, "cgroup.subtree_control"):         "+memory",
		filepath.Join(root, "herd", "cgroup.subtree_control"): "+memory",
		filepath.Join(dir, "memory.max"):                      "1073741824",
	} {
		if got := readTrim(t, f); got != want {
			t.Errorf("%s = %q, want %q", f, got, want)
		}
	}
	if _, err := c.Create("vm-1", 0); err != nil {
		t.Fatalf("taking a leftover cgroup again: %v", err)
	}
	if got := readTrim(t, filepath.Join(dir, "memory.max")); got != "max" {
		t.Errorf("unlimited memory.max = %q", got)
	}
	if _, err := c.Create("../escape", 0); err == nil {
		t.Error("a cgroup outside herd's was made")
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove("vm-1"); !errors.Is(err, syscall.EBUSY) {
		t.Errorf("removing a populated cgroup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove("vm-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove("vm-1"); err != nil {
		t.Errorf("removing an absent cgroup: %v", err)
	}

	noMem, _ := fakeCgroupfs(t, "cpu pids")
	if _, err := (CgroupV2{Root: noMem}).Create("vm-1", 0); err == nil || !strings.Contains(err.Error(), "memory controller") {
		t.Errorf("a hierarchy without the memory controller: %v", err)
	}
}

// TestCgroupV2Check: doctor's check makes and removes a probe, and refuses a herd cgroup inside the herdlet's own (its
// VMs would be charged to the herdlet's limit, which is PLT-214 again).
func TestCgroupV2Check(t *testing.T) {
	root, rmdir := fakeCgroupfs(t, "memory")
	for _, self := range []string{"/", "/nomad.slice/share.slice/a1b2.herdlet.scope", "/system.slice/herdlet.service", ""} {
		if err := (CgroupV2{Root: root, rmdir: rmdir}).Check(self); err != nil {
			t.Errorf("herdlet in %q: %v", self, err)
		}
	}
	ents, _ := os.ReadDir(filepath.Join(root, DefaultCgroupParent))
	if len(ents) != 1 || ents[0].Name() != "cgroup.subtree_control" {
		t.Errorf("the probe was left behind: %v", ents)
	}
	inside := CgroupV2{Root: root, Parent: "nomad.slice/share.slice/a1b2.herdlet.scope/herd", rmdir: rmdir}
	if err := inside.Check("/nomad.slice/share.slice/a1b2.herdlet.scope"); err == nil || !strings.Contains(err.Error(), "inside") {
		t.Errorf("a herd cgroup inside the herdlet's: %v", err)
	}
	if err := (CgroupV2{Root: t.TempDir()}).Check("/"); err == nil {
		t.Error("a hierarchy that is not cgroup v2 passed")
	}
	got, err := ParseProcCgroup("12:memory:/old\n0::/nomad.slice/share.slice/a1b2.herdlet.scope\n")
	if err != nil || got != "/nomad.slice/share.slice/a1b2.herdlet.scope" {
		t.Errorf("ParseProcCgroup = %q, %v", got, err)
	}
	if _, err := ParseProcCgroup("4:memory:/x\n"); err == nil {
		t.Error("a cgroup v1-only file parsed")
	}
}

// TestExecProcessesInCgroup starts sleep(1) in a real cgroup of its own and reads it back from /proc. It needs root on
// a cgroup v2 host: HERD_CGROUP_REAL=1.
func TestExecProcessesInCgroup(t *testing.T) {
	if os.Getenv("HERD_CGROUP_REAL") != "1" {
		t.Skip("set HERD_CGROUP_REAL=1 (as root on a cgroup v2 host) to start a process in a real VM cgroup")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	parent := "herd-test-" + strconv.Itoa(os.Getpid())
	c := CgroupV2{Parent: parent}
	t.Cleanup(func() { _ = syscall.Rmdir(filepath.Join(DefaultCgroupRoot, parent)) })
	dir, err := c.Create("vm-1", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ExecProcesses{}.Start(ProcSpec{Path: sleep, Args: []string{"30"}, Log: filepath.Join(t.TempDir(), "log"), Cgroup: dir})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(p.Pid()) + "/cgroup")
	_ = p.Signal(syscall.SIGKILL)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := ParseProcCgroup(string(b)); got != "/"+parent+"/vm-1" {
		t.Errorf("the process is in %q, want /%s/vm-1", got, parent)
	}
	for i := 0; i < 200; i++ {
		if gone, _ := p.Exited(); gone {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.Remove("vm-1"); err != nil {
		t.Error(err)
	}
}
