// Package bundlecache is a node's local copy of the snapshot objects it holds (PLT-227): the bundles it wrote itself
// and the ones it fetched from the snapshot store before, each kept under its digest. A restore of an object the
// node holds reads local disk and nothing from the store; a restore of one it does not hold reads the store once,
// hashing as it writes, and keeps it. The cache is bounded in bytes and evicts the least recently used objects.
package bundlecache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/everdict/herd/snapstore"
)

// Cache keeps objects under Dir/<sha256 hex>, at most MaxBytes in all.
type Cache struct {
	// Stall bounds a fetch: one that has read no bytes from its store for this long ends with a deadline (zero:
	// DefaultStall). No caller's deadline ends a fetch (PLT-457).
	Stall time.Duration

	dir string
	max int64

	mu       sync.Mutex
	inflight map[string]*flight // digest → its fetch in progress
	fetched  int64              // bytes read from stores
}

// DefaultStall is Cache.Stall's default.
const DefaultStall = 2 * time.Minute

// flight is a fetch in progress, which the Opens of its digest wait for.
type flight struct {
	done chan struct{} // closed when the fetch ends
	err  error
	// big is the object of a fetch too large to keep, for the first waiter to take; waiters counts the Opens waiting.
	big     *os.File
	waiters int
}

// New returns a cache in dir (created) holding at most maxBytes.
func New(dir string, maxBytes int64) (*Cache, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("bundlecache: size %d", maxBytes)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("bundlecache: %w", err)
	}
	// Fetches a crash cut short leave temporary files; nothing else writes here.
	if tmps, err := filepath.Glob(filepath.Join(dir, ".tmp-*")); err == nil {
		for _, t := range tmps {
			_ = os.Remove(t)
		}
	}
	return &Cache{dir: dir, max: maxBytes, inflight: map[string]*flight{}}, nil
}

// Fetched is how many bytes the cache has read from snapshot stores.
func (c *Cache) Fetched() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetched
}

func (c *Cache) path(digest string) (string, error) {
	hx, ok := strings.CutPrefix(digest, "sha256:")
	if b, err := hex.DecodeString(hx); !ok || err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("bundlecache: digest %q is not sha256:<hex>", digest)
	}
	return filepath.Join(c.dir, hx), nil
}

// Open returns the bytes of the object st holds at key, which must be digest: from the cache when it holds digest,
// else read from st once (and kept). The object is refused (snapstore.ErrDigestMismatch) unless its bytes are digest.
//
// A fetch is not the caller's: it goes on when the Open that started it gives up (ctx ends), until it has the object or
// has read nothing for Stall, and every Open of the digest meanwhile waits for it rather than starting another. A
// restore whose fetch outlasts the deadline of its work therefore ends unknown, and the next attempt joins the fetch
// or finds the object kept, instead of fetching from zero again and again (PLT-457).
func (c *Cache) Open(ctx context.Context, st snapstore.Store, key, digest string) (io.ReadCloser, error) {
	p, err := c.path(digest)
	if err != nil {
		return nil, err
	}
	for {
		if f, err := c.hit(p); err == nil {
			c.note(p, key)
			return f, nil
		}
		c.mu.Lock()
		fl, busy := c.inflight[digest]
		if !busy {
			fl = &flight{done: make(chan struct{})}
			c.inflight[digest] = fl
			go c.fly(context.WithoutCancel(ctx), st, key, digest, p, fl)
		}
		fl.waiters++
		c.mu.Unlock()
		select {
		case <-fl.done:
			c.mu.Lock()
			fl.waiters--
			big, err := fl.big, fl.err
			fl.big = nil
			c.mu.Unlock()
			if big != nil {
				return big, nil
			}
			if err != nil {
				return nil, err
			}
			// Kept: the next pass opens it.
		case <-ctx.Done():
			c.mu.Lock()
			fl.waiters--
			c.unclaimed(fl)
			c.mu.Unlock()
			return nil, fmt.Errorf("bundlecache: waiting for the fetch of %s: %w", key, ctx.Err())
		}
	}
}

