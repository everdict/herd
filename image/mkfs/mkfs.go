// Package mkfs builds read-only root filesystem images from a flattened tar with the host's filesystem tools:
// erofs with erofs-utils (mkfs.erofs --tar, 1.7 or later) where it is installed, ext4 with e2fsprogs (mke2fs -d
// <tarball>, 1.47.1 or later and built with libarchive, which Detect checks by building from a tar) otherwise. Both read owners and modes from the tar, so no root is needed. The tools
// are reached through Runner, so tests check the command lines without running them.
package mkfs

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/everdict/herd/image"
)

// Runner runs a tool and returns its combined output.
type Runner interface {
	LookPath(name string) (string, error)
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Exec is the Runner that runs host binaries.
type Exec struct{}

func (Exec) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (Exec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// EROFS builds erofs images with mkfs.erofs.
type EROFS struct {
	Runner Runner
	// Path is mkfs.erofs's path.
	Path string
}

func (EROFS) FSType() string { return "erofs" }

func (e EROFS) Build(ctx context.Context, tarPath, outPath string) error {
	// -T0 fixes the build time stamp, so the image is a function of the tar alone.
	out, err := e.Runner.Run(ctx, e.Path, "--tar=f", "-T0", "--quiet", outPath, tarPath)
	if err != nil {
		return fmt.Errorf("mkfs.erofs: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// EXT4 builds ext4 images with mke2fs.
type EXT4 struct {
	Runner Runner
	// Path is mke2fs's path.
	Path string
	// FromDir unpacks the tar into a directory for mke2fs -d (e2fsprogs before 1.47.1 cannot read a tar); owners are
	// then kept only when the herdlet runs as root.
	FromDir bool
}

func (EXT4) FSType() string { return "ext4" }

// Size is the image size for a tar of n bytes: the tar plus half again for metadata, at least 16 MiB, in KiB.
func Size(n int64) int64 {
	k := (n + n/2) / 1024
	if k < 16<<10 {
		k = 16 << 10
	}
	return k
}

func (e EXT4) Build(ctx context.Context, tarPath, outPath string) error {
	fi, err := os.Stat(tarPath)
	if err != nil {
		return err
	}
	src := tarPath
	if e.FromDir {
		dir, err := os.MkdirTemp(filepath.Dir(outPath), ".tree-")
		if err != nil {
			return err
		}
		defer removeTree(dir)
		if err := unpack(tarPath, dir); err != nil {
			return err
		}
		src = dir
	}
	out, err := e.Runner.Run(ctx, e.Path, "-q", "-F", "-t", "ext4", "-O", "^has_journal", "-E", "root_owner=0:0",
		"-U", "clear", "-d", src, outPath, strconv.FormatInt(Size(fi.Size()), 10)+"k")
	if err != nil {
		return fmt.Errorf("mke2fs: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ErrNoTool is answered when the host has neither tool in a version that reads tars.
var ErrNoTool = errors.New("mkfs: no filesystem tool that builds from a tar (mkfs.erofs >= 1.7 or mke2fs >= 1.47.1)")

// Detect picks the builder: erofs when mkfs.erofs is installed and reads tars, ext4 when mke2fs is. why says what
// was found, for herdlet doctor.
func Detect(ctx context.Context, r Runner) (b image.Builder, why string, err error) {
	var notes []string
	if p, err := r.LookPath("mkfs.erofs"); err == nil {
		out, _ := r.Run(ctx, p, "--help")
		if strings.Contains(string(out), "--tar") {
			return EROFS{Runner: r, Path: p}, "mkfs.erofs at " + p + " reads tars", nil
		}
		notes = append(notes, "mkfs.erofs at "+p+" has no --tar (erofs-utils < 1.7)")
	} else {
		notes = append(notes, "no mkfs.erofs")
	}
	for _, name := range []string{"mke2fs", "mkfs.ext4"} {
		if p, err := r.LookPath(name); err == nil {
			out, _ := r.Run(ctx, p, "-V")
			if readsTar(string(out)) {
				// The version is not enough: e2fsprogs built without libarchive refuses every tarball, so try one.
				perr := probeTar(ctx, EXT4{Runner: r, Path: p})
				if perr == nil {
					return EXT4{Runner: r, Path: p}, strings.Join(append(notes, name+" at "+p+" reads tars"), "; "), nil
				}
				return EXT4{Runner: r, Path: p, FromDir: true}, strings.Join(append(notes, name+" at "+p+" refused a tar ("+perr.Error()+"): unpacking to a directory"), "; "), nil
			}
			return EXT4{Runner: r, Path: p, FromDir: true}, strings.Join(append(notes, name+" at "+p+" predates tar input (< 1.47.1): unpacking to a directory"), "; "), nil
		}
	}
	notes = append(notes, "no mke2fs")
	return nil, strings.Join(notes, "; "), fmt.Errorf("%w: %s", ErrNoTool, strings.Join(notes, "; "))
}

// probeTar builds an image with e from a one-file tar in a scratch directory, so e must read the tar itself.
func probeTar(ctx context.Context, e EXT4) error {
	dir, err := os.MkdirTemp("", "herd-mkfs-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	tarPath := filepath.Join(dir, "probe.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	body := []byte("probe\n")
	err = tw.WriteHeader(&tar.Header{Name: "probe", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	if err == nil {
		_, err = tw.Write(body)
	}
	if err == nil {
		err = tw.Close()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return e.Build(ctx, tarPath, filepath.Join(dir, "probe.img"))
}

var versionRE = regexp.MustCompile(`mke2fs (\d+)\.(\d+)(?:\.(\d+))?`)

// readsTar says whether mke2fs -V names a version that takes a tarball for -d (1.47.1 and later).
func readsTar(v string) bool {
	m := versionRE.FindStringSubmatch(v)
	if m == nil {
		return false
	}
	n := func(s string) int { i, _ := strconv.Atoi(s); return i }
	maj, min, patch := n(m[1]), n(m[2]), n(m[3])
	return maj > 1 || maj == 1 && (min > 47 || min == 47 && patch >= 1)
}
