package layer_test

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/everdict/herd/guest/layer"
)

func write(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// A container's layer saved into one share and restored from it into fresh directories holds the files, modes,
// times, symlinks and read-only directories it had, and nothing under a skipped path.
func TestSaveRestoreRoundTrip(t *testing.T) {
	upper, tmp, share := t.TempDir(), t.TempDir(), t.TempDir()
	write(t, filepath.Join(upper, "root", "marker"), "parent", 0o640)
	write(t, filepath.Join(upper, "usr", "bin", "tool"), "#!/bin/sh\n", os.ModeSetuid|0o755)
	write(t, filepath.Join(upper, "everdict", "exec", "p1", "meta"), "running", 0o644)
	write(t, filepath.Join(upper, "ro", "inside"), "kept", 0o444)
	if err := os.Chmod(filepath.Join(upper, "ro"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(upper, "ro"), 0o755) })
	if err := os.Symlink("../root/marker", filepath.Join(upper, "usr", "marker")); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(upper, "root", "marker"), at, at); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tmp, "walk-marker"), "tmp", 0o600)
	if err := syscall.Mkfifo(filepath.Join(tmp, "fifo"), 0o600); err != nil && runtime.GOOS == "linux" {
		t.Fatal(err)
	}

	if err := layer.Save(share, "main", map[string]string{"upper": upper, "tmp": tmp}, []string{"upper/everdict/exec"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(share, layer.Path("main"))); err != nil {
		t.Fatalf("no archive at %s: %v", layer.Path("main"), err)
	}
	if ents, _ := os.ReadDir(filepath.Join(share, layer.Dir)); len(ents) != 1 {
		t.Errorf("the share's layer directory holds %d entries, want the archive only", len(ents))
	}

	up2, tmp2 := t.TempDir(), t.TempDir()
	ok, err := layer.Restore(share, "main", map[string]string{"upper": up2, "tmp": tmp2})
	if err != nil || !ok {
		t.Fatalf("Restore = %v, %v", ok, err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(up2, "ro"), 0o755) })
	if got := read(t, filepath.Join(up2, "root", "marker")); got != "parent" {
		t.Errorf("root/marker = %q", got)
	}
	fi, err := os.Stat(filepath.Join(up2, "root", "marker"))
	if err != nil || fi.Mode().Perm() != 0o640 || !fi.ModTime().Equal(at) {
		t.Errorf("root/marker: %v, %v; want 0640 at %v", fi.Mode(), fi.ModTime(), at)
	}
	if fi, err := os.Stat(filepath.Join(up2, "usr", "bin", "tool")); err != nil || fi.Mode()&os.ModeSetuid == 0 || fi.Mode().Perm() != 0o755 {
		t.Errorf("usr/bin/tool: %v, %v; want setuid 0755", fi.Mode(), err)
	}
	if l, err := os.Readlink(filepath.Join(up2, "usr", "marker")); err != nil || l != "../root/marker" {
		t.Errorf("usr/marker link = %q, %v", l, err)
	}
	if fi, err := os.Stat(filepath.Join(up2, "ro")); err != nil || fi.Mode().Perm() != 0o555 {
		t.Errorf("ro: %v, %v; want 0555", fi.Mode(), err)
	}
	if got := read(t, filepath.Join(up2, "ro", "inside")); got != "kept" {
		t.Errorf("ro/inside = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(up2, "everdict", "exec")); !os.IsNotExist(err) {
		t.Errorf("the skipped exec records came back: %v", err)
	}
	if got := read(t, filepath.Join(tmp2, "walk-marker")); got != "tmp" {
		t.Errorf("tmp/walk-marker = %q", got)
	}
	if runtime.GOOS == "linux" {
		if fi, err := os.Lstat(filepath.Join(tmp2, "fifo")); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
			t.Errorf("tmp/fifo: %v, %v; want a fifo", fi, err)
		}
	}
}

// A share without the container's archive restores nothing and is no error: a new actor, or a snapshot from before
// herd carried layers.
func TestRestoreWithoutArchive(t *testing.T) {
	up := t.TempDir()
	ok, err := layer.Restore(t.TempDir(), "main", map[string]string{"upper": up})
	if ok || err != nil {
		t.Fatalf("Restore = %v, %v; want false, nil", ok, err)
	}
	if ents, _ := os.ReadDir(up); len(ents) != 0 {
		t.Errorf("upper holds %d entries", len(ents))
	}
}

// An archive cannot write outside its directories: not by a ".." element, not through a symlink it made.
func TestExtractStaysInside(t *testing.T) {
	for _, c := range []struct {
		name string
		ents []tar.Header
	}{
		{"dotdot", []tar.Header{{Name: "upper/../escape", Typeflag: tar.TypeReg, Mode: 0o644}}},
		{"symlink", []tar.Header{
			{Name: "upper/out", Typeflag: tar.TypeSymlink, Linkname: "/"},
			{Name: "upper/out/escape", Typeflag: tar.TypeReg, Mode: 0o644},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var b bytes.Buffer
			tw := tar.NewWriter(&b)
			for _, h := range c.ents {
				if err := tw.WriteHeader(&h); err != nil {
					t.Fatal(err)
				}
			}
			tw.Close()
			parent := t.TempDir()
			up := filepath.Join(parent, "upper")
			if err := os.Mkdir(up, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := layer.Extract(&b, map[string]string{"upper": up}); err == nil {
				t.Errorf("Extract took an escaping archive")
			}
			if _, err := os.Lstat(filepath.Join(parent, "escape")); !os.IsNotExist(err) {
				t.Errorf("a file was written outside: %v", err)
			}
		})
	}
}

// An overlay upper's whiteout (a 0/0 character device) and an opaque directory (trusted.overlay.opaque) come back
// as they were. Making a device and setting a trusted attribute need root, as herd-guest is in its VM.
func TestWhiteoutAndOpaqueRoundTrip(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("needs root on Linux (mknod, trusted.* attributes), as herd-guest has in its VM")
	}
	upper, share := t.TempDir(), t.TempDir()
	if err := syscall.Mknod(filepath.Join(upper, "gone"), syscall.S_IFCHR|0o000, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(upper, "opaque"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setxattr(filepath.Join(upper, "opaque"), "trusted.overlay.opaque", []byte("y"), 0); err != nil {
		t.Skipf("the test filesystem keeps no trusted attributes: %v", err)
	}
	if err := layer.Save(share, "main", map[string]string{"upper": upper}, nil); err != nil {
		t.Fatal(err)
	}
	up2 := t.TempDir()
	if _, err := layer.Restore(share, "main", map[string]string{"upper": up2}); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(up2, "gone"), &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFCHR || st.Rdev != 0 {
		t.Errorf("whiteout: mode %o rdev %d, %v", st.Mode, st.Rdev, err)
	}
	buf := make([]byte, 8)
	if n, err := syscall.Getxattr(filepath.Join(up2, "opaque"), "trusted.overlay.opaque", buf); err != nil || string(buf[:n]) != "y" {
		t.Errorf("opaque: %q, %v", buf[:n], err)
	}
}
