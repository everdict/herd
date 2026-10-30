//go:build linux

package cloudhypervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/vmmtest"
)

// CPUID words (EAX, EBX, ECX, EDX) from public AIDA64 dumps in github.com/InstLatx64/InstLatx64 (GenuineIntel/):
// GenuineIntel00806C1_TigerLake_01_CPUID.txt (i5-1135G7: the i7-1165G7's die, CPUID 806C1) and
// GenuineIntel00B06D1_LunarLake_04_CPUID.txt (Core Ultra 9 288V: the Core Ultra 5 226V's die, CPUID B06D1). The R0
// spike restored between an i7-1165G7 and a Core Ultra 5 226V and was refused on leaf 0x1 ECX and leaf 0x7.
var (
	tigerLake = cpuidTable{
		0: {0x0000001B, 0x756E6547, 0x6C65746E, 0x49656E69},
		1: {0x000806C1, 0x00100800, 0x7FFAFBBF, 0xBFEBFBFF},
		7: {0x00000000, 0xF3BFA7EB, 0x18C05FCE, 0xFC100510},
	}
	lunarLake = cpuidTable{
		0: {0x00000023, 0x756E6547, 0x6C65746E, 0x49656E69},
		1: {0x000B06D1, 0x00800800, 0x7FFAFBFF, 0xBFEBFBFF},
		7: {0x00000002, 0x239CA7EB, 0x994027AC, 0xFC1CC430},
	}
)

type cpuidTable map[uint32][4]uint32

func (c cpuidTable) cpuid(leaf, _ uint32) (eax, ebx, ecx, edx uint32) {
	r := c[leaf]
	return r[0], r[1], r[2], r[3]
}

func (c cpuidTable) with(leaf uint32, reg int, v uint32) cpuidTable {
	n := cpuidTable{}
	for k, r := range c {
		n[k] = r
	}
	r := n[leaf]
	r[reg] = v
	n[leaf] = r
	return n
}

// shortDir is a temp dir short enough for unix socket paths (t.TempDir's names can exceed 108 bytes).
func shortDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "herdch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

type rig struct {
	host    *fakeHost
	root    string
	volumes string
}

func newRig(t *testing.T) *rig {
	r := &rig{host: newFakeHost(t), root: shortDir(t)}
	r.volumes = filepath.Join(r.root, "volumes")
	if err := os.MkdirAll(r.volumes, 0o700); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) vmm(stateDir string, cpu cpuidTable) *VMM {
	return New(Config{Binary: r.host.chBin, Virtiofsd: r.host.fsBin, StateDir: filepath.Join(r.root, stateDir),
		SnapshotDir: filepath.Join(r.root, "snapshots"), CPU: X86CPU{CPUID: cpu.cpuid}, Processes: r.host,
		ExitTimeout: 200 * time.Millisecond})
}

func (r *rig) spec(id string) vmm.Spec {
	return vmm.Spec{ID: id, Kernel: "/guest/bzImage", Initrd: "/guest/initramfs", Cmdline: "console=ttyS0",
		Rootfs: []string{"/guest/rootfs.raw"}, VCPUs: 1, MemoryMiB: 512,
		Shares: []vmm.Share{{Tag: "volumes", HostPath: r.volumes}}}
}

