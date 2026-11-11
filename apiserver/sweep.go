package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/store"
)

// DefaultSweepGrace is Config.SweepGrace's default: an object younger than this is a write that may still be on its
// way to a record (a snapshot taken, proved and not recorded yet), and the sweep leaves it.
const DefaultSweepGrace = time.Hour

// UploadMaxAge is how old an unfinished write (an S3 multipart upload, a directory's temporary file) must be before
// the sweep aborts it. A Put of the largest snapshot finishes well inside it.
const UploadMaxAge = 24 * time.Hour

// ClaimKey is the snapshot store object that names the record store whose records decide what the sweep removes
// (A-58). It has no slash, so no snapshot key (<space>/<kind>/…) is ever it.
const ClaimKey = "herd-record-store"

// SweepReport is what one SweepSnapshots pass found and did.
type SweepReport struct {
	// Listed is how many objects the store holds (the claim aside); Named how many of them a record names; Young how
	// many no record names that are younger than the grace; Removed how many were deleted, verified gone, and
	// RemovedBytes their size; Owed how many deletes failed (the next pass tries again); Foreign how many keys are no
	// snapshot key herd writes, which the sweep leaves.
	Listed, Named, Young, Removed, Owed, Foreign int
	RemovedBytes                                 int64
	// Aborted is how many unfinished writes older than UploadMaxAge were aborted.
	Aborted int
}

// SweepSnapshots removes from the snapshot store every object that no actor, tag or template record names and that is
// older than the grace, and aborts the writes that never finished (A-58). herd-api deletes an object with the last
// record that names it (A-45), but nothing removed an object whose record never named it or no longer did: a suspend
// or tag whose answer was lost, a golden attempt from before PLT-382, an older herd's generations. The pass:
//   - runs only for the record store the snapshot store is claimed by (ClaimKey; the first pass claims it): a herd-api
//     on another record store (PLT-458) names none of the objects and would remove them all;
//   - reads every actor, tag and template record before it removes anything, and a read or parse that fails removes
//     nothing;
//   - holds refsMu throughout, so no tag can come to name an object between the read and the delete, as dropObject
//     holds it (A-45);
//   - deletes each object with the store's Delete, which answers only once a read finds it gone; a delete that fails
//     is logged owed and the next pass tries it again.
func (s *Server) SweepSnapshots(ctx context.Context) (SweepReport, error) {
	var r SweepReport
	if err := s.claimSnapshots(ctx); err != nil {
		return r, fmt.Errorf("apiserver: snapshot sweep: %w", err)
	}
	s.refsMu.Lock()
	defer s.refsMu.Unlock()
	named, err := s.namedObjects(ctx)
	if err != nil {
		return r, fmt.Errorf("apiserver: snapshot sweep: records not read, nothing removed: %w", err)
	}
	lctx, cancel := context.WithTimeout(ctx, 4*s.cfg.CallTimeout)
	objs, err := s.cfg.Snapshots.List(lctx, "")
	cancel()
	if err != nil {
		return r, fmt.Errorf("apiserver: snapshot sweep: store not listed, nothing removed: %w", err)
	}
	grace := s.cfg.SweepGrace
	if grace == 0 {
		grace = DefaultSweepGrace
	}
	cutoff := time.Now().Add(-max(grace, 0))
	for _, o := range objs {
		switch {
		case o.Key == ClaimKey:
			continue
		case snapstore.ValidateKey(o.Key) != nil:
			r.Listed++
			r.Foreign++
			log.Printf("herd-api: snapshot sweep: %q is not a snapshot key herd writes: left", o.Key)
			continue
		}
		r.Listed++
		switch {
		case named[o.Key]:
			r.Named++
			continue
		case o.Modified.After(cutoff):
			r.Young++
			continue
		}
		dctx, cancel := s.storeCall(ctx)
		err := s.cfg.Snapshots.Delete(dctx, o.Key)
		cancel()
		if err != nil {
			r.Owed++
			log.Printf("herd-api: snapshot sweep: %s (%d bytes, written %s) is named by no record; its delete is owed to the next pass: %v",
				o.Key, o.Size, o.Modified.UTC().Format(time.RFC3339), err)
			continue
		}
		r.Removed++
		r.RemovedBytes += o.Size
		log.Printf("herd-api: snapshot sweep: %s (%d bytes, written %s) is named by no record: removed, verified gone", o.Key, o.Size, o.Modified.UTC().Format(time.RFC3339))
	}
	actx, cancel := context.WithTimeout(ctx, 4*s.cfg.CallTimeout)
	n, err := s.cfg.Snapshots.AbortUploads(actx, time.Now().Add(-UploadMaxAge))
	cancel()
	r.Aborted = n
	if err != nil {
		log.Printf("herd-api: snapshot sweep: unfinished writes not all aborted (%d were): %v", n, err)
	}
	log.Printf("herd-api: snapshot sweep: %d objects, %d named by a record, %d younger than %v, %d removed (%d bytes), %d owed, %d foreign; %d unfinished writes aborted",
		r.Listed, r.Named, r.Young, grace, r.Removed, r.RemovedBytes, r.Owed, r.Foreign, r.Aborted)
	return r, nil
}

