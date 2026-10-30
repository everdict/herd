package image_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
)

var update = flag.Bool("update", false, "rewrite golden files")

var (
	amd64 = image.Platform{OS: "linux", Architecture: "amd64"}
	arm64 = image.Platform{OS: "linux", Architecture: "arm64"}
)

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("listing differs from %s:\n--- got\n%s--- want\n%s", p, got, want)
	}
}

// layers is a three-layer image exercising whiteouts, an opaque directory, owners, links and a dir over a dir.
func layers() [][]byte {
	return [][]byte{
		imagetest.Layer(
			imagetest.File{Name: "bin/", Type: tar.TypeDir},
			imagetest.File{Name: "bin/sh", Body: "#!busybox", Mode: 0o755},
			imagetest.File{Name: "bin/ash", Type: tar.TypeLink, Link: "bin/sh"},
			imagetest.File{Name: "etc/passwd", Body: "root:x:0:0\n"},
			imagetest.File{Name: "etc/old.conf", Body: "old"},
			imagetest.File{Name: "var/cache/a", Body: "a"},
			imagetest.File{Name: "var/cache/b", Body: "b"},
			imagetest.File{Name: "/home/app/", Type: tar.TypeDir, UID: 1000, GID: 1000, Mode: 0o750},
		),
		imagetest.Layer(
			imagetest.File{Name: "etc/.wh.old.conf"},
			imagetest.File{Name: "var/cache/.wh..wh..opq"},
			imagetest.File{Name: "var/cache/c", Body: "c"},
			imagetest.File{Name: "./usr/bin/env", Type: tar.TypeSymlink, Link: "/bin/sh"},
			imagetest.File{Name: "home/app/", Type: tar.TypeDir, UID: 1000, GID: 1000, Mode: 0o700},
		),
		imagetest.Layer(
			imagetest.File{Name: "home/app/.profile", Body: "export A=1\n", UID: 1000, GID: 1000},
			imagetest.File{Name: "bin/sh", Body: "#!busybox v2", Mode: 0o755},
		),
	}
}

func readers(ls [][]byte) []io.Reader {
	var rs []io.Reader
	for _, l := range ls {
		rs = append(rs, bytes.NewReader(l))
	}
	return rs
}

func TestFlattenGolden(t *testing.T) {
	var out bytes.Buffer
	if err := image.Flatten(readers(layers()), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	l, err := image.Listing(&out)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "flatten.golden", l)
}

func TestFlattenRefusesEscape(t *testing.T) {
	for _, f := range []imagetest.File{
		{Name: "../etc/passwd", Body: "x"},
		{Name: "a/../../b", Body: "x"},
		{Name: "bin/x", Type: tar.TypeLink, Link: "../../etc/shadow"},
	} {
		err := image.Flatten(readers([][]byte{imagetest.Layer(f)}), io.Discard, t.TempDir())
		if !errors.Is(err, image.ErrBadLayer) {
			t.Errorf("%s: err = %v, want ErrBadLayer", f.Name, err)
		}
	}
}

func newCache(t *testing.T, reg *imagetest.Registry, b *imagetest.Builder, p image.Platform) *image.Cache {
	return &image.Cache{Dir: t.TempDir(), Source: reg, Builder: b, Platform: p}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestCacheKeyedByDigest is the image-to-rootfs counterexample: the same tag pushed twice with different contents
// must give two images, each holding its own contents; a cache keyed by tag would hand out the first for both.
func TestCacheKeyedByDigest(t *testing.T) {
	ctx := context.Background()
	reg, b := imagetest.NewRegistry(), &imagetest.Builder{}
	c := newCache(t, reg, b, amd64)
	d1 := reg.Push("team/tool", "v1", imagetest.Manifest{Platform: amd64, Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "version", Body: "one"})}})
	r1, err := c.Get(ctx, "registry.example/team/tool:v1")
	if err != nil {
		t.Fatal(err)
	}
	d2 := reg.Push("team/tool", "v1", imagetest.Manifest{Platform: amd64, Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "version", Body: "two!"})}})
	if d1 == d2 {
		t.Fatal("test setup: both pushes have one digest")
	}
	r2, err := c.Get(ctx, "registry.example/team/tool:v1")
	if err != nil {
		t.Fatal(err)
	}
	if r1.Digest != d1 || r2.Digest != d2 {
		t.Fatalf("digests %s, %s; want %s, %s", r1.Digest, r2.Digest, d1, d2)
	}
	if r1.Path == r2.Path {
		t.Fatalf("two digests share one cache entry %s", r1.Path)
	}
	if got := read(t, r1.Path); got != "-0644 0:0 3 version\n" {
		t.Errorf("first image holds %q", got)
	}
	if got := read(t, r2.Path); got != "-0644 0:0 4 version\n" {
		t.Errorf("second image holds %q", got)
	}
	// Pinned by digest: the first image again, from the cache.
	r3, err := c.Get(ctx, "registry.example/team/tool:v1@"+d1)
	if err != nil {
		t.Fatal(err)
	}
	if r3.Path != r1.Path || b.Builds() != 2 {
		t.Errorf("pinned ref: path %s (want %s), %d builds (want 2)", r3.Path, r1.Path, b.Builds())
	}
}

