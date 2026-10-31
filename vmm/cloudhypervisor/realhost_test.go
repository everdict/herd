//go:build linux

package cloudhypervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/vmmtest"
)

// TestRealHostContract runs the vmm contract against real cloud-hypervisor and virtiofsd on a KVM host. It runs only
// with HERD_CH_REAL=1 and these paths set:
//
//	HERD_CH_BIN        cloud-hypervisor v54.0 (static release binary)
//	HERD_VIRTIOFSD_BIN virtiofsd >= 1.11
//	HERD_KERNEL        guest kernel (bzImage on amd64, Image on arm64; ch-release-v7.2.8-20261009)
//	HERD_INITRAMFS     an initramfs whose /init keeps running (it must not exit, or the guest panics)
//
// Optional: HERD_CMDLINE (default DefaultCmdline: "console=ttyS0 panic=-1" on amd64, "console=ttyAMA0 panic=-1" on arm64),
// HERD_ROOTFS (a raw disk image to attach), HERD_VIRTIOFSD_ARGS (extra virtiofsd arguments, space-separated;
// default --sandbox=none when not root). The contract's Faults subtest skips: real Cloud Hypervisor cannot be made to
// misbehave on demand, and the fake-server contract covers those paths in every go test.
func TestRealHostContract(t *testing.T) {
	if os.Getenv("HERD_CH_REAL") != "1" {
		t.Skip("set HERD_CH_REAL=1 with HERD_CH_BIN, HERD_VIRTIOFSD_BIN, HERD_KERNEL and HERD_INITRAMFS to run the contract against real Cloud Hypervisor on a KVM host")
	}
	paths := map[string]string{}
	for _, k := range []string{"HERD_CH_BIN", "HERD_VIRTIOFSD_BIN", "HERD_KERNEL", "HERD_INITRAMFS"} {
		paths[k] = os.Getenv(k)
		if paths[k] == "" {
			t.Fatalf("HERD_CH_REAL=1 but %s is not set", k)
		}
		if _, err := os.Stat(paths[k]); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Fatalf("HERD_CH_REAL=1 but this host has no KVM: %v", err)
	}
	cpu := HostCPU()
	cmdline := os.Getenv("HERD_CMDLINE")
	if cmdline == "" {
		cmdline = DefaultCmdline(cpu.Arch())
	}
	fsArgs := strings.Fields(os.Getenv("HERD_VIRTIOFSD_ARGS"))
	if len(fsArgs) == 0 && os.Geteuid() != 0 {
		fsArgs = []string{"--sandbox=none"}
	}
	root := shortDir(t)
	volumes := filepath.Join(root, "volumes")
	if err := os.MkdirAll(volumes, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := func(id string) vmm.Spec {
		s := vmm.Spec{ID: id, Kernel: paths["HERD_KERNEL"], Initrd: paths["HERD_INITRAMFS"], Cmdline: cmdline,
			VCPUs: 1, MemoryMiB: 512, Shares: []vmm.Share{{Tag: "volumes", HostPath: volumes}}}
		if r := os.Getenv("HERD_ROOTFS"); r != "" {
			s.Rootfs = []string{r}
		}
		return s
	}
	var n int
	vmmtest.Run(t, vmmtest.Harness{
		New: func(t *testing.T) vmm.VMM {
			n++
			dir := filepath.Join(root, fmt.Sprintf("h%d", n))
			v := New(Config{Binary: paths["HERD_CH_BIN"], Virtiofsd: paths["HERD_VIRTIOFSD_BIN"], VirtiofsdArgs: fsArgs,
				StateDir: dir, CPU: cpu})
			t.Cleanup(func() {
				ents, _ := os.ReadDir(filepath.Join(dir, "vms"))
				for _, e := range ents {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					p, err := v.Delete(ctx, e.Name())
					cancel()
					if p != vmm.PresenceGone {
						t.Errorf("cleanup: %s is %v (%v)", e.Name(), p, err)
					}
				}
			})
			return v
		},
		Spec:    spec,
		Timeout: 60 * time.Second,
	})
}

// TestRealHostConfinedHerdlet is PLT-214 on a KVM host: a herdlet confined to a 200 MB memory cgroup (as Nomad confines
// a task to its memory, 300 MB by default) boots a 512 MiB VM, takes a MEMORY snapshot and restores it as a fork. Each
// VM's cloud-hypervisor and virtiofsd run in herd's cgroup of the VM's own, never in the herdlet's, and the herdlet's
// cgroup sees no OOM kill. With -vm-cgroup off (the base) the restore is OOM-killed. It needs HERD_CH_REAL=1 (the
// paths TestRealHostContract names) and root on a cgroup v2 host.
func TestRealHostConfinedHerdlet(t *testing.T) {
	if os.Getenv("HERD_CH_REAL") != "1" {
		t.Skip("set HERD_CH_REAL=1 with HERD_CH_BIN, HERD_VIRTIOFSD_BIN, HERD_KERNEL and HERD_INITRAMFS, as root on a cgroup v2 KVM host")
	}
	if os.Getuid() != 0 {
		t.Fatal("HERD_CH_REAL=1 but this test confines a herdlet to a cgroup, which needs root")
	}
	parent := "herd-test-" + strconv.Itoa(os.Getpid())
	if os.Getenv("HERD_CONFINED_CHILD") == "" {
		// The herdlet half runs in a child of this test binary, started inside the small cgroup.
		confined := CgroupV2{Parent: parent + "-confined"}
		dir, err := confined.Create("herdlet", 200<<20)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = confined.Remove("herdlet")
			_ = syscall.Rmdir(filepath.Join(DefaultCgroupRoot, parent+"-confined"))
			_ = syscall.Rmdir(filepath.Join(DefaultCgroupRoot, parent))
		})
		cg, err := os.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer cg.Close()
		cmd := exec.Command(os.Args[0], "-test.run=^TestRealHostConfinedHerdlet$", "-test.v")
		cmd.Env = append(os.Environ(), "HERD_CONFINED_CHILD="+parent, "HERD_CONFINED_SELF=/"+parent+"-confined/herdlet")
		cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cg.Fd())}
		out, err := cmd.CombinedOutput()
		t.Logf("confined herdlet:\n%s", out)
		if err != nil {
			t.Fatalf("the confined herdlet failed: %v", err)
		}
		if ev, err := os.ReadFile(filepath.Join(dir, "memory.events")); err == nil && !regexp.MustCompile(`(?m)^oom_kill 0$`).Match(ev) {
			t.Errorf("the herdlet's cgroup saw an OOM kill: %s", ev)
		}
		return
	}
	parent = os.Getenv("HERD_CONFINED_CHILD")
	self, err := SelfCgroup()
	if err != nil || self != os.Getenv("HERD_CONFINED_SELF") {
		t.Fatalf("the herdlet half runs in %q (%v), want %s", self, err, os.Getenv("HERD_CONFINED_SELF"))
	}
	paths := map[string]string{}
	for _, k := range []string{"HERD_CH_BIN", "HERD_VIRTIOFSD_BIN", "HERD_KERNEL", "HERD_INITRAMFS"} {
		paths[k] = os.Getenv(k)
	}
	cgroups := Cgroups(CgroupV2{Parent: parent})
	if os.Getenv("HERD_CONFINED_NEUTRALIZE") == "1" {
		cgroups = nil // the base: the VMs stay in the herdlet's cgroup
	}
	root := shortDir(t)
	volumes := filepath.Join(root, "volumes")
	if err := os.MkdirAll(volumes, 0o700); err != nil {
		t.Fatal(err)
	}
	cpu := HostCPU()
	v := New(Config{Binary: paths["HERD_CH_BIN"], Virtiofsd: paths["HERD_VIRTIOFSD_BIN"], StateDir: filepath.Join(root, "h"),
		CPU: cpu, Cgroups: cgroups})
	spec := func(id string) vmm.Spec {
		return vmm.Spec{ID: id, Kernel: paths["HERD_KERNEL"], Initrd: paths["HERD_INITRAMFS"], Cmdline: DefaultCmdline(cpu.Arch()),
			VCPUs: 1, MemoryMiB: 512, Shares: []vmm.Share{{Tag: "volumes", HostPath: volumes}}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		for _, id := range []string{"src", "fork"} {
			_, _ = v.Delete(context.Background(), id)
		}
	})
	if _, err := v.Boot(ctx, spec("src")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second) // let the guest touch its memory
	snap, err := v.Snapshot(ctx, "src", vmm.FidelityMemory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Restore(ctx, snap, spec("fork")); err != nil {
		t.Fatalf("a MEMORY restore under a herdlet confined to 200 MB: %v", err)
	}
	for _, id := range []string{"src", "fork"} {
		in, err := v.instance(id)
		if err != nil || in == nil {
			t.Fatalf("%s: %v", id, err)
		}
		for _, p := range in.procs() {
			b, err := os.ReadFile("/proc/" + strconv.Itoa(p.Pid()) + "/cgroup")
			if err != nil {
				t.Fatal(err)
			}
			got, _ := ParseProcCgroup(string(b))
			if got != "/"+parent+"/"+id {
				t.Errorf("%s: pid %d runs in %s, want /%s/%s (the herdlet is in %s)", id, p.Pid(), got, parent, id, self)
			}
		}
	}
	time.Sleep(2 * time.Second)
	if gone, why, err := v.Exited(ctx, "fork"); gone || err != nil {
		t.Fatalf("the fork exited: %s (%v)", why, err)
	}
}

