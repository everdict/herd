package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A golden built by an older guest is rebuilt for the guest the workers run now (DEFAUL-1606); golden.go says how.

// startRebuild begins a rebuild of the golden of the template at k if one is due for the guests in pool, and says
// whether it did. Only Reconcile begins one, so that it can bound how many run at once.
func (s *Server) startRebuild(ctx context.Context, k store.Key, pool []string) (bool, error) {
	unlock := s.lock(k)
	defer unlock()
	rec, err := s.cfg.Store.Get(ctx, k)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	g := &goldenBuild{s: s, rec: rec}
	tpl, err := g.template()
	if err != nil {
		return false, err
	}
	st, err := parseTemplateState(rec)
	if err != nil {
		return false, err
	}
	gs := tpl.GetStatus().GetGoldenSnapshotStatus()
	if !goldenEnabled(tpl) || gs.GetGoldenTag() == nil || !goldenRebuildDue(st, pool) {
		return false, nil
	}
	st.GoldenGeneration++
	st.RebuiltFor = pool
	st.Rebuild = &goldenRebuild{Name: fmt.Sprintf("%s-%d", rec.UID, st.GoldenGeneration)}
	st.RebuildError = ""
	if err := g.saveState(ctx, st); err != nil {
		return false, err
	}
	log.Printf("herd-api: template %s/%s: golden %s was built by guest %q, which no connected worker runs (they run %q): building it again as %s; %s serves until then",
		rec.Space, rec.Name, gs.GetGoldenTag().GetName(), st.GoldenGuest, pool, st.Rebuild.Name, gs.GetGoldenTag().GetName())
	return true, nil
}

// rebuilding: a rebuild of the template's golden is in progress (also while it waits out a retry's backoff).
func rebuilding(r store.Record) bool {
	st, err := parseTemplateState(r)
	return err == nil && st.Rebuild != nil
}

// building: a rebuild of the template's golden is being built at now, which counts against Config.GoldenRebuilds; one
// that waits out a failure's backoff is not.
func building(r store.Record, now time.Time) bool {
	st, err := parseTemplateState(r)
	return err == nil && st.Rebuild != nil && (st.Rebuild.RetryAt == nil || !now.Before(*st.Rebuild.RetryAt))
}

// used records that an actor was made from the template at k now.
func (s *Server) used(k store.Key) {
	s.goldenMu.Lock()
	defer s.goldenMu.Unlock()
	if s.uses == nil {
		s.uses = map[store.Key]time.Time{}
	}
	s.uses[k] = time.Now()
}

// byUse sorts templates by when an actor was last made from them, latest first, and by name among those none was:
// the order ReconcileTemplates begins golden rebuilds in. A use is a CreateActor this process answered or, across a
// restart of herd-api, the creation of an actor the store still holds.
func (s *Server) byUse(ctx context.Context, tpls []store.Record) error {
	actors, err := s.cfg.Store.List(ctx, store.KindActor, "")
	if err != nil {
		return err
	}
	last := map[store.Key]time.Time{}
	for _, r := range actors {
		ar, err := parseActor(r)
		if err != nil {
			return err
		}
		if ar.st.TemplateName == "" || ar.st.Golden {
			continue
		}
		if k := templateKey(r.Space, ar.st.TemplateName); r.CreateTime.After(last[k]) {
			last[k] = r.CreateTime
		}
	}
	s.goldenMu.Lock()
	for k, t := range s.uses {
		if t.After(last[k]) {
			last[k] = t
		}
	}
	s.goldenMu.Unlock()
	sort.SliceStable(tpls, func(i, j int) bool {
		a, b := last[tpls[i].Key], last[tpls[j].Key]
		if !a.Equal(b) {
			return a.After(b)
		}
		return tpls[i].Key.String() < tpls[j].Key.String()
	})
	return nil
}

// retiring: the template has replaced golden tags still to delete.
func retiring(r store.Record) bool {
	st, err := parseTemplateState(r)
	return err == nil && len(st.Retired) > 0
}

