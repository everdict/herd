package guest_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/everdict/herd/guest"
)

// kernelImage is a file with arch's boot header and nothing else.
func kernelImage(arch string) []byte {
	b := make([]byte, 4096)
	switch arch {
	case "arm64":
		copy(b[56:], "ARM\x64")
	case "amd64":
		copy(b[0x202:], "HdrS")
	}
	return b
}

// TestKernelArch (criterion multi-arch): the boot header tells an arm64 Image from an x86 bzImage, and anything
// else is refused rather than guessed; herdlet doctor compares it with the host's architecture.
func TestKernelArch(t *testing.T) {
	dir := t.TempDir()
	for _, arch := range []string{"arm64", "amd64"} {
		p := filepath.Join(dir, arch)
		if err := os.WriteFile(p, kernelImage(arch), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := guest.KernelArch(p); err != nil || got != arch {
			t.Errorf("%s kernel read as %q %v", arch, got, err)
		}
	}
	junk := filepath.Join(dir, "junk")
	if err := os.WriteFile(junk, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	if a, err := guest.KernelArch(junk); err == nil {
		t.Errorf("junk read as a %s kernel", a)
	}
	if _, err := guest.KernelArch(filepath.Join(dir, "missing")); err == nil {
		t.Errorf("a missing kernel was read")
	}
}

// TestBuildPlansBothArches (criterion multi-arch): build.sh all builds amd64 (bzImage) and arm64 (Image) into one
// output directory, herd-guest-<arch>/{kernel,initrd.img}: what scripts/release.sh packs per platform.
func TestBuildPlansBothArches(t *testing.T) {
	out := t.TempDir()
	b, err := exec.Command("sh", "build.sh", "--plan", "all", out).CombinedOutput()
	if err != nil {
		t.Fatalf("build.sh --plan all: %v\n%s", err, b)
	}
	want := "amd64 x86_64 bzImage " + out + "/herd-guest-amd64/kernel " + out + "/herd-guest-amd64/initrd.img\n" +
		"arm64 arm64 Image " + out + "/herd-guest-arm64/kernel " + out + "/herd-guest-arm64/initrd.img\n"
	if string(b) != want {
		t.Errorf("build.sh --plan all:\n%s\nwant\n%s", b, want)
	}
	if b, err := exec.Command("sh", "build.sh", "--plan", "riscv64", out).CombinedOutput(); err == nil || !strings.Contains(string(b), "unknown arch") {
		t.Errorf("build.sh riscv64: %v %s", err, b)
	}
}
