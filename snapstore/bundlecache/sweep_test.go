package bundlecache_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/snapstore/bundlecache"
)

func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
	}
	return n
}

// keepLocal keeps b in the cache from a file outside it, as the herdlet keeps a bundle it wrote.
func keepLocal(t *testing.T, c *bundlecache.Cache, b []byte) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "bundle")
	if err := os.WriteFile(src, b, 0o600); err != nil {
		t.Fatal(err)
	}
	d := digest(b)
	if err := c.Keep(src, d); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestTemporaryFilesCountAgainstTheBound (PLT-462, A-58): a bundle being written into the cache's directory (the
// herdlet's snapshot, CreateTemp) takes disk the bound must count, so keeping another object evicts until the kept
// objects and the temporary files together fit. On 8241c2c only kept objects counted: a 7.2 GB temporary file sat
// beside a full 20 GiB cache.
func TestTemporaryFilesCountAgainstTheBound(t *testing.T) {
	dir := t.TempDir()
	c, err := bundlecache.New(dir, 1000)
	if err != nil {
		t.Fatal(err)
	}
	keepLocal(t, c, bytes.Repeat([]byte("a"), 400))
	time.Sleep(10 * time.Millisecond)
	keepLocal(t, c, bytes.Repeat([]byte("b"), 400))
	f, err := c.CreateTemp()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(bytes.Repeat([]byte("w"), 500)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	keepLocal(t, c, bytes.Repeat([]byte("c"), 100))
	if n := dirBytes(t, dir); n > 1000 {
		t.Errorf("the cache's directory holds %d bytes with a bundle being written, over its bound of 1000", n)
	}
}

// failingStat is a store whose Stat answers neither found nor not found.
type failingStat struct{ snapstore.Store }

func (failingStat) Stat(context.Context, string) (snapstore.Object, error) {
	return snapstore.Object{}, errors.New("test: the store does not answer")
}

// TestSweepDropsABundleWhoseObjectIsGone (PLT-462, A-58): the cache records which store keys each bundle was kept
// for, and a sweep removes a bundle once every one of them answers not found (herd-api deleted the object with its
// last record, A-45, or its sweep did). A bundle one of whose keys is still there stays, so does one whose store does
// not answer, and so does one kept before the cache recorded keys (the bound evicts it in time).
func TestSweepDropsABundleWhoseObjectIsGone(t *testing.T) {
	st := store(t)
	dir := t.TempDir()
	c, err := bundlecache.New(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	gone := []byte("a golden whose tag was deleted")
	dg := put(t, st, "sp/tags/g/u/1", gone)
	read(t, c, st, "sp/tags/g/u/1", dg)
	shared := []byte("an actor's snapshot a tag references")
	ds := put(t, st, "sp/actors/a/u/1", shared)
	read(t, c, st, "sp/actors/a/u/1", ds)
	put(t, st, "sp/tags/t/u/1", shared)
	read(t, c, st, "sp/tags/t/u/1", ds)
	own := []byte("a bundle this node wrote")
	src := filepath.Join(t.TempDir(), "own")
	if err := os.WriteFile(src, own, 0o600); err != nil {
		t.Fatal(err)
	}
	do := put(t, st, "sp/actors/b/u/1", own)
	if err := c.KeepAs(src, "sp/actors/b/u/1", do); err != nil {
		t.Fatal(err)
	}
	old := keepLocal(t, c, []byte("kept before keys were recorded"))
	for _, k := range []string{"sp/tags/g/u/1", "sp/actors/a/u/1", "sp/actors/b/u/1"} {
		if err := st.Delete(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	held := func(d string) bool {
		_, err := os.Stat(filepath.Join(dir, strings.TrimPrefix(d, "sha256:")))
		return err == nil
	}
	if n, err := c.Sweep(ctx, failingStat{st}); err != nil || n != 0 || !held(dg) {
		t.Errorf("a sweep whose store does not answer removed %d (%v), golden held %v; want nothing removed", n, err, held(dg))
	}
	n, err := c.Sweep(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || held(dg) || held(do) || !held(ds) || !held(old) {
		t.Errorf("sweep removed %d; golden held %v, own held %v, shared held %v, unrecorded held %v; want the golden and own bundles removed, the shared and unrecorded kept",
			n, held(dg), held(do), held(ds), held(old))
	}
	if err := st.Delete(ctx, "sp/tags/t/u/1"); err != nil {
		t.Fatal(err)
	}
	if n, err := c.Sweep(ctx, st); err != nil || n != 1 || held(ds) {
		t.Errorf("sweep after the tag's object went removed %d (%v), shared held %v; want it removed", n, err, held(ds))
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("the cache's directory holds %v; want only the unrecorded bundle, no record of a removed one", names)
	}
}
