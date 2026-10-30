//go:build linux

package cloudhypervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