// fly runs fl's fetch and hands its end to the Opens waiting for it.
func (c *Cache) fly(ctx context.Context, st snapstore.Store, key, digest, p string, fl *flight) {
	f, kept, err := c.fetch(ctx, st, key, digest, p)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, digest)
	fl.err = err
	if f != nil {
		if kept {
			// Every waiter opens its own copy from the cache.
			_ = f.Close()
		} else {
			fl.big = f
		}
	}
	close(fl.done)
	c.unclaimed(fl)
}

// unclaimed closes the object of a fetch too large to keep once no Open waits for it any more. c.mu is held.
func (c *Cache) unclaimed(fl *flight) {
	select {
	case <-fl.done:
	default:
		return
	}
	if fl.waiters == 0 && fl.big != nil {
		_ = fl.big.Close()
		fl.big = nil
	}
}

// hit opens a kept object and marks it used.
func (c *Cache) hit(p string) (*os.File, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	return f, nil
}

// fetch reads the object from st into the cache, and returns it open, and whether it was kept (a too large one is
// served from a file that goes when it is closed). It ends with a deadline once it has read nothing for Stall.
func (c *Cache) fetch(ctx context.Context, st snapstore.Store, key, digest, p string) (*os.File, bool, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stall := c.Stall
	if stall <= 0 {
		stall = DefaultStall
	}
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(stall / 4)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			if time.Since(time.Unix(0, last.Load())) >= stall {
				cancel(fmt.Errorf("no bytes from the store for %v: %w", stall, context.DeadlineExceeded))
				return
			}
		}
	}()
	// What a fetch that ended reports: why it was ended, if it was.
	cause := func(err error) error {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return err
	}
	rc, err := st.Open(ctx, key)
	if err != nil {
		return nil, false, cause(err)
	}
	defer rc.Close()
	// A store whose read does not watch its context still ends when its reader is closed.
	unwatch := context.AfterFunc(ctx, func() { _ = rc.Close() })
	defer unwatch()
	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		return nil, false, fmt.Errorf("bundlecache: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), ctxReader{ctx, rc, &last})
	c.mu.Lock()
	c.fetched += n
	c.mu.Unlock()
	if err != nil {
		return nil, false, fmt.Errorf("bundlecache: fetching %s: %w", key, cause(err))
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != digest {
		return nil, false, fmt.Errorf("%w: %s holds %s (%d bytes), want %s", snapstore.ErrDigestMismatch, key, got, n, digest)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, false, fmt.Errorf("bundlecache: %w", err)
	}
	keep = true
	if n > c.max {
		// Too large to keep: the reader still gets it, and the file goes when it is closed.
		_ = os.Remove(tmp.Name())
		return tmp, false, nil
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		_ = os.Remove(tmp.Name())
		return tmp, false, nil
	}
	c.evict(p)
	return tmp, true, nil
}

// CreateTemp creates a temporary file in the cache's directory, for a file Keep then moves in without a copy. A
// temporary file left there is removed by the next New.
func (c *Cache) CreateTemp() (*os.File, error) {
	return os.CreateTemp(c.dir, ".tmp-*")
}

// Keep adds the file at src, whose bytes are digest, to the cache: it is moved there (copied when src is on another
// filesystem). The herdlet keeps every bundle it put in the store, so restoring its own snapshot reads no store.
func (c *Cache) Keep(src, digest string) error {
	p, err := c.path(digest)
	if err != nil {
		return err
	}
	fi, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("bundlecache: %w", err)
	}
	if fi.Size() > c.max {
		return nil
	}
	if err := os.Rename(src, p); err != nil {
		if err := copyFile(src, c.dir, p); err != nil {
			return fmt.Errorf("bundlecache: keeping %s: %w", digest, err)
		}
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	c.evict(p)
	return nil
}

func copyFile(src, dir, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// evict removes the least recently used objects until the cache fits its bound, never fresh (the one just added).
// A reader holding a removed object keeps reading it: the file goes when it is closed.
func (c *Cache) evict(fresh string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type obj struct {
		path string
		size int64
		used time.Time
	}
	var objs []obj
	var total int64
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if strings.HasPrefix(e.Name(), ".tmp-") {
			// A bundle being written or fetched here takes disk too, and is not the cache's to evict (A-58).
			total += fi.Size()
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		objs = append(objs, obj{filepath.Join(c.dir, e.Name()), fi.Size(), fi.ModTime()})
		total += fi.Size()
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].used.Before(objs[j].used) })
	for _, o := range objs {
		if total <= c.max {
			return
		}
		if o.path == fresh {
			continue
		}
		if err := os.Remove(o.path); err == nil || errors.Is(err, fs.ErrNotExist) {
			_ = os.Remove(keysPath(o.path))
			total -= o.size
		}
	}
}

