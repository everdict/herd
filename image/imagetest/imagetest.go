// Package imagetest is an in-memory image registry (an image.Source) and an image.Builder that writes the tree's
// listing instead of a filesystem, for tests that must not reach a registry or run mkfs.
package imagetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/everdict/herd/image"
)

// File is one entry of a test layer.
type File struct {
	Name     string
	Body     string
	Mode     int64
	Type     byte
	Link     string
	UID, GID int
}

// Layer is an uncompressed tar of files.
func Layer(files ...File) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, f := range files {
		h := &tar.Header{Name: f.Name, Mode: f.Mode, Typeflag: f.Type, Linkname: f.Link, Uid: f.UID, Gid: f.GID,
			ModTime: time.Unix(1700000000, 0), Format: tar.FormatPAX}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Mode == 0 {
			h.Mode = 0o644
			if h.Typeflag == tar.TypeDir {
				h.Mode = 0o755
			}
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(f.Body))
		}
		_ = tw.WriteHeader(h)
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(f.Body))
		}
	}
	_ = tw.Close()
	return b.Bytes()
}

// Manifest is one platform's image to push.
type Manifest struct {
	Platform image.Platform
	Config   image.Config
	// Layers are uncompressed tars, lowest first; the registry stores them gzip-compressed.
	Layers [][]byte
}

type stored struct {
	platform image.Platform
	img      image.Image
}

// Registry is an in-memory registry.
type Registry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string]stored
	indexes   map[string][]string
	tags      map[string]string
	reads     map[string]int
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{blobs: map[string][]byte{}, manifests: map[string]stored{}, indexes: map[string][]string{},
		tags: map[string]string{}, reads: map[string]int{}}
}

// Push stores the manifests under repo:tag (moving the tag if it existed) and returns the digest the tag now names:
// the manifest's for one manifest, an index's for several.
func (r *Registry) Push(repo, tag string, ms ...Manifest) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var digests []string
	for _, m := range ms {
		img := image.Image{Config: m.Config}
		for _, l := range m.Layers {
			var z bytes.Buffer
			zw := gzip.NewWriter(&z)
			_, _ = zw.Write(l)
			_ = zw.Close()
			d := image.DigestOf(z.Bytes())
			r.blobs[d] = z.Bytes()
			img.Layers = append(img.Layers, image.Descriptor{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: d, Size: int64(z.Len())})
		}
		body, _ := json.Marshal(struct {
			P image.Platform
			I image.Image
		}{m.Platform, img})
		img.Digest = image.DigestOf(body)
		r.manifests[img.Digest] = stored{platform: m.Platform, img: img}
		digests = append(digests, img.Digest)
	}
	d := digests[0]
	if len(digests) > 1 {
		body, _ := json.Marshal(digests)
		d = image.DigestOf(body)
		r.indexes[d] = digests
	}
	r.tags[repo+":"+tag] = d
	return d
}

// BlobReads is how often the blob was read.
func (r *Registry) BlobReads(digest string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads[digest]
}

func (r *Registry) Resolve(ctx context.Context, ref image.Ref, p image.Platform) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return image.Image{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d := ref.Digest
	if d == "" {
		var ok bool
		if d, ok = r.tags[ref.Repository+":"+ref.Tag]; !ok {
			return image.Image{}, fmt.Errorf("%s: %w", ref, image.ErrNotFound)
		}
	}
	if list, ok := r.indexes[d]; ok {
		for _, md := range list {
			if r.manifests[md].platform == p {
				img := r.manifests[md].img
				img.Index = d
				return img, nil
			}
		}
		return image.Image{}, fmt.Errorf("%s: no manifest for %s: %w", ref, p, image.ErrNotFound)
	}
	m, ok := r.manifests[d]
	if !ok {
		return image.Image{}, fmt.Errorf("%s: %w", ref, image.ErrNotFound)
	}
	if m.platform != p {
		return image.Image{}, fmt.Errorf("%s is %s, not %s: %w", ref, m.platform, p, image.ErrNotFound)
	}
	img := m.img
	img.Index = d
	return img, nil
}

func (r *Registry) Blob(ctx context.Context, ref image.Ref, d image.Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.blobs[d.Digest]
	if !ok {
		return nil, fmt.Errorf("blob %s: %w", d.Digest, image.ErrNotFound)
	}
	r.reads[d.Digest]++
	return io.NopCloser(bytes.NewReader(b)), nil
}

// Builder writes the flattened tree's listing (image.Listing) as the "filesystem image".
type Builder struct {
	Type string

	mu     sync.Mutex
	builds int
}

func (b *Builder) FSType() string {
	if b.Type == "" {
		return "erofs"
	}
	return b.Type
}

// Builds is how many images were built.
func (b *Builder) Builds() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builds
}

func (b *Builder) Build(ctx context.Context, tarPath, outPath string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	l, err := image.Listing(f)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.builds++
	b.mu.Unlock()
	return os.WriteFile(outPath, []byte(l), 0o644)
}

// Corrupt flips a byte of a stored blob, as a registry or a proxy serving the wrong bytes would.
func (r *Registry) Corrupt(digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b := r.blobs[digest]; len(b) > 0 {
		c := append([]byte(nil), b...)
		c[len(c)-1] ^= 0xff
		r.blobs[digest] = c
	}
}