// claimSnapshots answers whether this herd-api's record store is the one the snapshot store is swept by: the store id
// ClaimKey holds, written by the first sweep when it holds none. Any other answer refuses the sweep.
func (s *Server) claimSnapshots(ctx context.Context) error {
	cctx, cancel := s.storeCall(ctx)
	defer cancel()
	owner, err := s.readClaim(cctx)
	if errors.Is(err, snapstore.ErrNotFound) {
		if _, err := s.cfg.Snapshots.Put(cctx, ClaimKey, strings.NewReader(s.storeID+"\n")); err != nil {
			return fmt.Errorf("claiming the snapshot store for record store %s: %w", s.storeID, err)
		}
		log.Printf("herd-api: snapshot sweep: the snapshot store is claimed for record store %s (%s)", s.storeID, ClaimKey)
		// Two herd-apis that claimed at once: the one whose id is not there now stops here.
		owner, err = s.readClaim(cctx)
	}
	if err != nil {
		return fmt.Errorf("reading the snapshot store's claim (%s): %w", ClaimKey, err)
	}
	if owner != s.storeID {
		return fmt.Errorf("the snapshot store is claimed by record store %s, and this herd-api answers from record store %s: "+
			"its records name none of that store's objects, so nothing is removed (if %s is gone for good, delete %s from the snapshot store)",
			owner, s.storeID, owner, ClaimKey)
	}
	return nil
}

func (s *Server) readClaim(ctx context.Context) (string, error) {
	rc, err := s.cfg.Snapshots.Open(ctx, ClaimKey)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 1024))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// namedObjects is every snapshot object a record names: an actor's Keys, its latest snapshot and what a pending start
// restores from; a tag's snapshot; a template's golden attempt. Any record kind that cannot be listed, or a record that
// cannot be parsed, fails the read.
func (s *Server) namedObjects(ctx context.Context) (map[string]bool, error) {
	named := map[string]bool{}
	actors, err := s.cfg.Store.List(ctx, store.KindActor, "")
	if err != nil {
		return nil, err
	}
	for _, r := range actors {
		ar, err := parseActor(r)
		if err != nil {
			return nil, err
		}
		for _, k := range ar.st.Keys {
			named[k] = true
		}
		if ar.st.Snapshot != nil {
			named[ar.st.Snapshot.Key] = true
		}
		if ar.st.Restore != nil {
			named[ar.st.Restore.Key] = true
		}
	}
	tags, err := s.cfg.Store.List(ctx, store.KindTag, "")
	if err != nil {
		return nil, err
	}
	for _, r := range tags {
		var st tagState
		if err := json.Unmarshal(r.State, &st); err != nil {
			return nil, fmt.Errorf("apiserver: %v: state: %w", r.Key, err)
		}
		if st.Snapshot != nil {
			named[st.Snapshot.Key] = true
		}
	}
	tpls, err := s.cfg.Store.List(ctx, store.KindActorTemplate, "")
	if err != nil {
		return nil, err
	}
	for _, r := range tpls {
		st, err := parseTemplateState(r)
		if err != nil {
			return nil, err
		}
		if st.Attempt != nil {
			named[st.Attempt.Key] = true
		}
	}
	return named, nil
}
