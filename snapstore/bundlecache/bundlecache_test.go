package bundlecache_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/snapstore/bundlecache"
	"github.com/everdict/herd/snapstore/disk"
)

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// counting is a store that counts the bytes read from it.
type counting struct {
	snapstore.Store
	mu   sync.Mutex
	read int64
	gate chan struct{} // if set, every Open waits for it
}

type countReader struct {
	io.ReadCloser
	c *counting
}

func (r countReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.c.mu.Lock()
	r.c.read += int64(n)
	r.c.mu.Unlock()
	return n, err
}

func (c *counting) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if c.gate != nil {
		<-c.gate
	}
	rc, err := c.Store.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	return countReader{rc, c}, nil
}

func (c *counting) bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.read
}

func store(t *testing.T) *counting {
	t.Helper()
	d, err := disk.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d.NoSync = true
	return &counting{Store: d}
}

func put(t *testing.T, st snapstore.Store, key string, b []byte) string {
	t.Helper()
	o, err := st.Put(context.Background(), key, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return o.Digest
}

func read(t *testing.T, c *bundlecache.Cache, st snapstore.Store, key, d string) []byte {
	t.Helper()
	rc, err := c.Open(context.Background(), st, key, d)
	if err != nil {
		t.Fatalf("Open %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestFetchedOnceThenLocal: the first Open of a digest reads the store once; every later Open reads none.
func TestFetchedOnceThenLocal(t *testing.T) {
	st := store(t)
	b := bytes.Repeat([]byte("golden"), 1000)
	d := put(t, st, "tags/g/1", b)
	c, err := bundlecache.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if got := read(t, c, st, "tags/g/1", d); !bytes.Equal(got, b) {
			t.Fatalf("Open %d: wrong bytes", i)
		}
	}
	if n := st.bytes(); n != int64(len(b)) {
		t.Errorf("the store was read for %d bytes over three Opens, want %d (once)", n, len(b))
	}
	if c.Fetched() != int64(len(b)) {
		t.Errorf("Fetched = %d", c.Fetched())
	}
}

// TestKeptBundleReadsNoStore: a bundle the node wrote (Keep) is opened with no store read.
func TestKeptBundleReadsNoStore(t *testing.T) {
	st := store(t)
	b := []byte("a bundle this node took")
	d := put(t, st, "tags/g/1", b)
	c, err := bundlecache.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	f, err := c.CreateTemp()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := c.Keep(f.Name(), d); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got := read(t, c, st, "tags/g/1", d); !bytes.Equal(got, b) || st.bytes() != 0 {
		t.Errorf("Open of a kept bundle: %q, %d store bytes read; want its bytes and none", got, st.bytes())
	}
}

// TestDigestMismatchRefusedAndNotKept: an object whose bytes are not the digest asked for is refused and not kept.
func TestDigestMismatchRefusedAndNotKept(t *testing.T) {
	st := store(t)
	put(t, st, "k", []byte("what the store holds"))
	dir := t.TempDir()
	c, err := bundlecache.New(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := digest([]byte("what was recorded"))
	if _, err := c.Open(context.Background(), st, "k", want); !errors.Is(err, snapstore.ErrDigestMismatch) {
		t.Fatalf("Open = %v, want ErrDigestMismatch", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("the cache holds %d entries after a refused fetch", len(ents))
	}
}

// TestBoundEvictsLeastRecentlyUsed: the cache stays within its bound by evicting the least recently used object.
func TestBoundEvictsLeastRecentlyUsed(t *testing.T) {
	st := store(t)
	dir := t.TempDir()
	c, err := bundlecache.New(dir, 2500)
	if err != nil {
		t.Fatal(err)
	}
	ds := map[string]string{}
	for _, k := range []string{"a", "b"} {
		ds[k] = put(t, st, k, bytes.Repeat([]byte(k), 1000))
		read(t, c, st, k, ds[k])
		time.Sleep(10 * time.Millisecond)
	}
	read(t, c, st, "a", ds["a"]) // a is now the more recently used
	time.Sleep(10 * time.Millisecond)
	ds["c"] = put(t, st, "c", bytes.Repeat([]byte("c"), 1000))
	read(t, c, st, "c", ds["c"])
	var total int64
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		fi, _ := e.Info()
		total += fi.Size()
	}
	if total > 2500 {
		t.Errorf("the cache holds %d bytes, bound 2500", total)
	}
	before := st.bytes()
	read(t, c, st, "a", ds["a"])
	read(t, c, st, "c", ds["c"])
	if st.bytes() != before {
		t.Errorf("a (recently used) or c (just fetched) was evicted")
	}
	read(t, c, st, "b", ds["b"])
	if st.bytes() != before+1000 {
		t.Errorf("b (least recently used) was not the one evicted")
	}
}

// TestLargerThanBoundServedNotKept: an object larger than the bound is served from a file that goes when closed.
func TestLargerThanBoundServedNotKept(t *testing.T) {
	st := store(t)
	b := bytes.Repeat([]byte("x"), 4096)
	d := put(t, st, "big", b)
	dir := t.TempDir()
	c, err := bundlecache.New(dir, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, c, st, "big", d); !bytes.Equal(got, b) {
		t.Fatal("wrong bytes")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("the cache keeps %d entries of an object past its bound", len(ents))
	}
}

// TestConcurrentOpensFetchOnce: restores of one golden at once on a node read it from the store once.
func TestConcurrentOpensFetchOnce(t *testing.T) {
	st := store(t)
	b := bytes.Repeat([]byte("g"), 5000)
	d := put(t, st, "g", b)
	st.gate = make(chan struct{})
	c, err := bundlecache.New(filepath.Join(t.TempDir(), "bundles"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, err := c.Open(context.Background(), st, "g", d)
			if err != nil {
				errs <- err
				return
			}
			got, err := io.ReadAll(rc)
			rc.Close()
			if err != nil || !bytes.Equal(got, b) {
				errs <- errors.New("wrong bytes")
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(st.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := st.bytes(); n != int64(len(b)) {
		t.Errorf("four Opens at once read %d store bytes, want %d", n, len(b))
	}
}
