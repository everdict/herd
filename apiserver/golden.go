package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"
	"unicode/utf8"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Golden snapshots follow Substrate's template reconciler at the pinned commit
// (cmd/ateapi/internal/controlapi/template_reconciler.go): every template whose golden_snapshot_config.mode is not
// DISABLED (an omitted config or UNSPECIFIED is stored as ENABLED, defaults.Apply) gets a golden actor named by the
// template's uid in its atespace; once it has run for the warmup, a PUBLISHED tag of the same name takes its
// snapshot, the golden actor is deleted, and status.golden_snapshot_status.golden_tag names the tag. A build that
// cannot succeed ends in error_message "<Reason>: <detail>". A tag is terminal; an error is herd's to retry
// (DEFAUL-1601): Reconcile builds a failed golden again after Config.GoldenRetryBackoff, doubled for each retry, up to
// Config.GoldenRetries times, and the message says when, so a transient failure heals with no change to the
// template and a permanent one ends. CreateActor without a source_tag then starts from the golden tag.

// Reasons of a failed golden build, Substrate's where it has one.
const (
	reasonGoldenTagConflict    = "GoldenTagConflict"
	reasonGoldenActorInvalid   = "GoldenActorInvalid"
	reasonGoldenActorCrashed   = "GoldenActorCrashed"
	reasonUnexpectedState      = "GoldenActorUnexpectedState"
	reasonGoldenSnapshotFailed = "GoldenSnapshotFailed"
)

// maxGoldenErrorMessageLen is the maxLength of GoldenSnapshotStatus.error_message.
const maxGoldenErrorMessageLen = 4096

// Config.GoldenRetries and Config.GoldenRetryBackoff defaults: retries 1, 2, 4, 8 and 16 minutes after a failure.
const (
	DefaultGoldenRetries      = 5
	DefaultGoldenRetryBackoff = time.Minute
)

// templateState is herd-api's bookkeeping for a template (store.Record.State): its failed golden builds.
type templateState struct {
	// GoldenFailures counts the template's golden builds that failed.
	GoldenFailures int `json:"goldenFailures,omitempty"`
	// GoldenRetryAt is when Reconcile builds the failed golden again; nil: not again.
	GoldenRetryAt *time.Time `json:"goldenRetryAt,omitempty"`
}

func parseTemplateState(r store.Record) (templateState, error) {
	var st templateState
	if len(r.State) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(r.State, &st); err != nil {
		return st, fmt.Errorf("apiserver: %v: state: %w", r.Key, err)
	}
	return st, nil
}

// goldenRetryDue: the template's golden build failed and its retry is due.
func goldenRetryDue(r store.Record, gs *ateapi.GoldenSnapshotStatus, now time.Time) bool {
	if gs.GetErrorMessage() == "" || gs.GetGoldenTag() != nil {
		return false
	}
	st, err := parseTemplateState(r)
	return err == nil && st.GoldenRetryAt != nil && !now.Before(*st.GoldenRetryAt)
}

// goldenBackoff is the wait before retry n (from 1): the backoff, doubled for each retry before it, at most a day.
func goldenBackoff(base time.Duration, n int) time.Duration {
	d := base
	for i := 1; i < n && d < 24*time.Hour; i++ {
		d *= 2
	}
	return min(d, 24*time.Hour)
}

func templateKey(space, name string) store.Key {
	return store.Key{Kind: store.KindActorTemplate, Space: space, Name: name}
}

// goldenEnabled: Substrate builds a golden snapshot unless the mode is DISABLED.
func goldenEnabled(t *ateapi.ActorTemplate) bool {
	return t.GetSnapshotConfig().GetGoldenSnapshotConfig().GetMode() != ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED
}

// goldenDone: the golden build reached a terminal state, a tag or an error.
func goldenDone(gs *ateapi.GoldenSnapshotStatus) bool {
	return gs.GetGoldenTag() != nil || gs.GetErrorMessage() != ""
}

// defaultGolden stores an omitted or UNSPECIFIED golden snapshot mode as ENABLED, as Substrate's defaults.Apply does.
func defaultGolden(t *ateapi.ActorTemplate) {
	if t.SnapshotConfig == nil {
		t.SnapshotConfig = &ateapi.SnapshotConfig{}
	}
	if t.SnapshotConfig.GoldenSnapshotConfig == nil {
		t.SnapshotConfig.GoldenSnapshotConfig = &ateapi.GoldenSnapshotConfig{}
	}
	if t.SnapshotConfig.GoldenSnapshotConfig.Mode == ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_UNSPECIFIED {
		t.SnapshotConfig.GoldenSnapshotConfig.Mode = ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED
	}
}