func ctxT(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// TestContractFakeCH runs the vmm contract against the backend driving the fake Cloud Hypervisor API server, with a
// second host of the Lunar Lake class sharing the snapshot store.
func TestContractFakeCH(t *testing.T) {
	var r *rig
	vmmtest.Run(t, vmmtest.Harness{
		New: func(t *testing.T) vmm.VMM {
			r = newRig(t)
			return &faultyVMM{VMM: r.vmm("a", tigerLake), h: r.host}
		},
		Elsewhere: func(t *testing.T) vmm.VMM { return r.vmm("b", lunarLake) },
		Spec:      func(id string) vmm.Spec { return r.spec(id) },
		Timeout:   10 * time.Second,
	})
}

func TestFamilyFromCPUID(t *testing.T) {
	ctx := ctxT(t, time.Second)
	r := newRig(t)
	tl, err := r.vmm("a", tigerLake).Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ll, err := r.vmm("b", lunarLake).Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("i7-1165G7 class: %s; Core Ultra 226V class: %s", tl.RestoreFamily, ll.RestoreFamily)
	if tl.RestoreFamily == ll.RestoreFamily {
		t.Errorf("Tiger Lake and Lunar Lake both declare %s; CH refuses restores between them", tl.RestoreFamily)
	}
	if tl.Platform != vmm.PlatformCloudHypervisor || tl.Arch != "amd64" || tl.SuspendMaxFidelity != vmm.FidelityMemory {
		t.Errorf("capabilities %+v", tl)
	}
	if !strings.HasPrefix(tl.RestoreFamily, "microvm-ch-amd64-intel-") {
		t.Errorf("family %q is not microvm-ch-amd64-intel-<digest>", tl.RestoreFamily)
	}
	// Words CH does not check (stepping in leaf 1 EAX, the APIC id in leaf 1 EBX, leaf 7's sub-leaf count) do not
	// change the class: every core of one host, and identical hosts, share it.
	same := tigerLake.with(1, 0, 0x000806C2).with(1, 1, 0x07100800)
	if c, _ := (X86CPU{CPUID: same.cpuid}).Class(); "microvm-ch-amd64-"+c != tl.RestoreFamily {
		t.Errorf("unchecked words changed the class: %s vs %s", c, tl.RestoreFamily)
	}
	// One bit in any checked word changes it.
	base, _ := (X86CPU{CPUID: tigerLake.cpuid}).Class()
	for _, w := range []struct {
		leaf uint32
		reg  int
	}{{1, 2}, {7, 1}, {7, 2}, {7, 3}} {
		flipped := tigerLake.with(w.leaf, w.reg, tigerLake[w.leaf][w.reg]^(1<<5))
		if c, _ := (X86CPU{CPUID: flipped.cpuid}).Class(); c == base {
			t.Errorf("a flipped bit in leaf %#x reg %d kept class %s", w.leaf, w.reg, c)
		}
	}
	if _, err := (X86CPU{CPUID: cpuidTable{0: {6, 0, 0, 0}}.cpuid}).Class(); err == nil {
		t.Errorf("a CPU without leaf 7 got a class")
	}
}

// TestMemoryRestoreOtherFamilyRefusedBeforeCH: the refusal is herd's, decided before any process starts or any
// Cloud Hypervisor endpoint is called on the restoring host.
func TestMemoryRestoreOtherFamilyRefusedBeforeCH(t *testing.T) {
	ctx := ctxT(t, 10*time.Second)
	r := newRig(t)
	here, there := r.vmm("a", tigerLake), r.vmm("b", lunarLake)
	if _, err := here.Boot(ctx, r.spec("m1")); err != nil {
		t.Fatal(err)
	}
	mem, err := here.Snapshot(ctx, "m1", vmm.FidelityMemory)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"config.json", "state.json", "memory-ranges", metaFile} {
		if _, err := os.Stat(filepath.Join(mem.Location, f)); err != nil {
			t.Errorf("MEMORY snapshot lacks %s: %v", f, err)
		}
	}
	calls, starts := r.host.callCount()
	if _, err := there.Restore(ctx, mem, r.spec("m1")); !errors.Is(err, vmm.ErrRestoreFamilyMismatch) {
		t.Fatalf("MEMORY restore into the Lunar Lake family = %v, want ErrRestoreFamilyMismatch", err)
	}
	if c, s := r.host.callCount(); c != calls || s != starts {
		t.Errorf("the refused restore made %d API calls and started %d processes", c-calls, s-starts)
	}
	if _, err := os.Stat(there.vmDir("m1")); !os.IsNotExist(err) {
		t.Errorf("the refused restore left %s (%v)", there.vmDir("m1"), err)
	}
	// The same family restores, through vm.restore with the eager (Copy) mode.
	if p, err := here.Delete(ctx, "m1"); p != vmm.PresenceGone {
		t.Fatalf("Delete = %v (%v)", p, err)
	}
	twin := r.vmm("a", tigerLake.with(1, 1, 0x02100800)) // another core of the same host class
	if _, err := twin.Restore(ctx, mem, r.spec("m1")); err != nil {
		t.Fatalf("MEMORY restore in the same family: %v", err)
	}
	if p, err := twin.Delete(ctx, "m1"); p != vmm.PresenceGone {
		t.Errorf("Delete = %v (%v)", p, err)
	}
}

// TestBootWaitsForVMBoot: Boot answers only once vm.boot has answered, and a vm.boot that never answers is an
// error, not a VM.
func TestBootWaitsForVMBoot(t *testing.T) {
	r := newRig(t)
	v := r.vmm("a", tigerLake)
	r.host.mu.Lock()
	r.host.delay[epBoot] = 150 * time.Millisecond
	r.host.mu.Unlock()
	start := time.Now()
	vm, err := v.Boot(ctxT(t, 5*time.Second), r.spec("w1"))
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 150*time.Millisecond || vm.State != vmm.StateRunning {
		t.Errorf("Boot answered %+v after %v, before vm.boot (150 ms) answered", vm, d)
	}
	r.host.clear()
	r.host.mu.Lock()
	r.host.hang[epBoot] = true
	r.host.mu.Unlock()
	vm, err = v.Boot(ctxT(t, 100*time.Millisecond), r.spec("w2"))
	if !errors.Is(err, context.DeadlineExceeded) || vm != (vmm.VM{}) {
		t.Errorf("Boot with a hung vm.boot = %+v (%v), want no VM and context.DeadlineExceeded", vm, err)
	}
	r.host.clear()
	if p, err := v.Inspect(ctxT(t, time.Second), "w2"); p != vmm.PresenceGone {
		t.Errorf("a failed Boot left w2 %v (%v)", p, err)
	}
}

