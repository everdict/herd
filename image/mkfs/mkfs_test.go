package mkfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakeRunner struct {
	paths map[string]string
	help  string
	calls [][]string
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	if p, ok := f.paths[name]; ok {
		return p, nil
	}
	return "", fmt.Errorf("%s: not found", name)
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if len(args) == 1 && args[0] == "--help" {
		return []byte(f.help), nil
	}
	return nil, nil
}

func TestDetectPrefersErofsThatReadsTars(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{paths: map[string]string{"mkfs.erofs": "/usr/bin/mkfs.erofs", "mke2fs": "/sbin/mke2fs"}, help: "  --tar=X  ..."}
	b, _, err := Detect(ctx, r)
	if err != nil || b.FSType() != "erofs" {
		t.Fatalf("Detect = %v, %v; want erofs", b, err)
	}
	r.help = "old erofs-utils"
	b, why, err := Detect(ctx, r)
	if err != nil || b.FSType() != "ext4" || !strings.Contains(why, "no --tar") {
		t.Fatalf("Detect without --tar = %v, %q, %v; want ext4", b, why, err)
	}
	r.paths = map[string]string{}
	if _, _, err := Detect(ctx, r); !errors.Is(err, ErrNoTool) {
		t.Fatalf("Detect with no tool: %v", err)
	}
}

func TestCommandLines(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "rootfs.tar")
	if err := os.WriteFile(tarPath, make([]byte, 64<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{}
	if err := (EROFS{Runner: r, Path: "mkfs.erofs"}).Build(ctx, tarPath, "out.img"); err != nil {
		t.Fatal(err)
	}
	if err := (EXT4{Runner: r, Path: "mke2fs"}).Build(ctx, tarPath, "out.img"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"mkfs.erofs", "--tar=f", "-T0", "--quiet", "out.img", tarPath},
		{"mke2fs", "-q", "-F", "-t", "ext4", "-O", "^has_journal", "-E", "root_owner=0:0", "-U", "clear", "-d", tarPath, "out.img", "98304k"},
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("calls:\n%q\nwant\n%q", r.calls, want)
	}
}

func TestReadsTar(t *testing.T) {
	for v, want := range map[string]bool{
		"mke2fs 1.47.0 (5-Feb-2023)": false, "mke2fs 1.47.1 (20-May-2024)": true, "mke2fs 1.47.2 (1-Jan-2025)": true,
		"mke2fs 1.46.5 (30-Dec-2021)": false, "mke2fs 1.48 (x)": true, "garbage": false,
	} {
		if got := readsTar(v); got != want {
			t.Errorf("readsTar(%q) = %v", v, got)
		}
	}
}

// mke2fsRunner is an mke2fs that names version, and fails -d on a tarball with tarErr when tarErr is set (e2fsprogs
// 1.47.1+ built without libarchive).
type mke2fsRunner struct {
	version string
	tarErr  string
	calls   [][]string
}

func (f *mke2fsRunner) LookPath(name string) (string, error) {
	if name == "mke2fs" {
		return "/opt/bin/mke2fs", nil
	}
	return "", fmt.Errorf("%s: not found", name)
}

func (f *mke2fsRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if len(args) == 1 && args[0] == "-V" {
		return []byte(f.version), nil
	}
	for i, a := range args {
		if a == "-d" && i+1 < len(args) {
			if fi, err := os.Stat(args[i+1]); err == nil && !fi.IsDir() && f.tarErr != "" {
				return []byte(f.tarErr), errors.New("exit status 1")
			}
		}
	}
	return nil, nil
}

// TestDetectProbesTarInput: a version that should read tars is not enough. An mke2fs 1.47.2 built without libarchive
// refuses every tarball, so Detect must try one and unpack to a directory when the tool refuses (PLT-196).
func TestDetectProbesTarInput(t *testing.T) {
	ctx := context.Background()
	r := &mke2fsRunner{version: "mke2fs 1.47.2 (1-Jan-2025)",
		tarErr: "__populate_fs_from_tar: you need libarchive to be able to process tarballs"}
	b, why, err := Detect(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := b.(EXT4); !ok || !e.FromDir {
		t.Fatalf("Detect with an mke2fs that refuses tars = %#v (%q); want ext4 from a directory", b, why)
	}
	if !strings.Contains(why, "libarchive") {
		t.Errorf("why does not say why tar input was refused: %q", why)
	}
	r.tarErr = ""
	b, why, err = Detect(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := b.(EXT4); !ok || e.FromDir {
		t.Fatalf("Detect with an mke2fs that reads tars = %#v (%q); want ext4 from the tar", b, why)
	}
}

// TestProbeTarRealMke2fs runs the probe against the host's mke2fs: one whose version predates tar input must fail it,
// so the probe exercises tar input rather than passing on any mke2fs. It skips where mke2fs is not installed.
func TestProbeTarRealMke2fs(t *testing.T) {
	ctx := context.Background()
	p, err := Exec{}.LookPath("mke2fs")
	if err != nil {
		t.Skip("no mke2fs")
	}
	v, _ := Exec{}.Run(ctx, p, "-V")
	perr := probeTar(ctx, EXT4{Runner: Exec{}, Path: p})
	t.Logf("%s: probe: %v", strings.SplitN(string(v), "\n", 2)[0], perr)
	if !readsTar(string(v)) && perr == nil {
		t.Fatalf("an mke2fs older than 1.47.1 passed the tar probe")
	}
}
