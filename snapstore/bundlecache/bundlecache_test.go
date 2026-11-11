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
	// pace, if set, is how long each read takes, of at most chunk bytes: a slow link to the store.
	pace  time.Duration
	chunk int
	// hang, if set, makes the reads of the first Open block until their context ends: a store that stops sending.
	hang  bool
	opens int
}

type countReader struct {
	io.ReadCloser
	c    *counting
	ctx  context.Context
	hang bool
}

func (r countReader) Read(p []byte) (int, error) {
	if r.hang {
		<-r.ctx.Done()
		return 0, r.ctx.Err()
	}
	if r.c.pace > 0 {
		time.Sleep(r.c.pace)
		if len(p) > r.c.chunk {
			p = p[:r.c.chunk]
		}
	}
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
	c.mu.Lock()
	c.opens++
	hang := c.hang && c.opens == 1
	c.mu.Unlock()
	return countReader{rc, c, ctx, hang}, nil
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

// TestFetchOutlivesItsCaller (PLT-457): a fetch that takes longer than any one caller waits goes on after the caller
// that started it gave up, and a later Open joins it or finds its object kept, so the attempts of a restore that each
// outlast their deadline add up instead of each starting from zero. The store is read once.
func TestFetchOutlivesItsCaller(t *testing.T) {
	st := store(t)
	b := bytes.Repeat([]byte("golden bundle "), 100) // 1400 bytes
	d := put(t, st, "tags/g/1", b)
	st.pace, st.chunk = 10*time.Millisecond, 100 // about 140 ms in all
	c, err := bundlecache.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	attempts := 0
	for attempts < 10 && got == nil {
		attempts++
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		rc, err := c.Open(ctx, st, "tags/g/1", d)
		if err == nil {
			got, err = io.ReadAll(rc)
			rc.Close()
		} else if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("attempt %d: %v, want a deadline (the herdlet answers unknown, and herd-api asks again)", attempts, err)
		}
		cancel()
	}
	if got == nil {
		t.Fatalf("ten attempts of 50 ms each never opened a bundle whose fetch takes about 140 ms: every attempt fetched from zero")
	}
	if !bytes.Equal(got, b) {
		t.Fatal("wrong bytes")
	}
	if n := st.bytes(); n != int64(len(b)) {
		t.Errorf("%d attempts read %d store bytes, want %d (one fetch)", attempts, n, len(b))
	}
}

// TestStalledFetchEndsAndStartsAgain (PLT-457): a fetch that gets no bytes for the cache's stall bound ends with a
// deadline, whoever waits, and the next Open fetches again: a fetch no caller bounds is still bounded.
func TestStalledFetchEndsAndStartsAgain(t *testing.T) {
	st := store(t)
	b := []byte("a bundle behind a store that stopped sending")
	d := put(t, st, "k", b)
	st.hang = true
	c, err := bundlecache.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	c.Stall = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := c.Open(ctx, st, "k", d); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("Open of a stalled fetch = %v after %v, want a deadline within the stall bound", err, time.Since(start))
	}
	if got := read(t, c, st, "k", d); !bytes.Equal(got, b) {
		t.Fatal("the Open after a stall: wrong bytes")
	}
}