// kickGolden runs a golden pass for the template at k after the given delay, in the background.
func (s *Server) kickGolden(k store.Key, after time.Duration) {
	s.goldenMu.Lock()
	defer s.goldenMu.Unlock()
	if s.closed {
		return
	}
	if t := s.goldenTimers[k]; t != nil {
		t.Stop()
		delete(s.goldenTimers, k)
	}
	if after > 0 {
		s.goldenTimers[k] = time.AfterFunc(after, func() { s.kickGolden(k, 0) })
		return
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		again, _, err := s.goldenPass(context.Background(), k)
		if err != nil {
			// Reconcile retries it.
			log.Printf("herd-api: template %s/%s: golden snapshot not built yet: %v", k.Space, k.Name, err)
		}
		if again > 0 {
			s.kickGolden(k, again)
		}
	}()
}

// goldenPass advances the golden build of the template at k as far as it can go now. again > 0 asks to look once
// more after that long (the warmup); pending says the build has not reached a terminal state. An error is a step
// that did not converge and is retried (by Reconcile); a build that cannot succeed is not an error but a terminal
// error_message.
func (s *Server) goldenPass(ctx context.Context, k store.Key) (again time.Duration, pending bool, err error) {
	unlock := s.lock(k)
	defer unlock()
	rec, err := s.cfg.Store.Get(ctx, k)
	if errors.Is(err, store.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, true, err
	}
	g := &goldenBuild{s: s, rec: rec}
	return g.run(ctx)
}

type goldenBuild struct {
	s   *Server
	rec store.Record
}

func (g *goldenBuild) template() (*ateapi.ActorTemplate, error) {
	return decode(g.rec, &ateapi.ActorTemplate{})
}

func (g *goldenBuild) run(ctx context.Context) (time.Duration, bool, error) {
	s, space, uid := g.s, g.rec.Space, g.rec.UID
	ref := &ateapi.ObjectRef{Atespace: space, Name: uid}
	for {
		tpl, err := g.template()
		if err != nil {
			return 0, true, err
		}
		gs := tpl.GetStatus().GetGoldenSnapshotStatus()
		if goldenEnabled(tpl) && goldenRetryDue(g.rec, gs, time.Now()) {
			if err := g.retry(ctx, gs.GetErrorMessage()); err != nil {
				return 0, true, err
			}
			continue
		}
		if !goldenEnabled(tpl) || goldenDone(gs) {
			if st, _ := parseTemplateState(g.rec); !goldenEnabled(tpl) || gs.GetGoldenTag() != nil || st.GoldenRetryAt == nil {
				// The build ended for good; a retry still to come boots with the template's image_pull_auth.
				s.dropPullAuth(uid)
			}
			return 0, false, nil
		}
		// A complete tag survives a failure between the tag and the status write.
		tag, err := s.loadTag(ctx, space, uid)
		switch {
		case err == nil:
			if tag.tag.GetStatus().GetActorTemplateUid() != uid || tag.tag.GetSourceActor().GetName() != uid {
				return 0, false, g.fail(ctx, reasonGoldenTagConflict, "golden tag belongs to another actor or template")
			}
			err := g.saveTag(ctx, ref)
			return 0, err != nil, err
		case !errors.Is(err, store.ErrNotFound):
			return 0, true, err
		}
		ar, err := s.loadActor(ctx, space, uid)
		if errors.Is(err, store.ErrNotFound) {
			_, err := s.createActor(ctx, &ateapi.Actor{
				Metadata:      &ateapi.ResourceMetadata{Atespace: space, Name: uid},
				ActorTemplate: &ateapi.ObjectRef{Atespace: space, Name: g.rec.Name},
			}, true)
			switch status.Code(err) {
			case codes.OK, codes.AlreadyExists:
				continue
			case codes.InvalidArgument, codes.FailedPrecondition:
				// The template cannot boot (its herdlet refused it for good); retrying cannot help.
				return 0, false, g.fail(ctx, reasonGoldenActorInvalid, status.Convert(err).Message())
			}
			return 0, true, fmt.Errorf("creating golden actor: %w", err)
		}
		if err != nil {
			return 0, true, err
		}
		switch st := ar.actor.GetStatus().GetState(); st {
		case ateapi.ActorState_ACTOR_STATE_CRASHED:
			return 0, false, g.fail(ctx, reasonGoldenActorCrashed, "golden actor crashed before its snapshot was taken")
		case ateapi.ActorState_ACTOR_STATE_RUNNING:
			if gs.GetSnapshotTime() == nil {
				deadline := time.Now().Add(s.cfg.GoldenWarmup)
				if err := g.checkpoint(ctx, func(gs *ateapi.GoldenSnapshotStatus) { gs.SnapshotTime = timestamppb.New(deadline) }); err != nil {
					return 0, true, err
				}
				continue
			}
			if rem := time.Until(gs.GetSnapshotTime().AsTime()); rem > 0 {
				return rem, true, nil
			}
			fallthrough
		case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
			// A running golden actor is snapshotted in place at the highest fidelity its worker keeps (capped by the
			// template's preference); a suspended one has its snapshot copied.
			_, err := s.createTag(ctx, &ateapi.Tag{Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: uid}, SourceActor: ref},
				ateapi.TagScope_TAG_SCOPE_PUBLISHED)
			switch status.Code(err) {
			case codes.OK, codes.AlreadyExists:
				continue
			case codes.FailedPrecondition:
				return 0, false, g.fail(ctx, reasonGoldenSnapshotFailed, status.Convert(err).Message())
			}
			return 0, true, fmt.Errorf("tagging golden actor: %w", err)
		case ateapi.ActorState_ACTOR_STATE_RESUMING, ateapi.ActorState_ACTOR_STATE_SUSPENDING, ateapi.ActorState_ACTOR_STATE_DELETING:
			// An owed boot or suspend, or the delete of a failed build's golden actor before a retry: Reconcile settles
			// the actor, and the next pass goes on from there.
			return 0, true, fmt.Errorf("golden actor is %v: %s", st, owedReason(ar))
		default:
			return 0, false, g.fail(ctx, reasonUnexpectedState, fmt.Sprintf("golden actor in unexpected state %v", st))
		}
	}
}

