// Package storetest is the contract every store.Store keeps.
package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/everdict/herd/store"
)

// Run checks s against the contract. s must be empty.
func Run(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	k := store.Key{Kind: store.KindActor, Space: "s1", Name: "a1"}

	t.Run("CreateGet", func(t *testing.T) {
		r, err := s.Create(ctx, store.Record{Key: k, Data: []byte("d1"), State: []byte("s1")})
		if err != nil {
			t.Fatal(err)
		}
		if r.UID == "" || r.Version != 1 || r.CreateTime.IsZero() {
			t.Errorf("Create = %+v", r)
		}
		got, err := s.Get(ctx, k)
		if err != nil || got.UID != r.UID || got.Version != 1 || string(got.Data) != "d1" || string(got.State) != "s1" {
			t.Errorf("Get = %+v, %v", got, err)
		}
		if _, err := s.Create(ctx, store.Record{Key: k}); !errors.Is(err, store.ErrExists) {
			t.Errorf("second Create: %v, want ErrExists", err)
		}
		if _, err := s.Get(ctx, store.Key{Kind: store.KindActor, Space: "s1", Name: "nope"}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get missing: %v, want ErrNotFound", err)
		}
	})

	t.Run("StaleVersionRefused", func(t *testing.T) {
		cur, err := s.Get(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		next, err := s.Update(ctx, store.Record{Key: k, UID: cur.UID, Version: cur.Version, Data: []byte("d2")})
		if err != nil || next.Version != cur.Version+1 {
			t.Fatalf("Update = %+v, %v", next, err)
		}
		// cur is now stale: an update or delete made from it must be refused, and must change nothing.
		if _, err := s.Update(ctx, store.Record{Key: k, UID: cur.UID, Version: cur.Version, Data: []byte("lost")}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("stale Update: %v, want ErrConflict", err)
		}
		if _, err := s.Update(ctx, store.Record{Key: k, UID: "another-uid", Version: next.Version, Data: []byte("lost")}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("wrong-uid Update: %v, want ErrConflict", err)
		}
		if err := s.Delete(ctx, k, cur.UID, cur.Version); !errors.Is(err, store.ErrConflict) {
			t.Errorf("stale Delete: %v, want ErrConflict", err)
		}
		got, err := s.Get(ctx, k)
		if err != nil || string(got.Data) != "d2" || got.Version != next.Version {
			t.Errorf("after refused writes: %+v, %v", got, err)
		}
		if _, err := s.Update(ctx, store.Record{Key: store.Key{Kind: store.KindActor, Space: "s1", Name: "nope"}, UID: "x", Version: 1}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Update missing: %v, want ErrNotFound", err)
		}
	})

	t.Run("OneWriterWins", func(t *testing.T) {
		cur, err := s.Get(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		const n = 8
		var wg sync.WaitGroup
		var mu sync.Mutex
		won := 0
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.Update(ctx, store.Record{Key: k, UID: cur.UID, Version: cur.Version, Data: []byte("race")}); err == nil {
					mu.Lock()
					won++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if won != 1 {
			t.Errorf("%d of %d concurrent updates from one version won, want 1", won, n)
		}
	})

	t.Run("ListAndDelete", func(t *testing.T) {
		for _, name := range []string{"c", "b"} {
			if _, err := s.Create(ctx, store.Record{Key: store.Key{Kind: store.KindActor, Space: "s1", Name: name}}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Create(ctx, store.Record{Key: store.Key{Kind: store.KindActor, Space: "s2", Name: "z"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Create(ctx, store.Record{Key: store.Key{Kind: store.KindTag, Space: "s1", Name: "t"}}); err != nil {
			t.Fatal(err)
		}
		got, err := s.List(ctx, store.KindActor, "s1")
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, r := range got {
			names = append(names, r.Name)
		}
		if len(names) != 3 || names[0] != "a1" || names[1] != "b" || names[2] != "c" {
			t.Errorf("List = %v, want [a1 b c]", names)
		}
		all, err := s.List(ctx, store.KindActor, "")
		if err != nil || len(all) != 4 {
			t.Errorf("List all spaces = %d records, %v; want 4", len(all), err)
		}
		cur, err := s.Get(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, k, cur.UID, cur.Version); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, k); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get after Delete: %v", err)
		}
		re, err := s.Create(ctx, store.Record{Key: k})
		if err != nil || re.UID == cur.UID || re.Version != 1 {
			t.Errorf("re-Create = %+v, %v; want a new uid at version 1", re, err)
		}
	})
}

// RunReopen checks that what a store acknowledged survives closing and reopening it, preconditions included: a
// version read before the reopen is still the one to name, and a stale one is still refused. open must return the
// same underlying store each time; the first call sees it empty.
func RunReopen(t *testing.T, open func(t *testing.T) store.Store) {
	t.Helper()
	ctx := context.Background()
	a := store.Key{Kind: store.KindActor, Space: "s1", Name: "a"}
	b := store.Key{Kind: store.KindTag, Space: "s1", Name: "b"}

	s := open(t)
	ra, err := s.Create(ctx, store.Record{Key: a, Data: []byte("d1")})
	if err != nil {
		t.Fatal(err)
	}
	v1 := ra
	if ra, err = s.Update(ctx, store.Record{Key: a, UID: ra.UID, Version: ra.Version, Data: []byte("d2"), State: []byte("st2")}); err != nil {
		t.Fatal(err)
	}
	rb, err := s.Create(ctx, store.Record{Key: b, Data: []byte("tag")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, b, rb.UID, rb.Version); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t)
	got, err := s.Get(ctx, a)
	if err != nil {
		t.Fatalf("after reopen: %v", err)
	}
	if got.UID != ra.UID || got.Version != ra.Version || string(got.Data) != "d2" || string(got.State) != "st2" ||
		!got.CreateTime.Equal(ra.CreateTime) || !got.UpdateTime.Equal(ra.UpdateTime) {
		t.Errorf("after reopen: %+v, want %+v", got, ra)
	}
	if _, err := s.Get(ctx, b); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted record after reopen: %v, want ErrNotFound", err)
	}
	if _, err := s.Update(ctx, store.Record{Key: a, UID: v1.UID, Version: v1.Version, Data: []byte("lost")}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("stale update after reopen: %v, want ErrConflict", err)
	}
	ra, err = s.Update(ctx, store.Record{Key: a, UID: got.UID, Version: got.Version, Data: []byte("d3")})
	if err != nil || ra.Version != got.Version+1 {
		t.Fatalf("update after reopen = %+v, %v", ra, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t)
	defer s.Close()
	if got, err := s.Get(ctx, a); err != nil || got.Version != ra.Version || string(got.Data) != "d3" {
		t.Errorf("after second reopen: %+v, %v", got, err)
	}
	if l, err := s.List(ctx, store.KindTag, "s1"); err != nil || len(l) != 0 {
		t.Errorf("List tags after reopen = %v, %v", l, err)
	}
}
