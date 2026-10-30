// Package snapstore holds snapshot objects. herdlet writes a snapshot here and answers its key and digest; herd-api
// verifies the object against that digest before it records the snapshot, and records it before it releases the
// worker (durable before release, R-D6).
//
// Implementations: disk (a local directory, the all-in-one and shared-filesystem modes). An S3 implementation comes
// later behind the same interface.
package snapstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
)

// Object is one stored snapshot object.
type Object struct {
	Key string
	// Digest is "sha256:<hex>" of the object's bytes.
	Digest string
	Size   int64
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
	// Open returns the object's bytes.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the object and verifies it is gone; deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
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