func owedReason(ar *actorRec) string {
	if o := ar.st.Owed; o != nil {
		return o.Op + " owed: " + o.Reason
	}
	return "no step owed"
}

// saveTag deletes the golden actor and only then records the tag, so a retry finds the tag again if either fails.
func (g *goldenBuild) saveTag(ctx context.Context, ref *ateapi.ObjectRef) error {
	if _, err := g.s.deleteActor(ctx, ref.GetAtespace(), ref.GetName(), "", 0); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("deleting golden actor: %w", err)
	}
	if err := g.checkpoint(ctx, func(gs *ateapi.GoldenSnapshotStatus) { gs.GoldenTag = ref }); err != nil {
		return err
	}
	g.s.dropPullAuth(g.rec.UID)
	log.Printf("herd-api: template %s/%s: golden snapshot built: tag %s", g.rec.Space, g.rec.Name, ref.GetName())
	return nil
}

// fail records the error message, prefixed with its reason and cut to error_message's bound, schedules the build's
// retry if it has one left and says so in the message, and lets the golden actor go: nothing will snapshot it any
// more, and it holds a slot.
func (g *goldenBuild) fail(ctx context.Context, reason, msg string) error {
	st, err := parseTemplateState(g.rec)
	if err != nil {
		return err
	}
	st.GoldenFailures++
	st.GoldenRetryAt = nil
	retries := max(g.s.cfg.GoldenRetries, 0)
	note := ""
	if n := st.GoldenFailures; n <= retries {
		at := time.Now().Add(goldenBackoff(g.s.cfg.GoldenRetryBackoff, n)).UTC()
		st.GoldenRetryAt = &at
		note = fmt.Sprintf(" (herd-api builds it again at %s, retry %d of %d)", at.Format(time.RFC3339), n, retries)
	} else if retries > 0 {
		note = fmt.Sprintf(" (not built again: its %d retries failed)", retries)
	}
	if g.rec.State, err = json.Marshal(st); err != nil {
		return err
	}
	m := truncateUTF8(reason+": "+msg, maxGoldenErrorMessageLen-len(note)) + note
	if err := g.checkpoint(ctx, func(gs *ateapi.GoldenSnapshotStatus) { gs.ErrorMessage = m }); err != nil {
		return err
	}
	if st.GoldenRetryAt == nil {
		g.s.dropPullAuth(g.rec.UID)
	}
	log.Printf("herd-api: template %s/%s: golden snapshot failed: %s", g.rec.Space, g.rec.Name, m)
	if _, err := g.s.deleteActor(ctx, g.rec.Space, g.rec.UID, "", 0); err != nil && status.Code(err) != codes.NotFound {
		log.Printf("herd-api: template %s/%s: golden actor not deleted yet (Reconcile retries): %v", g.rec.Space, g.rec.Name, err)
	}
	return nil
}

