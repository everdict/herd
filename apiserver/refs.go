package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"

	"github.com/everdict/herd/store"
)

// dropObject deletes the snapshot object at key, verified gone, unless a record other than holder names it: an actor
// among its Keys, or a tag as its Snapshot (A-45). A sealed snapshot object is never written again, so a tag of a
// suspended actor records the actor's own object, and the object goes with the last record that names it. Every
// actor and tag record is read first; a List that fails or a record that cannot be parsed deletes nothing. The caller
// holds refsMu until the record that stops naming key is written, so two holders that go at once cannot each leave
// the object to the other. The store's Delete is bounded as a herdlet call is (storeCall): one that outlasts it deletes
// nothing verified, and the holder's release stays owed.
func (s *Server) dropObject(ctx context.Context, key string, holder store.Key) error {
	other, err := s.namedBy(ctx, key, holder)
	if err != nil {
		return fmt.Errorf("%s: references not read, nothing deleted: %w", key, err)
	}
	if other != nil {
		log.Printf("herd-api: snapshot %s kept: %s %s/%s names it", key, other.Kind, other.Space, other.Name)
		return nil
	}
	dctx, cancel := s.storeCall(ctx)
	defer cancel()
	return s.cfg.Snapshots.Delete(dctx, key)
}

// namedBy is the first record other than holder that names the snapshot object at key, or nil when none does.
func (s *Server) namedBy(ctx context.Context, key string, holder store.Key) (*store.Key, error) {
	actors, err := s.cfg.Store.List(ctx, store.KindActor, "")
	if err != nil {
		return nil, err
	}
	for _, r := range actors {
		if r.Key == holder {
			continue
		}
		ar, err := parseActor(r)
		if err != nil {
			return nil, err
		}
		if slices.Contains(ar.st.Keys, key) {
			return &r.Key, nil
		}
	}
	tags, err := s.cfg.Store.List(ctx, store.KindTag, "")
	if err != nil {
		return nil, err
	}
	for _, r := range tags {
		if r.Key == holder {
			continue
		}
		var st tagState
		if err := json.Unmarshal(r.State, &st); err != nil {
			return nil, fmt.Errorf("apiserver: %v: state: %w", r.Key, err)
		}
		if st.Snapshot != nil && st.Snapshot.Key == key {
			return &r.Key, nil
		}
	}
	return nil, nil
}
