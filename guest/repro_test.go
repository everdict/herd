package guest_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/guest/initrd"
)

// TestInitrdReproducible (DEFAUL-1626): the guest initramfs, which with the kernel is the guest herd-api compares to
// decide a golden rebuild (deploy.GuestIdentity), is the same bytes from two builds of one revision in two fresh
// checkouts, and from a later revision that changes nothing in the guest (a release of herdlet only); a revision
// that does change the guest changes it. busybox and crun are fixed stand-ins whose files are written at different
// times on each side, as a fresh download would be. The kernel is not built here (no C toolchain offline):
// scripts/guest-repro.sh --kernel compares it on a host that can.
func TestInitrdReproducible(t *testing.T) {
	if testing.Short() {
		t.Skip("builds herd-guest four times")
	}
	for _, tool := range []string{"git", "gzip", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("needs %s: %v", tool, err)
		}
	}
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	copyTree(t, "..", a)
	git(t, a, "2026-01-01T00:00:00Z", "init", "-q")
	git(t, a, "2026-01-01T00:00:00Z", "add", "-A")
	git(t, a, "2026-01-01T00:00:00Z", "commit", "-q", "-m", "one revision")
	b := filepath.Join(tmp, "another", "checkout")
	git(t, tmp, "", "clone", "-q", a, b)

	digestA, initA := buildInitrd(t, a, filepath.Join(tmp, "out-a"), time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC))
	digestB, initB := buildInitrd(t, b, filepath.Join(tmp, "out-b"), time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC))
	if digestA != digestB {
		t.Errorf("one revision, two checkouts: initrd.img %s and %s\n%s", digestA, digestB, diffInitrd(t, initA, initB))
	}

	// A release that changes herdlet only.
	if err := os.WriteFile(filepath.Join(b, "herdlet", "release_note.go"), []byte("package herdlet\n\n// A herdlet-only change.\nconst releaseNote = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, b, "2026-02-01T00:00:00Z", "add", "-A")
	git(t, b, "2026-02-01T00:00:00Z", "commit", "-q", "-m", "a herdlet-only release")
	digestR, initR := buildInitrd(t, b, filepath.Join(tmp, "out-release"), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if digestA != digestR {
		t.Errorf("a revision that changes no guest source: initrd.img %s, then %s\n%s", digestA, digestR, diffInitrd(t, initA, initR))
	}

	// A revision that changes the guest: the comparison is not vacuous.
	list := filepath.Join(b, "guest", "initrd", "initramfs.list")
	l, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(list, append(l, "dir /opt 0755 0 0\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, b, "2026-03-01T00:00:00Z", "commit", "-q", "-am", "a guest change")
	if d, _ := buildInitrd(t, b, filepath.Join(tmp, "out-guest"), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)); d == digestA {
		t.Errorf("a revision that adds /opt to the initramfs kept initrd.img %s", d)
	}
}

// buildInitrd runs src's guest/build.sh --initrd amd64 into out, with busybox and crun stand-ins whose files have
// mtime, and answers initrd.img's SHA-256 and path.
func buildInitrd(t *testing.T, src, out string, mtime time.Time) (string, string) {
	t.Helper()
	in := filepath.Join(out, "inputs")
	if err := os.MkdirAll(in, 0o755); err != nil {
		t.Fatal(err)
	}
	env := os.Environ()
	for _, name := range []string{"busybox", "crun"} {
		p := filepath.Join(in, name)
		if err := os.WriteFile(p, []byte("#!/bin/false\n# a stand-in for a static "+name+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		env = append(env, strings.ToUpper(name)+"="+p)
	}
	cmd := exec.Command("sh", filepath.Join(src, "guest", "build.sh"), "--initrd", "amd64", out)
	cmd.Dir = src
	cmd.Env = env
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s/guest/build.sh --initrd amd64 %s: %v\n%s", src, out, err, b)
	}
	p := filepath.Join(out, "herd-guest-amd64", "initrd.img")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b)), p
}

// diffInitrd names the members of two initramfs images that differ, and how.
func diffInitrd(t *testing.T, p, q string) string {
	t.Helper()
	mp, mq := readInitrd(t, p), readInitrd(t, q)
	var out []string
	for i := 0; i < len(mp) || i < len(mq); i++ {
		if i >= len(mp) || i >= len(mq) {
			out = append(out, fmt.Sprintf("member %d: only one side has it", i))
			continue
		}
		x, y := mp[i], mq[i]
		if x.Name != y.Name {
			out = append(out, fmt.Sprintf("member %d: %s and %s", i, x.Name, y.Name))
			continue
		}
		if x.MTime != y.MTime {
			out = append(out, fmt.Sprintf("%s: mtime %d and %d", x.Name, x.MTime, y.MTime))
		}
		if x.Mode != y.Mode || x.UID != y.UID || x.GID != y.GID || x.Ino != y.Ino {
			out = append(out, fmt.Sprintf("%s: mode/uid/gid/ino %o/%d/%d/%d and %o/%d/%d/%d", x.Name, x.Mode, x.UID, x.GID, x.Ino, y.Mode, y.UID, y.GID, y.Ino))
		}
		if !bytes.Equal(x.Data, y.Data) {
			out = append(out, fmt.Sprintf("%s: content sha256:%x and sha256:%x%s", x.Name, sha256.Sum256(x.Data), sha256.Sum256(y.Data), buildInfoDiff(t, x.Data, y.Data)))
		}
	}
	if len(out) == 0 {
		out = append(out, "every member is the same: the compression differs")
	}
	return strings.Join(out, "\n")
}

// buildInfoDiff names the Go build settings two binaries differ in, if they are Go binaries.
func buildInfoDiff(t *testing.T, x, y []byte) string {
	info := func(b []byte) map[string]bool {
		p := filepath.Join(t.TempDir(), "bin")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return nil
		}
		out, err := exec.Command("go", "version", "-m", p).Output()
		if err != nil {
			return nil
		}
		m := map[string]bool{}
		for _, l := range strings.Split(string(out), "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "build" {
				m[f[1]] = true
			}
		}
		return m
	}
	ix, iy := info(x), info(y)
	var d []string
	for k := range ix {
		if !iy[k] {
			d = append(d, "-"+k)
		}
	}
	for k := range iy {
		if !ix[k] {
			d = append(d, "+"+k)
		}
	}
	if len(d) == 0 {
		return ""
	}
	return " (go build settings " + strings.Join(d, " ") + ")"
}

func readInitrd(t *testing.T, p string) []initrd.Member {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	ms, err := initrd.Read(z)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return ms
}

// copyTree copies the files git tracks or would track under root (the working tree, uncommitted changes included)
// to dst.
func copyTree(t *testing.T, root, dst string) {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "-co", "--exclude-standard").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, name := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		src := filepath.Join(root, name)
		st, err := os.Lstat(src)
		if os.IsNotExist(err) {
			continue // deleted in the working tree
		}
		if err != nil {
			t.Fatal(err)
		}
		d := filepath.Join(dst, name)
		if err := os.MkdirAll(filepath.Dir(d), 0o755); err != nil {
			t.Fatal(err)
		}
		if st.Mode()&os.ModeSymlink != 0 {
			l, err := os.Readlink(src)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(l, d); err != nil {
				t.Fatal(err)
			}
			continue
		}
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(d, b, st.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
}

func git(t *testing.T, dir, date string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=herd", "-c", "user.email=herd@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	if date != "" {
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, b)
	}
}