// TestBootDefaultCmdline: a spec without a command line boots with the backend's default for the host's arch, a
// console the guest's output reaches and a panic that ends the VM, not an empty payload cmdline; a given one is
// passed through as is.
func TestBootDefaultCmdline(t *testing.T) {
	for _, c := range []struct {
		name, cmdline, want string
		cpu                 CPU
	}{
		{"amd64 empty", "", "console=ttyS0 panic=-1", X86CPU{CPUID: tigerLake.cpuid}},
		{"arm64 empty", "", "console=ttyAMA0 panic=-1", ARM64CPU{Root: "/nonexistent"}},
		{"given", "console=ttyS0 quiet", "console=ttyS0 quiet", X86CPU{CPUID: tigerLake.cpuid}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			v := New(Config{Binary: r.host.chBin, Virtiofsd: r.host.fsBin, StateDir: filepath.Join(r.root, "a"),
				CPU: c.cpu, Processes: r.host, ExitTimeout: 200 * time.Millisecond})
			spec := r.spec("c1")
			spec.Cmdline = c.cmdline
			if _, err := v.Boot(ctxT(t, 5*time.Second), spec); err != nil {
				t.Fatal(err)
			}
			r.host.mu.Lock()
			got := r.host.cmdlines
			r.host.mu.Unlock()
			if len(got) != 1 || got[0] != c.want {
				t.Errorf("spec cmdline %q sent payload cmdlines %q to Cloud Hypervisor, want [%q]", c.cmdline, got, c.want)
			}
		})
	}
}

// TestCallWithoutAnswerIsNotSuccess: the client never turns a timed-out or unanswered call into success.
func TestCallWithoutAnswerIsNotSuccess(t *testing.T) {
	r := newRig(t)
	v := r.vmm("a", tigerLake)
	ctx := ctxT(t, 5*time.Second)
	if _, err := v.Boot(ctx, r.spec("n1")); err != nil {
		t.Fatal(err)
	}
	in, _ := v.instance("n1")
	r.host.mu.Lock()
	r.host.hang[epPause] = true
	r.host.unreachable[epResume] = true
	r.host.mu.Unlock()
	if err := in.api.put(ctxT(t, 50*time.Millisecond), epPause, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("hung vm.pause = %v, want context.DeadlineExceeded", err)
	}
	if err := in.api.put(ctx, epResume, nil); !errors.Is(err, vmm.ErrUnreachable) {
		t.Errorf("vm.resume with the connection dropped = %v, want ErrUnreachable", err)
	}
	if err := newClient(filepath.Join(r.root, "nobody.sock")).put(ctx, epBoot, nil); !errors.Is(err, vmm.ErrUnreachable) {
		t.Errorf("call on a socket nobody listens on = %v, want ErrUnreachable", err)
	}
	r.host.clear()
	var ae *APIError
	if err := in.api.put(ctx, epBoot, nil); !errors.As(err, &ae) || ae.Status != 404 {
		t.Errorf("vm.boot of a booted VM = %v, want the VMM's refusal", err)
	}
	// A snapshot whose vm.snapshot never answered leaves nothing in the store and the VM running.
	r.host.mu.Lock()
	r.host.hang[epSnapshot] = true
	r.host.mu.Unlock()
	if s, err := v.Snapshot(ctxT(t, 100*time.Millisecond), "n1", vmm.FidelityMemory); err == nil || s != (vmm.Snapshot{}) {
		t.Errorf("Snapshot with a hung vm.snapshot = %+v (%v)", s, err)
	}
	r.host.clear()
	if ents, _ := os.ReadDir(filepath.Join(r.root, "snapshots")); len(ents) != 0 {
		t.Errorf("a failed snapshot left %d entries in the store", len(ents))
	}
	if _, err := v.Snapshot(ctx, "n1", vmm.FidelityMemory); err != nil {
		t.Errorf("the VM was not left running after the failed snapshot: %v", err)
	}
}