// TestVMProcessesEndWithTheHerdlet: a process started in a VM cgroup is out of the herdlet's cgroup, so a stopped
// Nomad task or systemd unit no longer takes it down; it ends when the herdlet process does (Pdeathsig), as a
// restarted herdlet does not adopt it. Needs root on a cgroup v2 host: HERD_CGROUP_REAL=1.
func TestVMProcessesEndWithTheHerdlet(t *testing.T) {
	if os.Getenv("HERD_CGROUP_REAL") != "1" {
		t.Skip("set HERD_CGROUP_REAL=1 (as root on a cgroup v2 host) to start a process in a real VM cgroup")
	}
	if parent := os.Getenv("HERD_PDEATH_CHILD"); parent != "" {
		dir, err := CgroupV2{Parent: parent}.Create("vm-1", 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		p, err := ExecProcesses{}.Start(ProcSpec{Path: "/bin/sleep", Args: []string{"60"}, Log: filepath.Join(os.TempDir(), parent+".log"), Cgroup: dir})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("PID", p.Pid())
		time.Sleep(time.Minute)
		return
	}
	parent := "herd-test-" + strconv.Itoa(os.Getpid())
	t.Cleanup(func() {
		_ = CgroupV2{Parent: parent}.Remove("vm-1")
		_ = syscall.Rmdir(filepath.Join(DefaultCgroupRoot, parent))
	})
	cmd := exec.Command(os.Args[0], "-test.run=^TestVMProcessesEndWithTheHerdlet$", "-test.v")
	cmd.Env = append(os.Environ(), "HERD_PDEATH_CHILD="+parent)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	buf := make([]byte, 4096)
	var seen string
	for pid == 0 {
		n, err := out.Read(buf)
		seen += string(buf[:n])
		if i := strings.Index(seen, "PID "); i >= 0 {
			if line, _, ok := strings.Cut(seen[i+4:], "\n"); ok {
				pid, _ = strconv.Atoi(line)
			}
		}
		if err != nil && pid == 0 {
			t.Fatalf("the herdlet half printed no pid: %q (%v)", seen, err)
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	p := ExecProcesses{}.Find(pid, "60")
	for i := 0; i < 500; i++ {
		if gone, _ := p.Exited(); gone {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("pid %d still runs after its herdlet was killed", pid)
}
