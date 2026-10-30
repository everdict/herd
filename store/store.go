// Package store is herd-api's record store: actors, actor templates, tags, egress policies, workers and spaces
// (atespaces). Every record carries a uid and a version; every update and delete names the uid and version it was
// read at, and a stale one is refused with ErrConflict, so two writers can never both win.
//
// Implementations: memstore (in-memory, tests and throwaway runs) and boltstore (one bbolt file, the all-in-one mode).
// Both pass storetest.Run; boltstore also storetest.RunReopen. The multi-host store (Postgres) comes later.
package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"
)

// Kind is a record type.
type Kind string

const (
	KindActor         Kind = "Actor"
	KindActorTemplate Kind = "ActorTemplate"
	KindTag           Kind = "Tag"
	KindEgressPolicy  Kind = "EgressPolicy"
	KindWorker        Kind = "Worker"
	KindSpace         Kind = "Space"
)

// Key names one record. Space is empty for global kinds (Worker, Space).
type Key struct {
	Kind  Kind
	Space string
	Name  string
}

func (k Key) String() string { return fmt.Sprintf("%s %s/%s", k.Kind, k.Space, k.Name) }

// Record is one stored resource.
type Record struct {
	Key
	// UID is assigned by Create and never changes; a record re-created under the same name has a new one.
	UID string
	// Version is 1 after Create and grows by one with every Update.
	Version int64
	// Data is the resource itself, protobuf-encoded.
	Data []byte
	// State is herd-api's own bookkeeping for the record (worker assignment, owed operations), opaque to the store.
	State      []byte
	CreateTime time.Time
	UpdateTime time.Time
}

// Store holds records. Methods are safe for concurrent use.
type Store interface {
	// Get returns the record or ErrNotFound.
	Get(ctx context.Context, k Key) (Record, error)
	// Create stores a new record with a fresh UID at version 1 and returns it; ErrExists if the key is taken.
	Create(ctx context.Context, r Record) (Record, error)
	// Update replaces Data and State of the record at r.Key, provided its stored UID and Version equal r.UID and
	// r.Version; it returns the record at the next version. ErrNotFound if absent, ErrConflict if stale.
	Update(ctx context.Context, r Record) (Record, error)
	// Delete removes the record if its UID and Version equal uid and version. ErrNotFound, ErrConflict.
	Delete(ctx context.Context, k Key, uid string, version int64) error
	// List returns the records of a kind in a space ordered by name; an empty space lists every space.
	List(ctx context.Context, kind Kind, space string) ([]Record, error)
	Close() error
}

var (
	ErrNotFound = errors.New("store: not found")
	ErrExists   = errors.New("store: already exists")
	ErrConflict = errors.New("store: stale uid or version")
)

// ConflictError is a refused precondition. errors.Is(err, ErrConflict) holds.
type ConflictError struct {
	Key                Key
	WantUID, HaveUID   string
	WantVersion, HaveV int64
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("store: %v: precondition uid %q version %d, stored uid %q version %d", e.Key, e.WantUID, e.WantVersion, e.HaveUID, e.HaveV)
}

// Is makes errors.Is(err, ErrConflict) true.
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// NewUID returns a random RFC 4122 version 4 UUID.
func NewUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: uid: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// Validate refuses a key no store should hold.
func (k Key) Validate() error {
	if k.Kind == "" || k.Name == "" {
		return fmt.Errorf("store: key needs a kind and a name, got %v", k)
	}
	return nil
}