// renew deletes the golden tags a rebuild replaced once it may, and advances the rebuild in progress, if any.
func (g *goldenBuild) renew(ctx context.Context) (time.Duration, bool, error) {
	if err := g.retire(ctx); err != nil {
		return 0, false, err
	}
	for {
		st, err := parseTemplateState(g.rec)
		if err != nil {
			return 0, true, err
		}
		rb := st.Rebuild
		if rb == nil {
			return 0, false, nil
		}
		if rb.RetryAt != nil {
			if time.Now().Before(*rb.RetryAt) {
				return 0, false, nil
			}
			rb.RetryAt, rb.WarmupEnd = nil, nil
			if err := g.saveState(ctx, st); err != nil {
				return 0, true, err
			}
			log.Printf("herd-api: template %s/%s: golden rebuild %s built again (failure %d)", g.rec.Space, g.rec.Name, rb.Name, rb.Failures)
			continue
		}
		again, pending, more, err := g.step(ctx, g.rebuildTarget(rb.Name))
		if !more || err != nil {
			return again, pending, err
		}
	}
}

// rebuildTarget keeps a rebuild's progress in the template's state: status.golden_tag goes on naming the old golden
// until the new tag exists, and a failure is never written to status.
func (g *goldenBuild) rebuildTarget(name string) goldenTarget {
	rebuild := func(ctx context.Context, f func(*templateState, *goldenRebuild)) error {
		st, err := parseTemplateState(g.rec)
		if err != nil {
			return err
		}
		if st.Rebuild == nil || st.Rebuild.Name != name {
			return fmt.Errorf("golden rebuild %s is no longer in progress", name)
		}
		f(&st, st.Rebuild)
		return g.saveState(ctx, st)
	}
	return goldenTarget{
		name: name,
		warmupEnd: func() *time.Time {
			if st, err := parseTemplateState(g.rec); err == nil && st.Rebuild != nil {
				return st.Rebuild.WarmupEnd
			}
			return nil
		},
		setWarmupEnd: func(ctx context.Context, t time.Time) error {
			return rebuild(ctx, func(_ *templateState, rb *goldenRebuild) { rb.WarmupEnd = &t })
		},
		tagged: func(ctx context.Context, ref *ateapi.ObjectRef, guest string) error {
			if _, err := g.s.deleteActor(ctx, ref.GetAtespace(), ref.GetName(), "", 0); err != nil && status.Code(err) != codes.NotFound {
				return fmt.Errorf("deleting golden actor: %w", err)
			}
			tpl, err := g.template()
			if err != nil {
				return err
			}
			old := tpl.GetStatus().GetGoldenSnapshotStatus().GetGoldenTag().GetName()
			st, err := parseTemplateState(g.rec)
			if err != nil {
				return err
			}
			if st.Rebuild == nil || st.Rebuild.Name != name {
				return fmt.Errorf("golden rebuild %s is no longer in progress", name)
			}
			taken := st.Rebuild.WarmupEnd
			if old != "" && old != name {
				st.Retired = append(st.Retired, retiredGolden{Name: old, At: time.Now().UTC()})
			}
			st.GoldenGuest, st.Rebuild = guest, nil
			if g.rec.State, err = json.Marshal(st); err != nil {
				return err
			}
			// The swap, in the one write that also retires the old tag: from here on an actor of the template starts
			// from the new golden.
			if err := g.checkpoint(ctx, func(gs *ateapi.GoldenSnapshotStatus) {
				gs.GoldenTag = ref
				if taken != nil {
					gs.SnapshotTime = timestamppb.New(*taken)
				}
			}); err != nil {
				return err
			}
			log.Printf("herd-api: template %s/%s: golden snapshot rebuilt: tag %s, guest %q, replacing %s", g.rec.Space, g.rec.Name, name, guest, old)
			return nil
		},
		fail: func(ctx context.Context, reason, msg string) error {
			retries := max(g.s.cfg.GoldenRetries, 0)
			var note string
			err := rebuild(ctx, func(st *templateState, rb *goldenRebuild) {
				rb.Failures++
				rb.WarmupEnd = nil
				if n := rb.Failures; n <= retries {
					at := time.Now().Add(goldenBackoff(g.s.cfg.GoldenRetryBackoff, n)).UTC()
					rb.RetryAt = &at
					note = fmt.Sprintf(" (herd-api builds it again at %s, retry %d of %d)", at.Format(time.RFC3339), n, retries)
					return
				}
				// The old golden goes on serving; a rebuild begins again only for another set of guests.
				st.Rebuild = nil
				st.RebuildError = truncateUTF8(reason+": "+msg, maxGoldenErrorMessageLen)
				note = " (not built again: its retries failed; the old golden serves)"
			})
			if err != nil {
				return err
			}
			log.Printf("herd-api: template %s/%s: golden rebuild %s failed: %s: %s%s", g.rec.Space, g.rec.Name, name, reason, msg, note)
			if _, err := g.s.deleteActor(ctx, g.rec.Space, name, "", 0); err != nil && status.Code(err) != codes.NotFound {
				log.Printf("herd-api: template %s/%s: golden actor %s not deleted yet (Reconcile retries): %v", g.rec.Space, g.rec.Name, name, err)
			}
			return nil
		},
	}
}

