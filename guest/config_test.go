package guest

import (
	"bufio"
	"os"
	"regexp"
	"strings"
	"testing"
)

var arches = []string{"amd64", "arm64"}

// required are the symbols the one guest kernel needs on both hypervisors (spec R-D7, R-D8): virtio over PCI,
// virtio-fs volumes, vsock control, erofs/ext4 rootfs with a writable overlay, an initramfs, and what crun needs.
var required = []string{
	"PCI", "VIRTIO", "VIRTIO_PCI", "VIRTIO_BLK", "VIRTIO_NET", "VIRTIO_CONSOLE", "HW_RANDOM_VIRTIO",
	"FUSE_FS", "VIRTIO_FS", "VSOCKETS", "VIRTIO_VSOCKETS",
	"EROFS_FS", "EXT4_FS", "OVERLAY_FS", "TMPFS", "PROC_FS", "SYSFS", "DEVTMPFS", "DEVTMPFS_MOUNT",
	"BLK_DEV_INITRD", "BINFMT_ELF", "BINFMT_SCRIPT", "UNIX", "INET",
	"CGROUPS", "MEMCG", "CGROUP_PIDS", "NAMESPACES", "PID_NS", "NET_NS", "IPC_NS", "UTS_NS", "USER_NS",
	"SECCOMP", "SECCOMP_FILTER",
}

// archOnly bounds what an architecture fragment may set: only what cannot be the same on both, so that the two
// kernels are built from one configuration.
var archOnly = map[string][]string{
	"amd64": {"64BIT", "HYPERVISOR_GUEST", "PARAVIRT", "KVM_GUEST", "PVH", "SERIAL_8250", "SERIAL_8250_CONSOLE"},
	"arm64": {"ARM64_4K_PAGES", "ARM_GIC_V3", "ARM_GIC_V3_ITS", "PCI_HOST_GENERIC", "SERIAL_AMBA_PL011", "SERIAL_AMBA_PL011_CONSOLE", "RTC_DRV_PL031"},
}

var line = regexp.MustCompile(`^CONFIG_([A-Z0-9_]+)=(.*)$`)

func parse(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer f.Close()
	out := map[string]string{}
	s := bufio.NewScanner(f)
	for n := 1; s.Scan(); n++ {
		l := strings.TrimSpace(s.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		m := line.FindStringSubmatch(l)
		if m == nil {
			t.Errorf("%s:%d: not a CONFIG_ line: %q", path, n, l)
			continue
		}
		if _, dup := out[m[1]]; dup {
			t.Errorf("%s:%d: CONFIG_%s set twice", path, n, m[1])
		}
		out[m[1]] = m[2]
	}
	return out
}

func TestOneKernelConfigForBothArches(t *testing.T) {
	common := parse(t, "kernel/herd-guest.config")
	for _, sym := range required {
		if common[sym] != "y" {
			t.Errorf("kernel/herd-guest.config: CONFIG_%s=%q, want y", sym, common[sym])
		}
	}
	for _, arch := range arches {
		frag := parse(t, "kernel/"+arch+".config")
		allowed := map[string]bool{}
		for _, s := range archOnly[arch] {
			allowed[s] = true
		}
		for sym := range frag {
			if _, ok := common[sym]; ok {
				t.Errorf("kernel/%s.config overrides CONFIG_%s of the common config", arch, sym)
			}
			if !allowed[sym] {
				t.Errorf("kernel/%s.config sets CONFIG_%s, which is not architecture-specific", arch, sym)
			}
		}
	}
}

func TestBuildUsesTheSameSourcesForBothArches(t *testing.T) {
	src, err := os.ReadFile("kernel/source.env")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^KERNEL_REPO=https://\S+$`).Match(src) || !regexp.MustCompile(`(?m)^KERNEL_REF=\S+$`).Match(src) {
		t.Errorf("kernel/source.env does not pin KERNEL_REPO and KERNEL_REF:\n%s", src)
	}
	build, err := os.ReadFile("build.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"source.env", "herd-guest.config", "${ARCH}.config", "initramfs.list", "amd64)", "arm64)"} {
		if !strings.Contains(string(build), w) {
			t.Errorf("build.sh does not use %q", w)
		}
	}
	list, err := os.ReadFile("initrd/initramfs.list")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"file /init ", "${HERD_GUEST}", "dir /dev ", "nod /dev/console "} {
		if !strings.Contains(string(list), w) {
			t.Errorf("initrd/initramfs.list lacks %q", w)
		}
	}
	if strings.Contains(string(list), "amd64") || strings.Contains(string(list), "arm64") {
		t.Errorf("initrd/initramfs.list names an architecture; it must be one list for both")
	}
}
