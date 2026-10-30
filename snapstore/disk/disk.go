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
	"strings"

	"github.com/everdict/herd/snapstore"
)

// Store is a snapstore.Store rooted at a directory.
type Store struct{ root string }

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

// writeDurable writes r to path via a fsynced temporary file and a rename, then fsyncs the directory.
func writeDurable(ctx context.Context, path string, r io.Reader) (digest string, size int64, err error) {
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
	if err = tmp.Sync(); err != nil {
		return "", 0, err
	}
	if err = tmp.Close(); err != nil {
		return "", 0, err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return "", 0, err
	}
	if err = syncDir(dir); err != nil {
		return "", 0, err
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
	digest, size, err := writeDurable(ctx, p, r)
	if err != nil {
		return snapstore.Object{}, fmt.Errorf("snapstore/disk: put %s: %w", key, err)
	}
	if _, _, err := writeDurable(ctx, p+".sha256", strings.NewReader(fmt.Sprintf("%s %d\n", digest, size))); err != nil {
		return snapstore.Object{}, fmt.Errorf("snapstore/disk: put %s digest: %w", key, err)
	}
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
	return f, nil
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