// retire deletes the golden tags (and their snapshots) that rebuilds replaced, each once CallTimeout has passed since
// (a create that read the old tag before the swap has recorded its actor by then) and no actor owes a start from its
// snapshot. An actor already started from it does not need it.
func (g *goldenBuild) retire(ctx context.Context) error {
	st, err := parseTemplateState(g.rec)
	if err != nil || len(st.Retired) == 0 {
		return err
	}
	var restoring map[string]bool
	keep := st.Retired[:0:0]
	for _, o := range st.Retired {
		if time.Since(o.At) < g.s.cfg.CallTimeout {
			keep = append(keep, o)
			continue
		}
		tr, err := g.s.loadTag(ctx, g.rec.Space, o.Name)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if tr.tag.GetStatus().GetActorTemplateUid() != g.rec.UID {
			continue
		}
		if restoring == nil {
			if restoring, err = g.s.restoringFrom(ctx, g.rec.Space); err != nil {
				return err
			}
		}
		if tr.st.Snapshot != nil && restoring[tr.st.Snapshot.Key] {
			keep = append(keep, o)
			continue
		}
		if _, err := g.s.deleteTag(ctx, g.rec.Space, o.Name, tr.rec.UID, 0); err != nil && status.Code(err) != codes.NotFound {
			log.Printf("herd-api: template %s/%s: replaced golden tag %s not deleted yet: %v", g.rec.Space, g.rec.Name, o.Name, err)
			keep = append(keep, o)
			continue
		}
		log.Printf("herd-api: template %s/%s: replaced golden tag %s deleted", g.rec.Space, g.rec.Name, o.Name)
	}
	if len(keep) == len(st.Retired) {
		return nil
	}
	st.Retired = keep
	return g.saveState(ctx, st)
}

// restoringFrom is the snapshot keys that actors in space owe a start from.
func (s *Server) restoringFrom(ctx context.Context, space string) (map[string]bool, error) {
	recs, err := s.cfg.Store.List(ctx, store.KindActor, space)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, r := range recs {
		ar, err := parseActor(r)
		if err != nil {
			return nil, err
		}
		if ar.st.Restore != nil {
			keys[ar.st.Restore.Key] = true
		}
	}
	return keys, nil
}

// saveState writes the template's state at the version it was read at.
func (g *goldenBuild) saveState(ctx context.Context, st templateState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	prev := g.rec.State
	g.rec.State = data
	if err := g.checkpoint(ctx, nil); err != nil {
		g.rec.State = prev
		return err
	}
	return nil
}

// rebuildLimit is how many golden rebuilds may be in progress at once.
func (s *Server) rebuildLimit() int {
	return max(s.cfg.GoldenRebuilds, 0)
}
