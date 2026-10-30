package image

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Rootfs is a built root filesystem image.
type Rootfs struct {
	// Ref is the reference as the template wrote it.
	Ref string `json:"ref"`
	// Digest is the platform manifest's digest the image was built from.
	Digest string `json:"digest"`
	Path   string `json:"path"`
	FSType string `json:"fsType"`
	Config Config `json:"config"`
}

// Cache builds rootfs images and keeps them in Dir, keyed by filesystem type and manifest digest: two references
// that resolve to the same manifest share an image, and the same tag resolving to another digest gets its own.
type Cache struct {
	Dir      string
	Source   Source
	Builder  Builder
	Platform Platform

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (c *Cache) lock(key string) func() {
	c.mu.Lock()
	if c.locks == nil {
		c.locks = map[string]*sync.Mutex{}
	}
	l, ok := c.locks[key]
	if !ok {
		l = &sync.Mutex{}
		c.locks[key] = l
	}
	c.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Get resolves ref for the cache's platform and returns its rootfs image, building it if the cache does not hold
// that manifest's image yet. Every blob is checked against its digest before it is used.
func (c *Cache) Get(ctx context.Context, ref string) (Rootfs, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return Rootfs{}, err
	}
	img, err := c.Source.Resolve(ctx, r, c.Platform)
	if err != nil {
		return Rootfs{}, fmt.Errorf("image: resolving %s for %s: %w", ref, c.Platform, err)
	}
	if !ValidDigest(img.Digest) {
		return Rootfs{}, fmt.Errorf("%w: %s resolved to manifest digest %q", ErrDigest, ref, img.Digest)
	}
	fsType := c.Builder.FSType()
	key := fsType + "/" + strings.TrimPrefix(img.Digest, "sha256:")
	unlock := c.lock(key)
	defer unlock()
	out := Rootfs{Ref: ref, Digest: img.Digest, FSType: fsType, Config: img.Config,
		Path: filepath.Join(c.Dir, fsType, strings.TrimPrefix(img.Digest, "sha256:")+"."+fsType)}
	if _, err := os.Stat(out.Path); err == nil {
		return out, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Rootfs{}, fmt.Errorf("image: cache: %w", err)
	}
	if err := c.build(ctx, r, img, out.Path); err != nil {
		return Rootfs{}, err
	}
	return out, nil
}

func (c *Cache) build(ctx context.Context, r Ref, img Image, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("image: cache: %w", err)
	}
	work, err := os.MkdirTemp(c.Dir, ".build-")
	if err != nil {
		return fmt.Errorf("image: cache: %w", err)
	}
	defer os.RemoveAll(work)
	var layers []io.Reader
	for i, d := range img.Layers {
		p := filepath.Join(work, fmt.Sprintf("layer-%d.tar", i))
		if err := c.fetchLayer(ctx, r, d, p); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("image: cache: %w", err)
		}
		defer f.Close()
		layers = append(layers, f)
	}
	flat := filepath.Join(work, "rootfs.tar")
	f, err := os.Create(flat)
	if err != nil {
		return fmt.Errorf("image: cache: %w", err)
	}
	if err := Flatten(layers, f, work); err != nil {
		_ = f.Close()
		return fmt.Errorf("image: %s: %w", r, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("image: cache: %w", err)
	}
	tmp := filepath.Join(work, "rootfs.img")
	if err := c.Builder.Build(ctx, flat, tmp); err != nil {
		return fmt.Errorf("image: building %s for %s: %w", c.Builder.FSType(), r, err)
	}
	if err := os.Chmod(tmp, 0o444); err != nil {
		return fmt.Errorf("image: cache: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("image: cache: %w", err)
	}
	return nil
}

// fetchLayer writes a layer's uncompressed tar to p after checking the blob against its digest.
func (c *Cache) fetchLayer(ctx context.Context, r Ref, d Descriptor, p string) error {
	if !ValidDigest(d.Digest) {
		return fmt.Errorf("%w: layer digest %q", ErrDigest, d.Digest)
	}
	rc, err := c.Source.Blob(ctx, r, d)
	if err != nil {
		return fmt.Errorf("image: layer %s: %w", d.Digest, err)
	}
	defer rc.Close()
	raw, err := os.Create(p + ".blob")
	if err != nil {
		return fmt.Errorf("image: cache: %w", err)
	}
	defer os.Remove(raw.Name())
	defer raw.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(raw, h), rc); err != nil {
		return fmt.Errorf("image: layer %s: %w", d.Digest, err)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != d.Digest {
		return fmt.Errorf("%w: layer %s read as %s", ErrDigest, d.Digest, got)
	}
	if _, err := raw.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("image: cache: %w", err)
	}
	var src io.Reader = raw
	switch {
	case strings.HasSuffix(d.MediaType, "+gzip") || strings.HasSuffix(d.MediaType, ".gzip") || d.MediaType == "":
		zr, err := gzip.NewReader(raw)
		if err != nil {
			return fmt.Errorf("%w: layer %s: %v", ErrBadLayer, d.Digest, err)
		}
		defer zr.Close()
		src = zr
	case strings.HasSuffix(d.MediaType, ".tar") || strings.HasSuffix(d.MediaType, "layer.v1.tar"):
	default:
		return fmt.Errorf("%w: layer media type %q", ErrUnsupported, d.MediaType)
	}
	out, err := os.Create(p)
	if err != nil {
		return fmt.Errorf("image: cache: %w", err)
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return fmt.Errorf("%w: layer %s: %v", ErrBadLayer, d.Digest, err)
	}
	return out.Close()
}

// DigestOf is the OCI digest of b.
func DigestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// ParseConfig reads the config of an OCI image config blob.
func ParseConfig(b []byte) (Config, error) {
	var c struct {
		Config Config `json:"config"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("image: config: %w", err)
	}
	return c.Config, nil
}
