// Package boltstore is a store.Store in one bbolt file, for the all-in-one deployment mode (ADR 0001, A-11).
//
// Every write is one bbolt read-write transaction: the uid+version precondition is checked and the record written
// inside it, and bbolt serializes those transactions and fsyncs on commit, so a write that returned is durable and no
// two writers can both pass one precondition. bbolt locks the file: one herd-api process per file. Several herd-api
// processes need a shared store (Postgres, later).
package boltstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/everdict/herd/store"
	bolt "go.etcd.io/bbolt"
)

var bucket = []byte("records")

// Store is a store.Store on a bbolt database.
type Store struct {
	db  *bolt.DB
	now func() time.Time
}

// Open opens (creating if needed) the database file at path. It waits at most timeout for another process's lock.
func Open(path string, timeout time.Duration) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: timeout})
	if err != nil {
		return nil, fmt.Errorf("boltstore: open %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucket)
		return err
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("boltstore: %s: %w", path, err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// value is a record as stored; the key holds kind, space and name.
type value struct {
	UID        string    `json:"uid"`
	Version    int64     `json:"version"`
	Data       []byte    `json:"data,omitempty"`
	State      []byte    `json:"state,omitempty"`
	CreateTime time.Time `json:"createTime"`
	UpdateTime time.Time `json:"updateTime"`
}

// The key is kind NUL space NUL name, so a prefix scan lists a kind (and a space) in space, name order.
func key(k store.Key) ([]byte, error) {
	if err := k.Validate(); err != nil {
		return nil, err
	}
	if strings.ContainsRune(string(k.Kind)+k.Space+k.Name, 0) {
		return nil, fmt.Errorf("boltstore: %q: NUL in a key", k)
	}
	return []byte(string(k.Kind) + "\x00" + k.Space + "\x00" + k.Name), nil
}

func parseKey(b []byte) store.Key {
	parts := strings.SplitN(string(b), "\x00", 3)
	if len(parts) != 3 {
		return store.Key{}
	}
	return store.Key{Kind: store.Kind(parts[0]), Space: parts[1], Name: parts[2]}
}

func decode(k store.Key, b []byte) (store.Record, error) {
	var v value
	if err := json.Unmarshal(b, &v); err != nil {
		return store.Record{}, fmt.Errorf("boltstore: %v: %w", k, err)
	}
	return store.Record{Key: k, UID: v.UID, Version: v.Version, Data: v.Data, State: v.State, CreateTime: v.CreateTime, UpdateTime: v.UpdateTime}, nil
}

func encode(r store.Record) ([]byte, error) {
	return json.Marshal(value{UID: r.UID, Version: r.Version, Data: r.Data, State: r.State, CreateTime: r.CreateTime, UpdateTime: r.UpdateTime})
}

// get reads a record inside tx: (record, true) or (zero, false) when absent.
func get(tx *bolt.Tx, k store.Key, kb []byte) (store.Record, bool, error) {
	b := tx.Bucket(bucket).Get(kb)
	if b == nil {
		return store.Record{}, false, nil
	}
	r, err := decode(k, b)
	return r, err == nil, err
}

func (s *Store) Get(ctx context.Context, k store.Key) (store.Record, error) {
	if err := ctx.Err(); err != nil {
		return store.Record{}, err
	}
	kb, err := key(k)
	if err != nil {
		return store.Record{}, err
	}
	var r store.Record
	err = s.db.View(func(tx *bolt.Tx) error {
		var ok bool
		var err error
		r, ok, err = get(tx, k, kb)
		if err == nil && !ok {
			return fmt.Errorf("%v: %w", k, store.ErrNotFound)
		}
		return err
	})
	return r, err
}

func (s *Store) Create(ctx context.Context, r store.Record) (store.Record, error) {
	if err := ctx.Err(); err != nil {
		return store.Record{}, err
	}
	kb, err := key(r.Key)
	if err != nil {
		return store.Record{}, err
	}
	uid, err := store.NewUID()
	if err != nil {
		return store.Record{}, err
	}
	now := s.now().UTC()
	r.UID, r.Version, r.CreateTime, r.UpdateTime = uid, 1, now, now
	err = s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bucket).Get(kb) != nil {
			return fmt.Errorf("%v: %w", r.Key, store.ErrExists)
		}
		b, err := encode(r)
		if err != nil {
			return err
		}
		return tx.Bucket(bucket).Put(kb, b)
	})
	if err != nil {
		return store.Record{}, err
	}
	return r, nil
}

// checked returns the stored record if its uid and version equal the precondition.
func checked(tx *bolt.Tx, k store.Key, kb []byte, uid string, version int64) (store.Record, error) {
	cur, ok, err := get(tx, k, kb)
	if err != nil {
		return store.Record{}, err
	}
	if !ok {
		return store.Record{}, fmt.Errorf("%v: %w", k, store.ErrNotFound)
	}
	if cur.UID != uid || cur.Version != version {
		return store.Record{}, &store.ConflictError{Key: k, WantUID: uid, WantVersion: version, HaveUID: cur.UID, HaveV: cur.Version}
	}
	return cur, nil
}

func (s *Store) Update(ctx context.Context, r store.Record) (store.Record, error) {
	if err := ctx.Err(); err != nil {
		return store.Record{}, err
	}
	kb, err := key(r.Key)
	if err != nil {
		return store.Record{}, err
	}
	var next store.Record
	err = s.db.Update(func(tx *bolt.Tx) error {
		cur, err := checked(tx, r.Key, kb, r.UID, r.Version)
		if err != nil {
			return err
		}
		next = cur
		next.Data, next.State = bytes.Clone(r.Data), bytes.Clone(r.State)
		next.Version++
		next.UpdateTime = s.now().UTC()
		b, err := encode(next)
		if err != nil {
			return err
		}
		return tx.Bucket(bucket).Put(kb, b)
	})
	if err != nil {
		return store.Record{}, err
	}
	return next, nil
}

func (s *Store) Delete(ctx context.Context, k store.Key, uid string, version int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	kb, err := key(k)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if _, err := checked(tx, k, kb, uid, version); err != nil {
			return err
		}
		return tx.Bucket(bucket).Delete(kb)
	})
}

func (s *Store) List(ctx context.Context, kind store.Kind, space string) ([]store.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prefix := []byte(string(kind) + "\x00")
	if space != "" {
		prefix = append(prefix, space+"\x00"...)
	}
	var out []store.Record
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucket).Cursor()
		for kb, v := c.Seek(prefix); kb != nil && bytes.HasPrefix(kb, prefix); kb, v = c.Next() {
			r, err := decode(parseKey(kb), v)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// Close closes the database file.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil && !errors.Is(err, bolt.ErrDatabaseNotOpen) {
		return err
	}
	return nil
}
