package deploy_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/everdict/herd/deploy"
)

// TestGuestIdentityNamesTheGuestBuild (DEFAUL-1606): a herdlet names the guest it boots by the bytes of its kernel and
// initramfs, so a release that changes either is a new guest and the same files are the same guest wherever they lie.
func TestGuestIdentityNamesTheGuestBuild(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	kernel, initrd := write("kernel", "kernel v0.1.5"), write("initrd.img", "herd-guest v0.1.5")
	id, err := deploy.GuestIdentity(kernel, initrd)
	if err != nil || !strings.HasPrefix(id, "sha256:") {
		t.Fatalf("GuestIdentity = %q, %v; want a sha256: digest", id, err)
	}
	if again, err := deploy.GuestIdentity(write("kernel.copy", "kernel v0.1.5"), write("initrd.copy", "herd-guest v0.1.5")); err != nil || again != id {
		t.Errorf("the same files elsewhere: %q, %v; want %q", again, err, id)
	}
	if fixed, err := deploy.GuestIdentity(kernel, write("initrd.img", "herd-guest v0.1.6")); err != nil || fixed == id || fixed == "" {
		t.Errorf("a new initramfs: %q, %v; want a guest other than %q", fixed, err, id)
	}
	if swapped, err := deploy.GuestIdentity(write("k2", "herd-guest v0.1.5"), write("i2", "kernel v0.1.5")); err != nil || swapped == id {
		t.Errorf("kernel and initramfs swapped: %q, %v; want a guest other than %q", swapped, err, id)
	}
	if _, err := deploy.GuestIdentity(filepath.Join(dir, "missing"), initrd); err == nil {
		t.Errorf("a missing kernel: no error")
	}
}