func TestCacheGoldenImage(t *testing.T) {
	reg, b := imagetest.NewRegistry(), &imagetest.Builder{}
	reg.Push("library/base", "1", imagetest.Manifest{Platform: amd64, Layers: layers(), Config: image.Config{Cmd: []string{"/bin/sh"}}})
	r, err := newCache(t, reg, b, amd64).Get(context.Background(), "base:1")
	if err != nil {
		t.Fatal(err)
	}
	if r.FSType != "erofs" || len(r.Config.Cmd) != 1 {
		t.Errorf("rootfs %+v", r)
	}
	golden(t, "flatten.golden", read(t, r.Path))
}

func TestCachePicksPlatform(t *testing.T) {
	reg, b := imagetest.NewRegistry(), &imagetest.Builder{Type: "ext4"}
	idx := reg.Push("team/multi", "1",
		imagetest.Manifest{Platform: amd64, Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "arch", Body: "x86"})}},
		imagetest.Manifest{Platform: arm64, Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "arch", Body: "arm64"})}})
	r, err := newCache(t, reg, b, arm64).Get(context.Background(), "ghcr.io/team/multi@"+idx)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, r.Path); got != "-0644 0:0 5 arch\n" || r.FSType != "ext4" || r.Digest == idx {
		t.Errorf("arm64 rootfs %+v holds %q", r, got)
	}
}

func TestCacheVerifiesLayerDigest(t *testing.T) {
	reg, b := imagetest.NewRegistry(), &imagetest.Builder{}
	reg.Push("team/bad", "1", imagetest.Manifest{Platform: amd64, Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "f", Body: "x"})}})
	img, err := reg.Resolve(context.Background(), image.Ref{Repository: "team/bad", Tag: "1"}, amd64)
	if err != nil {
		t.Fatal(err)
	}
	reg.Corrupt(img.Layers[0].Digest)
	if _, err := newCache(t, reg, b, amd64).Get(context.Background(), "example.com/team/bad:1"); !errors.Is(err, image.ErrDigest) {
		t.Fatalf("err = %v, want ErrDigest", err)
	}
	if b.Builds() != 0 {
		t.Fatal("a corrupted layer was built")
	}
}

func TestParseRef(t *testing.T) {
	d := "sha256:" + string(bytes.Repeat([]byte("a"), 64))
	for in, want := range map[string]string{
		"alpine":                       "registry-1.docker.io/library/alpine:latest",
		"alpine:3.20":                  "registry-1.docker.io/library/alpine:3.20",
		"docker.io/team/x@" + d:        "registry-1.docker.io/team/x@" + d,
		"ghcr.io/a/b:v1@" + d:          "ghcr.io/a/b:v1@" + d,
		"localhost:5000/x:1":           "localhost:5000/x:1",
		"127.0.0.1:5000/team/tool:2.0": "127.0.0.1:5000/team/tool:2.0",
	} {
		r, err := image.ParseRef(in)
		if err != nil || r.String() != want {
			t.Errorf("ParseRef(%q) = %s, %v; want %s", in, r, err, want)
		}
	}
	for _, bad := range []string{"", "UPPER/x", "x@sha256:abc", "x:bad tag", "a//b"} {
		if _, err := image.ParseRef(bad); !errors.Is(err, image.ErrBadRef) {
			t.Errorf("ParseRef(%q) err = %v, want ErrBadRef", bad, err)
		}
	}
}
