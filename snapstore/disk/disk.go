// Package disk is a snapstore.Store in a local directory. Objects are written to a temporary file, fsynced, renamed
// into place and the directory fsynced; the digest is kept beside the object the same way, written after it.
package disk

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
	"time"

	"github.com/everdict/herd/snapstore"
)

// Store is a snapstore.Store rooted at a directory.
type Store struct {
	root string
	// NoSync skips the fsyncs, so a Put costs no disk flush. It is for test harnesses whose herdlet call budgets
	// (hundreds of milliseconds) must not depend on the host disk's fsync latency; a production store never sets it.
	NoSync bool
}

// New returns a store rooted at dir, creating it.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("snapstore/disk: %w", err)
	}
	return &Store{root: dir}, nil
}

func (s *Store) path(key string) (string, error) {
	if err := snapstore.ValidateKey(key); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "objects", filepath.FromSlash(key)), nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// writeDurable writes r to path via a fsynced temporary file and a rename, then fsyncs the directory (without the
// fsyncs when s.NoSync).
func (s *Store) writeDurable(ctx context.Context, path string, r io.Reader) (digest string, size int64, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", 0, err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(tmp, h), ctxReader{ctx, r})
	if err != nil {
		return "", 0, err
	}
	if !s.NoSync {
		if err = tmp.Sync(); err != nil {
			return "", 0, err
		}
	}
	if err = tmp.Close(); err != nil {
		return "", 0, err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return "", 0, err
	}
	if !s.NoSync {
		if err = syncDir(dir); err != nil {
			return "", 0, err
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), size, nil
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

func (s *Store) Put(ctx context.Context, key string, r io.Reader) (snapstore.Object, error) {
	p, err := s.path(key)
	if err != nil {
		return snapstore.Object{}, err
	}
	// The digest file is what Stat reads; remove a previous one first so a failed Put never reports old bytes.
	if err := os.Remove(p + ".sha256"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return snapstore.Object{}, fmt.Errorf("snapstore/disk: put %s: %w", key, err)
	}
	digest, size, err := s.writeDurable(ctx, p, r)
	if err != nil {
		return snapstore.Object{}, fmt.Errorf("snapstore/disk: put %s: %w", key, err)
	}
	if _, _, err := s.writeDurable(ctx, p+".sha256", strings.NewReader(fmt.Sprintf("%s %d\n", digest, size))); err != nil {
		return snapstore.Object{}, fmt.Errorf("snapstore/disk: put %s digest: %w", key, err)
	}
	snapstore.Count(ctx, size)
	return snapstore.Object{Key: key, Digest: digest, Size: size}, nil
}

func (s *Store) Stat(ctx context.Context, key string) (snapstore.Object, error) {
	if err := ctx.Err(); err != nil {
		return snapstore.Object{}, err
	}
	p, err := s.path(key)
	if err != nil {
		return snapstore.Object{}, err
	}
	b, err := os.ReadFile(p + ".sha256")
	if errors.Is(err, fs.ErrNotExist) {
		return snapstore.Object{}, fmt.Errorf("%s: %w", key, snapstore.ErrNotFound)
	}
	if err != nil {
		return snapstore.Object{}, fmt.Errorf("snapstore/disk: stat %s: %w", key, err)
	}
	o := snapstore.Object{Key: key}
	if _, err := fmt.Sscanf(string(b), "%s %d", &o.Digest, &o.Size); err != nil {
		return snapstore.Object{}, fmt.Errorf("snapstore/disk: stat %s: digest file: %w", key, err)
	}
	return o, nil
}

func (s *Store) Verify(ctx context.Context, key, digest string) (snapstore.Object, error) {
	o, err := s.Stat(ctx, key)
	if err != nil {
		return snapstore.Object{}, err
	}
	rc, err := s.Open(ctx, key)
	if err != nil {
		return snapstore.Object{}, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, ctxReader{ctx, rc})
	if err != nil {
		return snapstore.Object{}, fmt.Errorf("snapstore/disk: verify %s: %w", key, err)
	}
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != digest || got != o.Digest || n != o.Size {
		return snapstore.Object{}, fmt.Errorf("%w: %s holds %s (%d bytes), recorded %s, want %s", snapstore.ErrDigestMismatch, key, got, n, o.Digest, digest)
	}
	return o, nil
}

// Prove reads the object (Verify): a directory keeps no checksum of its own.
func (s *Store) Prove(ctx context.Context, key, digest string) (snapstore.Object, error) {
	return s.Verify(ctx, key, digest)
}

func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", key, snapstore.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("snapstore/disk: open %s: %w", key, err)
	}
	return snapstore.CountReads(ctx, f), nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := s.path(key)
	if err != nil {
		return err
	}
	for _, f := range []string{p + ".sha256", p} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("snapstore/disk: delete %s: %w", key, err)
		}
	}
	if err := syncDir(filepath.Dir(p)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("snapstore/disk: delete %s: %w", key, err)
	}
	for _, f := range []string{p, p + ".sha256"} {
		if _, err := os.Lstat(f); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s still there (%v)", snapstore.ErrNotDeleted, f, err)
		}
	}
	return nil
}

// Copy streams the object: a local copy is a read and a write of the same disk.
func (s *Store) Copy(ctx context.Context, from, to, digest string) (snapstore.Object, error) {
	return snapstore.StreamCopy(ctx, s, from, to, digest)
}

// List walks the objects directory: every file that is not a digest (.sha256) or a Put's temporary file (.tmp-*) is
// an object, whether or not its digest file was written (a Put that died between the two leaves such a file, which
// Stat does not report and the sweep must still see). Its time is the file's.
func (s *Store) List(ctx context.Context, prefix string) ([]snapstore.Listed, error) {
	root := filepath.Join(s.root, "objects")
	var out []snapstore.Listed
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == root {
				return fs.SkipAll
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() || !d.Type().IsRegular() || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".sha256") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) || snapstore.ValidateKey(key) != nil {
			return nil
		}
		fi, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil // deleted while listed
		}
		if err != nil {
			return err
		}
		out = append(out, snapstore.Listed{Key: key, Size: fi.Size(), Modified: fi.ModTime()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("snapstore/disk: list %q: %w", prefix, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// AbortUploads removes the temporary files (.tmp-*) of Puts begun before cutoff: a Put that died (a crash, a full
// disk) leaves one, which no listing or Stat reports. A newer one may be a Put still writing, and is left.
func (s *Store) AbortUploads(ctx context.Context, cutoff time.Time) (int, error) {
	root := filepath.Join(s.root, "objects")
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || !strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.ModTime().Before(cutoff) {
			return nil
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		n++
		return nil
	})
	if err != nil {
		return n, fmt.Errorf("snapstore/disk: removing stale temporary files: %w", err)
	}
	return n, nil
}
