// Package bundlecache is a node's local copy of the snapshot objects it holds (PLT-227): the bundles it wrote itself
// and the ones it fetched from the snapshot store before, each kept under its digest. A restore of an object the
// node holds reads local disk and nothing from the store; a restore of one it does not hold reads the store once,
// hashing as it writes, and keeps it. The cache is bounded in bytes and evicts the least recently used objects.
package bundlecache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/everdict/herd/snapstore"
)

// Cache keeps objects under Dir/<sha256 hex>, at most MaxBytes in all.
type Cache struct {
	dir string
	max int64

	mu       sync.Mutex
	inflight map[string]chan struct{} // digest → closed when its fetch ends
	fetched  int64                    // bytes read from stores
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
	return &Cache{dir: dir, max: maxBytes, inflight: map[string]chan struct{}{}}, nil
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
// else read from st once (and kept). A fetch of the same digest in flight is waited for, not repeated. The object is
// refused (snapstore.ErrDigestMismatch) unless its bytes are digest.
func (c *Cache) Open(ctx context.Context, st snapstore.Store, key, digest string) (io.ReadCloser, error) {
	p, err := c.path(digest)
	if err != nil {
		return nil, err
	}
	for {
		if f, err := c.hit(p); err == nil {
			return f, nil
		}
		c.mu.Lock()
		wait, busy := c.inflight[digest]
		if !busy {
			done := make(chan struct{})
			c.inflight[digest] = done
			c.mu.Unlock()
			f, err := c.fetch(ctx, st, key, digest, p)
			c.mu.Lock()
			delete(c.inflight, digest)
			c.mu.Unlock()
			close(done)
			return f, err
		}
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, fmt.Errorf("bundlecache: waiting for the fetch of %s: %w", key, ctx.Err())
		}
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

func (c *Cache) fetch(ctx context.Context, st snapstore.Store, key, digest, p string) (*os.File, error) {
	rc, err := st.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("bundlecache: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), ctxReader{ctx, rc})
	c.mu.Lock()
	c.fetched += n
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("bundlecache: fetching %s: %w", key, err)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != digest {
		return nil, fmt.Errorf("%w: %s holds %s (%d bytes), want %s", snapstore.ErrDigestMismatch, key, got, n, digest)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("bundlecache: %w", err)
	}
	keep = true
	if n > c.max {
		// Too large to keep: the reader still gets it, and the file goes when it is closed.
		_ = os.Remove(tmp.Name())
		return tmp, nil
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		_ = os.Remove(tmp.Name())
		return tmp, nil
	}
	c.evict(p)
	return tmp, nil
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
		if strings.HasPrefix(e.Name(), ".") || !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
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
			total -= o.size
		}
	}
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
