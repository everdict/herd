//go:build linux

package cloudhypervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestExecProcesses exercises the real process boundary with sleep(1): a started child and the same pid found again
// through procfs are running until killed, and observed exited afterwards; a pid whose command line lacks the marker
// is not taken for the VM's process.
func TestExecProcesses(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep(1) on this host")
	}
	var ps ExecProcesses
	p, err := ps.Start(ProcSpec{Path: sleep, Args: []string{"31.5"}, Log: filepath.Join(t.TempDir(), "log"), Marker: "31.5"})
	if err != nil {
		t.Fatal(err)
	}
	found := ps.Find(p.Pid(), "31.5")
	for _, q := range []Process{p, found} {
		if gone, err := q.Exited(); gone || err != nil {
			t.Fatalf("pid %d exited=%v (%v) while sleeping", q.Pid(), gone, err)
		}
	}
	// Right after exec the command line can read empty for a moment; the marker check needs it filled in.
	for i := 0; i < 200; i++ {
		if b, _ := os.ReadFile("/proc/" + strconv.Itoa(p.Pid()) + "/cmdline"); len(b) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if gone, err := ps.Find(p.Pid(), "api.sock").Exited(); !gone || err != nil {
		t.Errorf("a pid without the marker = exited %v (%v), want taken as exited (pid reuse)", gone, err)
	}
	if err := found.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		g1, _ := p.Exited()
		g2, _ := found.Exited()
		if g1 && g2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after SIGKILL: child exited=%v, procfs exited=%v", g1, g2)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := p.Signal(syscall.SIGKILL); err != nil {
		t.Errorf("signal to an exited child = %v", err)
	}
}

// TestHostCPUClass derives this host's class from its own CPU (CPUID on amd64).
func TestHostCPUClass(t *testing.T) {
	cpu := HostCPU()
	class, err := cpu.Class()
	if cpu.Arch() != "amd64" && cpu.Arch() != "arm64" {
		if err == nil {
			t.Errorf("%s got class %q", cpu.Arch(), class)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if !idRE.MatchString(class) {
		t.Errorf("class %q is not a lower-case label", class)
	}
	t.Logf("this host: %s %s", cpu.Arch(), class)
}

// TestARM64Class: the arm64 class follows MIDR_EL1 and the feature list, and only them.
func TestARM64Class(t *testing.T) {
	host := func(midr, features string) ARM64CPU {
		root := t.TempDir()
		id := filepath.Join(root, "sys/devices/system/cpu/cpu0/regs/identification")
		if err := os.MkdirAll(id, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(id, "midr_el1"), []byte(midr+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(root, "proc/cpuinfo"), []byte("processor\t: 0\nBogoMIPS\t: 50.00\nFeatures\t: "+features+"\nCPU implementer\t: 0x41\n"), 0o644)
		return ARM64CPU{Root: root}
	}
	n1, err := host("0x00000000413fd0c1", "fp asimd evtstrm aes pmull sha1 sha2 crc32 atomics").Class()
	if err != nil || !idRE.MatchString(n1) {
		t.Fatalf("class %q, %v", n1, err)
	}
	if again, _ := host("0x00000000413fd0c1", "fp  asimd evtstrm aes pmull sha1 sha2 crc32 atomics").Class(); again != n1 {
		t.Errorf("the same core got %s and %s", n1, again)
	}
	if other, _ := host("0x00000000414fd0b1", "fp asimd evtstrm aes pmull sha1 sha2 crc32 atomics").Class(); other == n1 {
		t.Errorf("a different core kept class %s", n1)
	}
	if other, _ := host("0x00000000413fd0c1", "fp asimd evtstrm aes pmull sha1 sha2 crc32 atomics sve").Class(); other == n1 {
		t.Errorf("a different feature list kept class %s", n1)
	}
	if _, err := (ARM64CPU{Root: t.TempDir()}).Class(); err == nil {
		t.Errorf("a host without MIDR_EL1 got a class")
	}
}
