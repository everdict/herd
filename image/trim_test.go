package image_test

import (
	"context"
	"errors"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
)

// noise is n bytes gzip cannot shrink, so each image's layer blob is about n bytes in the cache.
func noise(seed int64, n int) string {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return string(b)
}

// trimFixture pushes three images of one 64 KiB layer each, built into 64 KiB images, and answers a cache bounded at
// 300 KiB: two images and their blobs fit, three do not.
func trimFixture(t *testing.T) (*image.Cache, []string) {
	reg := imagetest.NewRegistry()
	var refs []string
	for i, name := range []string{"a", "b", "c"} {
		reg.Push("team/"+name, "v1", imagetest.Manifest{Platform: amd64, Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "blob", Body: noise(int64(i), 64<<10)})}})
		refs = append(refs, "registry.example/team/"+name+":v1")
	}
	c := newCache(t, reg, &imagetest.Builder{Pad: 64 << 10}, amd64)
	c.MaxBytes, c.Recent = 300<<10, -1
	return c, refs
}

// get builds ref and marks it (its image and its blobs) used ago.
func get(t *testing.T, c *image.Cache, ref string, ago time.Duration) image.Rootfs {
	t.Helper()
	start := time.Now().Add(-time.Second)
	r, err := c.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("Get %s: %v", ref, err)
	}
	when := time.Now().Add(-ago)
	_ = filepath.WalkDir(c.Dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil && fi.ModTime().After(start) {
				_ = os.Chtimes(p, when, when)
			}
		}
		return nil
	})
	return r
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return !errors.Is(err, fs.ErrNotExist)
}

// cacheBytes is what the cache holds on disk, its manifest records aside.
func cacheBytes(t *testing.T, dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.Contains(p, "/manifests/") {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// TestCacheKeepsToItsBound (PLT-462, A-58): a cache past MaxBytes removes the least recently used image and its
// blob, so it holds no more than its bound. On 8241c2c nothing was ever removed: 5.3 GB and 14 GB on the two nodes.
func TestCacheKeepsToItsBound(t *testing.T) {
	c, refs := trimFixture(t)
	get(t, c, refs[0], 3*time.Minute)
	b := get(t, c, refs[1], 2*time.Minute)
	cc := get(t, c, refs[2], 0)
	if n := cacheBytes(t, c.Dir); n > c.MaxBytes {
		t.Errorf("the cache holds %d bytes, over its bound of %d", n, c.MaxBytes)
	}
	if !exists(b.Path) || !exists(cc.Path) {
		t.Errorf("b kept %v, c kept %v: the bound was kept by removing what was used more recently than a", exists(b.Path), exists(cc.Path))
	}
	if r, err := c.Get(context.Background(), refs[0]); err != nil || !exists(r.Path) {
		t.Errorf("image a asked for again = %v; want it there or built again", err)
	}
}

// TestCacheKeepsWhatAVMRuns (A-58): an image a VM runs on is never removed, however long ago a Get answered it.
func TestCacheKeepsWhatAVMRuns(t *testing.T) {
	c, refs := trimFixture(t)
	c.MaxBytes = 140 << 10 // room for one image and its blob
	a := get(t, c, refs[0], 3*time.Minute)
	c.InUse = func() []string { return []string{a.Path} }
	b := get(t, c, refs[1], 2*time.Minute)
	get(t, c, refs[2], 0)
	if !exists(a.Path) {
		t.Errorf("image a, which a VM runs, was removed")
	}
	if exists(b.Path) {
		t.Errorf("image b, which no VM runs, was kept past the bound")
	}
}

// TestCacheHitIsAUse (A-58): a Get answered from the cache counts as a use, so an image asked for again outlives one
// built after it and not asked for since; and an image used within Recent stays past the bound.
func TestCacheHitIsAUse(t *testing.T) {
	c, refs := trimFixture(t)
	a := get(t, c, refs[0], 3*time.Minute)
	get(t, c, refs[1], 2*time.Minute)
	if _, err := c.Get(context.Background(), refs[0]); err != nil {
		t.Fatal(err)
	}
	get(t, c, refs[2], 0)
	if !exists(a.Path) || cacheBytes(t, c.Dir) > c.MaxBytes {
		t.Errorf("after a hit on a: a kept %v, %d bytes held; want a kept (its use is the latest but one) within %d", exists(a.Path), cacheBytes(t, c.Dir), c.MaxBytes)
	}

	c2, refs2 := trimFixture(t)
	c2.Recent = time.Hour
	a2 := get(t, c2, refs2[0], 3*time.Minute)
	get(t, c2, refs2[1], 2*time.Minute)
	get(t, c2, refs2[2], 0)
	if !exists(a2.Path) {
		t.Errorf("an image used 3 minutes ago was removed with Recent an hour: a VM may still be booting on it")
	}
}
