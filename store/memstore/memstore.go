// Package memstore is an in-memory store.Store.
package memstore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/everdict/herd/store"
)

// Store is an in-memory store.Store.
type Store struct {
	mu   sync.Mutex
	recs map[store.Key]store.Record
	now  func() time.Time
}

// New returns an empty store.
func New() *Store { return &Store{recs: map[store.Key]store.Record{}, now: time.Now} }

func clone(r store.Record) store.Record {
	r.Data = slices.Clone(r.Data)
	r.State = slices.Clone(r.State)
	return r
}

func (s *Store) Get(ctx context.Context, k store.Key) (store.Record, error) {
	if err := ctx.Err(); err != nil {
		return store.Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[k]
	if !ok {
		return store.Record{}, fmt.Errorf("%v: %w", k, store.ErrNotFound)
	}
	return clone(r), nil
}

func (s *Store) Create(ctx context.Context, r store.Record) (store.Record, error) {
	if err := ctx.Err(); err != nil {
		return store.Record{}, err
	}
	if err := r.Key.Validate(); err != nil {
		return store.Record{}, err
	}
	uid, err := store.NewUID()
	if err != nil {
		return store.Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.recs[r.Key]; ok {
		return store.Record{}, fmt.Errorf("%v: %w", r.Key, store.ErrExists)
	}
	now := s.now().UTC()
	r = clone(r)
	r.UID, r.Version, r.CreateTime, r.UpdateTime = uid, 1, now, now
	s.recs[r.Key] = r
	return clone(r), nil
}

func (s *Store) check(k store.Key, uid string, version int64) (store.Record, error) {
	cur, ok := s.recs[k]
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
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.check(r.Key, r.UID, r.Version)
	if err != nil {
		return store.Record{}, err
	}
	cur.Data, cur.State = slices.Clone(r.Data), slices.Clone(r.State)
	cur.Version++
	cur.UpdateTime = s.now().UTC()
	s.recs[r.Key] = cur
	return clone(cur), nil
}

func (s *Store) Delete(ctx context.Context, k store.Key, uid string, version int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.check(k, uid, version); err != nil {
		return err
	}
	delete(s.recs, k)
	return nil
}

func (s *Store) List(ctx context.Context, kind store.Kind, space string) ([]store.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Record
	for k, r := range s.recs {
		if k.Kind == kind && (space == "" || k.Space == space) {
			out = append(out, clone(r))
		}
	}
	slices.SortFunc(out, func(a, b store.Record) int {
		if c := strings.Compare(a.Space, b.Space); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

func (s *Store) Close() error { return nil }