// retry clears a failed build's error so that the build runs again from the start, with no change to the template.
func (g *goldenBuild) retry(ctx context.Context, failed string) error {
	st, err := parseTemplateState(g.rec)
	if err != nil {
		return err
	}
	st.GoldenRetryAt = nil
	if g.rec.State, err = json.Marshal(st); err != nil {
		return err
	}
	if err := g.checkpoint(ctx, func(gs *ateapi.GoldenSnapshotStatus) { gs.ErrorMessage, gs.SnapshotTime = "", nil }); err != nil {
		return err
	}
	log.Printf("herd-api: template %s/%s: golden snapshot built again (failure %d was %s)", g.rec.Space, g.rec.Name, st.GoldenFailures, failed)
	return nil
}

// checkpoint writes a change of the template's golden snapshot status at the version it was read at.
func (g *goldenBuild) checkpoint(ctx context.Context, mutate func(*ateapi.GoldenSnapshotStatus)) error {
	tpl, err := g.template()
	if err != nil {
		return err
	}
	if tpl.Status == nil {
		tpl.Status = &ateapi.ActorTemplateStatus{}
	}
	if tpl.Status.GoldenSnapshotStatus == nil {
		tpl.Status.GoldenSnapshotStatus = &ateapi.GoldenSnapshotStatus{}
	}
	mutate(tpl.Status.GoldenSnapshotStatus)
	data, err := proto.Marshal(tpl)
	if err != nil {
		return err
	}
	r := g.rec
	r.Data = data
	next, err := g.s.cfg.Store.Update(ctx, r)
	if err != nil {
		return err
	}
	g.rec = next
	return nil
}

// truncateUTF8 cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// deleteGolden deletes the golden actor and the golden tag of the template with uid, whichever exist.
func (s *Server) deleteGolden(ctx context.Context, space, uid string) error {
	ar, err := s.loadActor(ctx, space, uid)
	switch {
	case err == nil && ar.st.Golden && ar.st.TemplateUID == uid:
		if _, err := s.deleteActor(ctx, space, uid, ar.rec.UID, 0); err != nil && status.Code(err) != codes.NotFound {
			return status.Errorf(codes.Unavailable, "golden actor not deleted yet: %v", err)
		}
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return toStatus(err)
	}
	tr, err := s.loadTag(ctx, space, uid)
	switch {
	case err == nil && tr.tag.GetStatus().GetActorTemplateUid() == uid:
		if _, err := s.deleteTag(ctx, space, uid, tr.rec.UID, 0); err != nil && status.Code(err) != codes.NotFound {
			return status.Errorf(codes.Unavailable, "golden tag not deleted yet: %v", err)
		}
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return toStatus(err)
	}
	return nil
}

// keepPullAuth holds a template's image_pull_auth for its golden build (PLT-330): in this process's memory only, so
// a golden build herd-api restarts in the middle of goes on without it.
func (s *Server) keepPullAuth(uid string, a link.Secret) {
	s.goldenMu.Lock()
	defer s.goldenMu.Unlock()
	s.pullAuths[uid] = a
}

// pullAuth is the image_pull_auth the golden build of the template with uid boots with ("" without one).
func (s *Server) pullAuth(uid string) link.Secret {
	s.goldenMu.Lock()
	defer s.goldenMu.Unlock()
	return s.pullAuths[uid]
}

// dropPullAuth forgets it: the golden build ended, or the template is gone.
func (s *Server) dropPullAuth(uid string) {
	s.goldenMu.Lock()
	defer s.goldenMu.Unlock()
	delete(s.pullAuths, uid)
}
