// Package image turns a template's OCI images into the read-only root filesystem images a microVM boots its
// containers from (spec R-D8, ADR A-19): erofs where the host can build it, ext4 otherwise. The registry is reached
// only through Source, the filesystem tool only through Builder; both have fakes, so the logic is tested without a
// registry or mkfs. Images are cached by the digest of the platform manifest the reference resolved to, never by tag.
package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Platform selects one manifest of a multi-arch image.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// Descriptor names a blob by digest.
type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// Config is the part of an image's config herd uses to run it.
type Config struct {
	Entrypoint []string `json:"Entrypoint,omitempty"`
	Cmd        []string `json:"Cmd,omitempty"`
	Env        []string `json:"Env,omitempty"`
	WorkingDir string   `json:"WorkingDir,omitempty"`
	User       string   `json:"User,omitempty"`
}

// Image is a reference resolved for one platform.
type Image struct {
	// Digest is the platform manifest's digest: the cache key.
	Digest string
	// Index is the digest the reference named or its tag resolved to: an index's for a multi-platform image, else
	// Digest. A reference pinned to it names the same image on every platform and every host, whatever the tag does.
	Index  string `json:",omitempty"`
	Config Config
	// Layers are in order, lowest first.
	Layers []Descriptor
}

// Source is the registry boundary.
type Source interface {
	// Resolve finds the image ref names for the platform: by digest when ref carries one (an index digest is
	// narrowed to the platform's manifest), by tag otherwise.
	Resolve(ctx context.Context, ref Ref, p Platform) (Image, error)
	// Blob returns a blob's bytes as stored (a layer is usually gzip-compressed). The caller verifies the digest.
	Blob(ctx context.Context, ref Ref, d Descriptor) (io.ReadCloser, error)
}

// Builder is the filesystem-tool boundary: it turns one flattened tar into a filesystem image.
type Builder interface {
	// FSType is "erofs" or "ext4".
	FSType() string
	// Build writes the image of the tar at tarPath to outPath.
	Build(ctx context.Context, tarPath, outPath string) error
}

var (
	ErrBadRef      = errors.New("image: bad reference")
	ErrNotFound    = errors.New("image: not found")
	ErrDigest      = errors.New("image: digest mismatch")
	ErrUnsupported = errors.New("image: unsupported")
	ErrBadLayer    = errors.New("image: bad layer")
	// ErrUnavailable: the registry answered that it cannot serve now (429, 503) for longer than the Source waits.
	// Nothing is wrong with the reference or the host; the same request can succeed later.
	ErrUnavailable = errors.New("image: registry unavailable")
)

// Ref is a parsed image reference: [registry/]repository[:tag][@digest].
type Ref struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
}

var (
	digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	repoRE   = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	tagRE    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// DefaultRegistry is where a reference without a registry host is pulled from.
const DefaultRegistry = "registry-1.docker.io"

// ParseRef parses an image reference. A reference without a registry is Docker Hub's, and a one-segment repository
// there is under library/.
func ParseRef(s string) (Ref, error) {
	var r Ref
	rest := s
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		r.Digest, rest = rest[i+1:], rest[:i]
		if !digestRE.MatchString(r.Digest) {
			return Ref{}, fmt.Errorf("%w: %q: digest must be sha256:<64 hex>", ErrBadRef, s)
		}
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		if host := rest[:i]; strings.ContainsAny(host, ".:") || host == "localhost" {
			r.Registry, rest = host, rest[i+1:]
		}
	}
	if i := strings.LastIndexByte(rest, ':'); i >= 0 && strings.IndexByte(rest[i:], '/') < 0 {
		r.Tag, rest = rest[i+1:], rest[:i]
		if !tagRE.MatchString(r.Tag) {
			return Ref{}, fmt.Errorf("%w: %q: tag %q", ErrBadRef, s, r.Tag)
		}
	}
	r.Repository = rest
	if !repoRE.MatchString(r.Repository) {
		return Ref{}, fmt.Errorf("%w: %q: repository %q", ErrBadRef, s, r.Repository)
	}
	if r.Registry == "" || r.Registry == "docker.io" || r.Registry == "index.docker.io" {
		r.Registry = DefaultRegistry
		if strings.IndexByte(r.Repository, '/') < 0 {
			r.Repository = "library/" + r.Repository
		}
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r, nil
}

func (r Ref) String() string {
	s := r.Registry + "/" + r.Repository
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// ValidDigest reports whether d is a sha256 digest in OCI form.
func ValidDigest(d string) bool { return digestRE.MatchString(d) }

// Pin is ref pinned to digest (Image.Index), keeping its tag for the reader: the digest decides what is pulled. A
// reference that names a digest of its own is already pinned and is returned as written.
func Pin(ref, digest string) (string, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return "", err
	}
	if r.Digest != "" {
		return ref, nil
	}
	if !ValidDigest(digest) {
		return "", fmt.Errorf("%w: pinning %s to %q", ErrDigest, ref, digest)
	}
	r.Digest = digest
	return r.String(), nil
}
