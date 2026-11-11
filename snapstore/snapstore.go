// Package snapstore holds snapshot objects. herdlet writes a snapshot here and answers its key and digest; herd-api
// verifies the object against that digest before it records the snapshot, and records it before it releases the
// worker (durable before release, R-D6).
//
// Implementations: disk (a local directory, the all-in-one and shared-filesystem modes) and s3 (a bucket every host
// reaches, which the join hands to herdlets). Both run the contract in storetest.
package snapstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// Object is one stored snapshot object.
type Object struct {
	Key string
	// Digest is "sha256:<hex>" of the object's bytes.
	Digest string
	Size   int64
}

// Listed is one object a List found.
type Listed struct {
	Key  string
	Size int64
	// Modified is when the object was last written, as the store records it.
	Modified time.Time
}

// Store holds snapshot objects.
type Store interface {
	// Put stores the bytes of r under key and returns only once they are durable. A Put that fails leaves no object
	// under key that Stat would report.
	Put(ctx context.Context, key string, r io.Reader) (Object, error)
	// Stat reports the object at key or ErrNotFound.
	Stat(ctx context.Context, key string) (Object, error)
	// Verify re-reads the object and checks it against digest: ErrNotFound, ErrDigestMismatch.
	Verify(ctx context.Context, key, digest string) (Object, error)
	// Prove checks the object against digest as Verify does, but by a checksum the store keeps of the bytes it holds
	// where it keeps one, without reading them; a store that keeps none reads them (Verify). It is for an object
	// just written, whose writer's own checks rested on that same checksum (Put); Verify stays for a re-read.
	Prove(ctx context.Context, key, digest string) (Object, error)
	// Open returns the object's bytes.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the object and verifies it is gone; deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// Copy stores under to the bytes of the object at from, which must be digest's, and returns only once the copy
	// is durable and holds exactly them: ErrNotFound, ErrDigestMismatch. A Copy that fails leaves no object under to
	// that Stat would report.
	Copy(ctx context.Context, from, to, digest string) (Object, error)
	// List answers every object whose key starts with prefix, in key order: each one Stat reports, and any a Put that
	// did not finish left under a key (herd-api's sweep removes what no record names, A-58). It reads the whole
	// listing or fails; an answer is never a part of one.
	List(ctx context.Context, prefix string) ([]Listed, error)
	// AbortUploads removes what writes begun before cutoff and never finished hold out of List's and Stat's sight — an
	// S3 multipart upload's parts, a directory's temporary file — and answers how many it removed.
	AbortUploads(ctx context.Context, cutoff time.Time) (int, error)
}

var (
	ErrNotFound       = errors.New("snapstore: not found")
	ErrDigestMismatch = errors.New("snapstore: digest mismatch")
	ErrBadKey         = errors.New("snapstore: bad key")
	ErrNotDeleted     = errors.New("snapstore: delete not verified")
)

var keyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

// ValidateKey refuses keys that are not slash-separated segments of [A-Za-z0-9._-] (each starting alphanumeric, so
// no "." or ".." segment).
func ValidateKey(key string) error {
	if len(key) > 512 || !keyRE.MatchString(key) {
		return fmt.Errorf("%w: %q", ErrBadKey, key)
	}
	return nil
}

// StreamCopy is Copy through the client: it reads the object at from and Puts its bytes under to, and answers only
// if what Put stored is digest's bytes; otherwise it deletes to. Put's own promises (durable, matching its digest,
// nothing under to after a failure) make that the whole check: the source's bytes are read once.
func StreamCopy(ctx context.Context, s Store, from, to, digest string) (Object, error) {
	if err := ValidateKey(to); err != nil {
		return Object{}, err
	}
	rc, err := s.Open(ctx, from)
	if err != nil {
		return Object{}, err
	}
	defer rc.Close()
	obj, err := s.Put(ctx, to, rc)
	if err != nil {
		return Object{}, err
	}
	if obj.Digest != digest {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if derr := s.Delete(dctx, to); derr != nil {
			return Object{}, fmt.Errorf("%w: copy of %s holds %s, want %s (and removing it: %v)", ErrDigestMismatch, from, obj.Digest, digest, derr)
		}
		return Object{}, fmt.Errorf("%w: copy of %s holds %s, want %s", ErrDigestMismatch, from, obj.Digest, digest)
	}
	return obj, nil
}
