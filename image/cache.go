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
	// Index is the digest Ref named or its tag resolved to (Image.Index): what Ref is pinned to from now on (Pin).
	Index  string `json:"index,omitempty"`
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

// Get returns ref's rootfs image for the cache's platform, building it if the cache does not hold that manifest's
// image yet. A reference that names a digest is answered from the cache's manifest records (Dir/manifests, keyed by
// digest) when they hold it, so a pinned reference whose image this host has reaches no registry, and one it lacks
// is fetched by its digest; a tag is resolved on every Get (pinning it is the caller's: Rootfs.Index, Pin). Every
// blob is checked against its digest before it is used, and layers are kept by digest (Dir/blobs).
func (c *Cache) Get(ctx context.Context, ref string) (Rootfs, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return Rootfs{}, err
	}
	img, err := c.resolve(ctx, r)
	if err != nil {
		return Rootfs{}, err
	}
	fsType := c.Builder.FSType()
	key := fsType + "/" + strings.TrimPrefix(img.Digest, "sha256:")
	unlock := c.lock(key)
	defer unlock()
	out := Rootfs{Ref: ref, Digest: img.Digest, Index: img.Index, FSType: fsType, Config: img.Config,
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

// resolve answers a reference that names a digest from the manifest records, and asks the Source otherwise (or when
// no record holds it), recording what the Source answered under both the digest named and the platform manifest's.
func (c *Cache) resolve(ctx context.Context, r Ref) (Image, error) {
	if r.Digest != "" {
		if img, ok := c.record(r.Digest); ok {
			return img, nil
		}
	}
	img, err := c.Source.Resolve(ctx, r, c.Platform)
	if err != nil {
		return Image{}, fmt.Errorf("image: resolving %s for %s: %w", r, c.Platform, err)
	}
	if !ValidDigest(img.Digest) {
		return Image{}, fmt.Errorf("%w: %s resolved to manifest digest %q", ErrDigest, r, img.Digest)
	}
	if img.Index == "" {
		img.Index = img.Digest
		if r.Digest != "" {
			img.Index = r.Digest
		}
	}
	if !ValidDigest(img.Index) || r.Digest != "" && img.Index != r.Digest {
		return Image{}, fmt.Errorf("%w: %s resolved through digest %q", ErrDigest, r, img.Index)
	}
	for _, d := range []string{img.Index, img.Digest} {
		if err := c.keep(d, img); err != nil {
			return Image{}, err
		}
	}
	return img, nil
}

func (c *Cache) recordPath(digest string) string {
	p := c.Platform.OS + "-" + c.Platform.Architecture
	if c.Platform.Variant != "" {
		p += "-" + c.Platform.Variant
	}
	return filepath.Join(c.Dir, "manifests", p, strings.TrimPrefix(digest, "sha256:")+".json")
}

// record is the image the digest resolved to for the cache's platform, if a resolve recorded it. A digest names
// immutable content, so a record never goes stale; one that does not read back whole is a miss.
func (c *Cache) record(digest string) (Image, bool) {
	if !ValidDigest(digest) {
		return Image{}, false
	}
	b, err := os.ReadFile(c.recordPath(digest))
	if err != nil {
		return Image{}, false
	}
	var img Image
	if err := json.Unmarshal(b, &img); err != nil || !ValidDigest(img.Digest) || !ValidDigest(img.Index) ||
		digest != img.Digest && digest != img.Index || len(img.Layers) == 0 {
		return Image{}, false
	}
	return img, true
}

// keep writes the record of img under digest, whole or not at all.
func (c *Cache) keep(digest string, img Image) error {
	p := c.recordPath(digest)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("image: cache: %w", err)
	}
	b, err := json.Marshal(img)
	if err != nil {
		return fmt.Errorf("image: cache: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".record-")
	if err != nil {
		return fmt.Errorf("image: cache: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("image: cache: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("image: cache: %w", err)
	}
	if err := os.Rename(f.Name(), p); err != nil {
		return fmt.Errorf("image: cache: %w", err)
	}
	return nil
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
	raw, err := c.blob(ctx, r, d)
	if err != nil {
		return err
	}
	defer raw.Close()
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

// blob opens the blob d from the cache's blob store (Dir/blobs), fetching it into the store first when the store does
// not hold it or what it holds no longer hashes to d. A blob enters the store only after it was checked.
func (c *Cache) blob(ctx context.Context, r Ref, d Descriptor) (*os.File, error) {
	p := filepath.Join(c.Dir, "blobs", "sha256", strings.TrimPrefix(d.Digest, "sha256:"))
	if f, err := os.Open(p); err == nil {
		h := sha256.New()
		_, err := io.Copy(h, f)
		if _, serr := f.Seek(0, io.SeekStart); err == nil && serr == nil && "sha256:"+hex.EncodeToString(h.Sum(nil)) == d.Digest {
			return f, nil
		}
		_ = f.Close()
		_ = os.Remove(p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, fmt.Errorf("image: cache: %w", err)
	}
	rc, err := c.Source.Blob(ctx, r, d)
	if err != nil {
		return nil, fmt.Errorf("image: layer %s: %w", d.Digest, err)
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(filepath.Dir(p), ".fetch-")
	if err != nil {
		return nil, fmt.Errorf("image: cache: %w", err)
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), rc); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("image: layer %s: %w", d.Digest, err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("image: cache: %w", err)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != d.Digest {
		return nil, fmt.Errorf("%w: layer %s read as %s", ErrDigest, d.Digest, got)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return nil, fmt.Errorf("image: cache: %w", err)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("image: cache: %w", err)
	}
	return f, nil
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
