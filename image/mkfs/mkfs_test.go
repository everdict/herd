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