// ctxReader reads r until ctx ends, and marks when it last read bytes.
type ctxReader struct {
	ctx  context.Context
	r    io.Reader
	last *atomic.Int64
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, context.Cause(c.ctx)
	}
	n, err := c.r.Read(p)
	if n > 0 {
		c.last.Store(time.Now().UnixNano())
	}
	return n, err
}

// keysPrefix names the record beside a kept object of the store keys it was kept for: .keys-<sha256 hex>.
const keysPrefix = ".keys-"

func keysPath(p string) string { return filepath.Join(filepath.Dir(p), keysPrefix+filepath.Base(p)) }

// readKeys is the store keys recorded for the object at p (nil: none recorded).
func readKeys(p string) ([]string, error) {
	b, err := os.ReadFile(keysPath(p))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []string
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, fmt.Errorf("bundlecache: %s: %w", keysPath(p), err)
	}
	return keys, nil
}

// note records that the object at p was kept or read for the store key key, so Sweep can tell when no object of the
// store holds it any more. A record that cannot be written leaves the object to the bound.
func (c *Cache) note(p, key string) {
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	keys, err := readKeys(p)
	if err != nil || slices.Contains(keys, key) {
		return
	}
	if _, err := os.Stat(p); err != nil {
		return // evicted meanwhile
	}
	b, err := json.Marshal(append(keys, key))
	if err != nil {
		return
	}
	tmp := keysPath(p) + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, keysPath(p))
	}
}

// KeepAs is Keep of a bundle that the store holds under key, recorded so Sweep removes it once that object is gone.
func (c *Cache) KeepAs(src, key, digest string) error {
	if err := c.Keep(src, digest); err != nil {
		return err
	}
	if p, err := c.path(digest); err == nil {
		c.note(p, key)
	}
	return nil
}

// Sweep removes each kept object every store key of which answers not found: herd-api deleted the snapshot object with
// the last record naming it (A-45) or swept it (A-58), so nothing restores from this copy any more (A-58). An object
// whose store does not answer for a key, or kept before the cache recorded keys, stays, and the bound evicts it in
// its turn. It answers how many objects it removed. The check is a HEAD per key, the cost the herdlet's VM snapshot
// sweep already pays per snapshot; done at eviction instead it would keep a dead bundle until the cache is full and
// then evict live ones first, since a dead bundle is often a recent one.
func (c *Cache) Sweep(ctx context.Context, st snapstore.Store) (int, error) {
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		return 0, fmt.Errorf("bundlecache: %w", err)
	}
	removed := 0
	for _, e := range ents {
		name := e.Name()
		if hx, ok := strings.CutPrefix(name, keysPrefix); ok && !strings.HasSuffix(name, ".new") {
			// A record whose object the bound evicted.
			if _, err := os.Stat(filepath.Join(c.dir, hx)); errors.Is(err, fs.ErrNotExist) {
				_ = os.Remove(filepath.Join(c.dir, name))
			}
			continue
		}
		if strings.HasPrefix(name, ".") || !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(c.dir, name)
		keys, err := readKeys(p)
		if err != nil || len(keys) == 0 {
			continue
		}
		if !allGone(ctx, st, keys) {
			continue
		}
		c.mu.Lock()
		now, err := readKeys(p)
		if err == nil && slices.Equal(now, keys) {
			err = os.Remove(p)
			if err == nil || errors.Is(err, fs.ErrNotExist) {
				_ = os.Remove(keysPath(p))
				removed++
				log.Printf("bundlecache: removed sha256:%s: no snapshot object holds it any more (%v)", name, keys)
			}
		}
		c.mu.Unlock()
	}
	return removed, ctx.Err()
}

// allGone says whether every key answers not found; any other answer keeps the object.
func allGone(ctx context.Context, st snapstore.Store, keys []string) bool {
	for _, k := range keys {
		if _, err := st.Stat(ctx, k); !errors.Is(err, snapstore.ErrNotFound) {
			return false
		}
	}
	return true
}