// TestDeleteVerifiesProcessesExited: a virtiofsd that survives SIGKILL keeps the VM present.
func TestDeleteVerifiesProcessesExited(t *testing.T) {
	r := newRig(t)
	v := r.vmm("a", tigerLake)
	ctx := ctxT(t, 5*time.Second)
	if _, err := v.Boot(ctx, r.spec("d1")); err != nil {
		t.Fatal(err)
	}
	r.host.mu.Lock()
	r.host.fsSurvive = true
	r.host.mu.Unlock()
	p, err := v.Delete(ctx, "d1")
	if p != vmm.PresencePresent || !errors.Is(err, vmm.ErrDeleteNotVerified) {
		t.Errorf("Delete with virtiofsd still running = %v (%v), want present with ErrDeleteNotVerified", p, err)
	}
	if r.host.running() == 0 {
		t.Errorf("the fake shows no surviving process")
	}
	r.host.clear()
	if p, err := v.Delete(ctx, "d1"); p != vmm.PresenceGone || err != nil {
		t.Errorf("Delete once virtiofsd can exit = %v (%v)", p, err)
	}
	if n := r.host.running(); n != 0 {
		t.Errorf("gone answered with %d processes running", n)
	}
}

// TestDeleteVerifiesStateDirAbsent: a VM directory that is still there after removal keeps the VM present.
func TestDeleteVerifiesStateDirAbsent(t *testing.T) {
	r := newRig(t)
	v := r.vmm("a", tigerLake)
	ctx := ctxT(t, 5*time.Second)
	if _, err := v.Boot(ctx, r.spec("d2")); err != nil {
		t.Fatal(err)
	}
	v.removeAll = func(string) error { return nil } // the files survive
	p, err := v.Delete(ctx, "d2")
	if p != vmm.PresencePresent || !errors.Is(err, vmm.ErrDeleteNotVerified) {
		t.Errorf("Delete with the state dir still there = %v (%v), want present with ErrDeleteNotVerified", p, err)
	}
	v.removeAll = os.RemoveAll
	if p, err := v.Delete(ctx, "d2"); p != vmm.PresenceGone || err != nil {
		t.Errorf("Delete = %v (%v)", p, err)
	}
	if _, err := os.Stat(v.vmDir("d2")); !os.IsNotExist(err) {
		t.Errorf("gone answered with %s still there (%v)", v.vmDir("d2"), err)
	}
}

// TestDeleteAfterRestart: a herdlet that restarted finds the VM's processes through its pid files and still verifies
// them exited.
func TestDeleteAfterRestart(t *testing.T) {
	r := newRig(t)
	ctx := ctxT(t, 5*time.Second)
	if _, err := r.vmm("a", tigerLake).Boot(ctx, r.spec("d3")); err != nil {
		t.Fatal(err)
	}
	again := r.vmm("a", tigerLake)
	if p, err := again.Inspect(ctx, "d3"); p != vmm.PresencePresent {
		t.Fatalf("Inspect after restart = %v (%v)", p, err)
	}
	r.host.mu.Lock()
	r.host.ignoreShutdown = true
	r.host.mu.Unlock()
	if p, err := again.Delete(ctx, "d3"); p != vmm.PresencePresent || !errors.Is(err, vmm.ErrDeleteNotVerified) {
		t.Errorf("Delete after restart with the VMM surviving = %v (%v)", p, err)
	}
	r.host.clear()
	if p, err := again.Delete(ctx, "d3"); p != vmm.PresenceGone || err != nil {
		t.Errorf("Delete after restart = %v (%v)", p, err)
	}
	if n := r.host.running(); n != 0 {
		t.Errorf("gone answered with %d processes running", n)
	}
}

// TestNICOnBootAndRestore: a spec's NIC reaches Cloud Hypervisor as a net device on the VM's TAP with its MAC, on
// vm.create and in the config a MEMORY restore re-opens; a spec without one has no net device.
func TestNICOnBootAndRestore(t *testing.T) {
	r := newRig(t)
	v := r.vmm("a", tigerLake)
	ctx := ctxT(t, 10*time.Second)
	spec := r.spec("n1")
	spec.NIC = &vmm.NIC{TAP: "herd7", MAC: "02:68:64:00:00:07"}
	if _, err := v.Boot(ctx, spec); err != nil {
		t.Fatal(err)
	}
	snap, err := v.Snapshot(ctx, "n1", vmm.FidelityMemory)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := v.Delete(ctx, "n1"); p != vmm.PresenceGone {
		t.Fatal(p, err)
	}
	if _, err := v.Restore(ctx, snap, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Boot(ctx, r.spec("n2")); err != nil {
		t.Fatal(err)
	}
	r.host.mu.Lock()
	got := strings.Join(r.host.nets, "; ")
	r.host.mu.Unlock()
	if want := "vm.create tap=herd7,mac=02:68:64:00:00:07; vm.restore tap=herd7,mac=02:68:64:00:00:07"; got != want {
		t.Errorf("net devices sent to Cloud Hypervisor: %q, want %q", got, want)
	}
}
