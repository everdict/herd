package mkfs_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/image/mkfs"
)

// TestRealBuild builds a real filesystem image with the host's tool (whichever Detect picks) from a layered image with
// whiteouts, and reads it back with debugfs (ext4) or dump.erofs. It skips where neither tool is installed.
func TestRealBuild(t *testing.T) {
	ctx := context.Background()
	b, why, err := mkfs.Detect(ctx, mkfs.Exec{})
	if err != nil {
		t.Skipf("no filesystem tool: %v", err)
	}
	t.Logf("builder: %s", why)
	layers := [][]byte{
		imagetest.Layer(
			imagetest.File{Name: "bin/sh", Body: "#!busybox", Mode: 0o755},
			imagetest.File{Name: "etc/old.conf", Body: "old"},
			imagetest.File{Name: "srv/ro/", Type: tar.TypeDir, Mode: 0o555},
			imagetest.File{Name: "srv/ro/kept", Body: "kept"},
		),
		imagetest.Layer(
			imagetest.File{Name: "etc/.wh.old.conf"},
			imagetest.File{Name: "etc/new.conf", Body: "new"},
			imagetest.File{Name: "usr/bin/env", Type: tar.TypeSymlink, Link: "/bin/sh"},
		),
	}
	dir := t.TempDir()
	flat := filepath.Join(dir, "rootfs.tar")
	f, err := os.Create(flat)
	if err != nil {
		t.Fatal(err)
	}
	var rs []io.Reader
	for _, l := range layers {
		rs = append(rs, bytes.NewReader(l))
	}
	if err := image.Flatten(rs, f, dir); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out := filepath.Join(dir, "rootfs.img")
	if err := b.Build(ctx, flat, out); err != nil {
		t.Fatal(err)
	}
	var read func(p string) (string, error)
	switch b.FSType() {
	case "ext4":
		debugfs, err := exec.LookPath("debugfs")
		if err != nil {
			t.Skip("ext4 built; no debugfs to read it back")
		}
		read = func(p string) (string, error) {
			o, err := exec.Command(debugfs, "-R", "cat "+p, out).Output()
			return string(o), err
		}
	default:
		fsck, err := exec.LookPath("fsck.erofs")
		if err != nil {
			t.Skip("erofs built; no fsck.erofs to read it back")
		}
		x := filepath.Join(dir, "x")
		if o, err := exec.Command(fsck, "--extract="+x, out).CombinedOutput(); err != nil {
			t.Fatalf("fsck.erofs: %v: %s", err, o)
		}
		read = func(p string) (string, error) {
			o, err := os.ReadFile(filepath.Join(x, p))
			return string(o), err
		}
	}
	for p, want := range map[string]string{"/bin/sh": "#!busybox", "/etc/new.conf": "new", "/srv/ro/kept": "kept"} {
		if got, err := read(p); err != nil || got != want {
			t.Errorf("%s in the %s image: %q, %v; want %q", p, b.FSType(), got, err, want)
		}
	}
	if got, _ := read("/etc/old.conf"); got != "" {
		t.Errorf("/etc/old.conf survived its whiteout: %q", got)
	}
	if b.FSType() == "ext4" {
		o, _ := exec.Command("debugfs", "-R", "stat /usr/bin/env", out).Output()
		if !strings.Contains(string(o), "Type: symlink") || !strings.Contains(string(o), "/bin/sh") {
			t.Errorf("/usr/bin/env is not a symlink to /bin/sh:\n%s", o)
		}
	}
}
