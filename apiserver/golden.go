package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
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
//
// A golden tag records the guest of the worker that took it (link.Capabilities.Guest). Once workers are connected and
// none of them runs that guest any more (a herd upgrade replaced it), Reconcile builds the golden again in the
// background (DEFAUL-1606): rebuild n's golden actor and tag are named "<uid>-<n>", status.golden_tag keeps naming
// the old tag until the new one exists and then names the new one, and the old tag is deleted once nothing can still
// be restoring from it. A failed rebuild keeps the old golden and retries as a failed build does; at most
// Config.GoldenRebuilds rebuilds run at once; and a template is rebuilt once for each set of guests the pool runs, so a
// pool that runs several guests during a rolling upgrade keeps the old golden while any worker runs its guest and
// never rebuilds in a loop.

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

// Config.GoldenRetries and Config.GoldenRetryBackoff defaults: retries 1, 2, 4, 8 and 16 minutes after a failure;
// Config.GoldenRebuilds's: two templates' goldens rebuilt at a time after an upgrade.
const (
	DefaultGoldenRetries      = 5
	DefaultGoldenRetryBackoff = time.Minute
	DefaultGoldenRebuilds     = 2
)

// templateState is herd-api's bookkeeping for a template (store.Record.State): its failed golden builds.
type templateState struct {
	// GoldenFailures counts the template's golden builds that failed.
	GoldenFailures int `json:"goldenFailures,omitempty"`
	// GoldenRetryAt is when Reconcile builds the failed golden again; nil: not again.
	GoldenRetryAt *time.Time `json:"goldenRetryAt,omitempty"`
	// GoldenGuest is the guest that built the golden status.golden_tag names; "" for a golden built before herd knew
	// guests (DEFAUL-1606) or by a herdlet that declares none.
	GoldenGuest string `json:"goldenGuest,omitempty"`
	// GoldenGeneration counts the template's golden rebuilds: rebuild n's golden actor and tag are named "<uid>-<n>".
	GoldenGeneration int `json:"goldenGeneration,omitempty"`
	// RebuiltFor is the set of guests the connected workers ran when the last rebuild began, sorted: another rebuild
	// begins only for another set.
	RebuiltFor []string `json:"rebuiltFor,omitempty"`
	// Rebuild is the rebuild in progress, while the old golden serves; nil: none.
	Rebuild *goldenRebuild `json:"rebuild,omitempty"`
	// RebuildError is why the last rebuild ended without a golden, once its retries failed.
	RebuildError string `json:"rebuildError,omitempty"`
	// Retired are golden tags a rebuild replaced, deleted once nothing can still be restoring from them.
	Retired []retiredGolden `json:"retired,omitempty"`
	// Attempt is the golden snapshot herd-api asked for and has not seen end, recorded before the ask (PLT-382): a
	// pass that finds it with no golden tag knows the attempt failed, though herd-api restarted before its answer.
	Attempt *goldenAttempt `json:"attempt,omitempty"`
}

// goldenAttempt is one golden snapshot asked of a herdlet: the golden actor and tag it is for, and the snapshot
// object it is stored under.
type goldenAttempt struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// goldenRebuild is a rebuild of a template's golden for the guests its workers run now (DEFAUL-1606).
type goldenRebuild struct {
	// Name names its golden actor and tag.
	Name string `json:"name"`
	// WarmupEnd is when its golden actor's snapshot is taken (status.snapshot_time of a first build).
	WarmupEnd *time.Time `json:"warmupEnd,omitempty"`
	// Failures counts its builds that failed; RetryAt is when it is built again (nil: now).
	Failures int        `json:"failures,omitempty"`
	RetryAt  *time.Time `json:"retryAt,omitempty"`
}

// retiredGolden is a golden tag a rebuild replaced at At.
type retiredGolden struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
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

// guestPool is the guests the connected workers run, sorted.
func (s *Server) guestPool() []string {
	var out []string
	for _, w := range s.cfg.Hub.Workers() {
		if g := w.Capabilities.Guest; !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	slices.Sort(out)
	return out
}

// goldenRebuildDue: the template's golden was built by a guest that no connected worker runs (and some worker is
// connected), no rebuild is in progress, and none began for this set of guests.
func goldenRebuildDue(st templateState, pool []string) bool {
	return len(pool) > 0 && !slices.Contains(pool, st.GoldenGuest) && st.Rebuild == nil && !slices.Equal(st.RebuiltFor, pool)
}

// goldenNames are the names of every golden actor and tag the template may have: its first, the one its status names,
// a rebuild's and the replaced ones not yet deleted.
func goldenNames(r store.Record, t *ateapi.ActorTemplate) []string {
	names := []string{r.UID}
	add := func(n string) {
		if n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	add(t.GetStatus().GetGoldenSnapshotStatus().GetGoldenTag().GetName())
	if st, err := parseTemplateState(r); err == nil {
		if st.Rebuild != nil {
			add(st.Rebuild.Name)
		}
		for _, o := range st.Retired {
			add(o.Name)
		}
	}
	return names
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
	s, uid := g.s, g.rec.UID
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
				// The build ended for good; a retry still to come boots with the template's image_pull_auth and plain hosts.
				s.dropPull(uid)
			}
			if goldenEnabled(tpl) && gs.GetGoldenTag() != nil {
				return g.renew(ctx)
			}
			return 0, false, nil
		}
		again, pending, more, err := g.step(ctx, g.firstTarget())
		if !more || err != nil {
			return again, pending, err
		}
	}
}

// goldenTarget is where a build's steps keep their progress: the template's status for its first golden (and the
// retries of that), its state for a rebuild while the old golden serves.
type goldenTarget struct {
	// name names the golden actor and the golden tag.
	name string
	// warmupEnd is when the running golden actor's snapshot is taken (nil: not set yet).
	warmupEnd    func() *time.Time
	setWarmupEnd func(context.Context, time.Time) error
	// tagged is called once the golden tag exists, with the guest that took it.
	tagged func(ctx context.Context, ref *ateapi.ObjectRef, guest string) error
	// fail ends the build in a failure.
	fail func(ctx context.Context, reason, msg string) error
}

func (g *goldenBuild) firstTarget() goldenTarget {
	return goldenTarget{
		name: g.rec.UID,
		warmupEnd: func() *time.Time {
			tpl, err := g.template()
			if err != nil || tpl.GetStatus().GetGoldenSnapshotStatus().GetSnapshotTime() == nil {
				return nil
			}
			t := tpl.GetStatus().GetGoldenSnapshotStatus().GetSnapshotTime().AsTime()
			return &t
		},
		setWarmupEnd: func(ctx context.Context, t time.Time) error {
			return g.checkpoint(ctx, func(gs *ateapi.GoldenSnapshotStatus) { gs.SnapshotTime = timestamppb.New(t) })
		},
		tagged: g.saveTag,
		fail:   g.fail,
	}
}

// step advances the build of the golden actor and tag named t.name by one step. more asks for the next step now;
// again > 0 asks to look once more after that long (the warmup); pending says the build has not ended. An error is a
// step that did not converge and is retried (by Reconcile); a build that cannot succeed is not an error but t.fail.
func (g *goldenBuild) step(ctx context.Context, t goldenTarget) (again time.Duration, pending, more bool, err error) {
	s, space, uid := g.s, g.rec.Space, g.rec.UID
	ref := &ateapi.ObjectRef{Atespace: space, Name: t.name}
	// A complete tag survives a failure between the tag and the status write.
	tag, err := s.loadTag(ctx, space, t.name)
	switch {
	case err == nil:
		if err := g.endAttempt(t.name); err != nil {
			return 0, true, false, err
		}
		if tag.tag.GetStatus().GetActorTemplateUid() != uid || tag.tag.GetSourceActor().GetName() != t.name {
			return 0, false, false, t.fail(ctx, reasonGoldenTagConflict, "golden tag belongs to another actor or template")
		}
		err := t.tagged(ctx, ref, tag.st.Guest)
		return 0, err != nil, false, err
	case !errors.Is(err, store.ErrNotFound):
		return 0, true, false, err
	}
	if st, err := parseTemplateState(g.rec); err != nil {
		return 0, true, false, err
	} else if a := st.Attempt; a != nil && a.Name == t.name {
		// Asked before and never ended here: herd-api stopped while the herdlet's answer was in flight.
		return 0, false, false, g.attemptFailed(ctx, t, "the golden snapshot herd-api asked for before it restarted got no answer")
	}
	ar, err := s.loadActor(ctx, space, t.name)
	if errors.Is(err, store.ErrNotFound) {
		_, err := s.createActor(ctx, &ateapi.Actor{
			Metadata:      &ateapi.ResourceMetadata{Atespace: space, Name: t.name},
			ActorTemplate: &ateapi.ObjectRef{Atespace: space, Name: g.rec.Name},
		}, true)
		switch status.Code(err) {
		case codes.OK, codes.AlreadyExists:
			return 0, true, true, nil
		case codes.InvalidArgument, codes.FailedPrecondition:
			// The template cannot boot (its herdlet refused it for good); retrying cannot help.
			return 0, false, false, t.fail(ctx, reasonGoldenActorInvalid, status.Convert(err).Message())
		}
		return 0, true, false, fmt.Errorf("creating golden actor: %w", err)
	}
	if err != nil {
		return 0, true, false, err
	}
	switch st := ar.actor.GetStatus().GetState(); st {
	case ateapi.ActorState_ACTOR_STATE_CRASHED:
		return 0, false, false, t.fail(ctx, reasonGoldenActorCrashed, "golden actor crashed before its snapshot was taken")
	case ateapi.ActorState_ACTOR_STATE_RUNNING:
		end := t.warmupEnd()
		if end == nil {
			if err := t.setWarmupEnd(ctx, time.Now().Add(s.cfg.GoldenWarmup)); err != nil {
				return 0, true, false, err
			}
			return 0, true, true, nil
		}
		if rem := time.Until(*end); rem > 0 {
			return rem, true, false, nil
		}
		fallthrough
	case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
		// A running golden actor is snapshotted in place at the highest fidelity its worker keeps (capped by the
		// template's preference); a suspended one has its snapshot referenced (A-45). The attempt is recorded first,
		// so that every attempt counts against the retries and leaves no object once it failed, whatever ends it.
		key, err := g.beginAttempt(ctx, t.name)
		if err != nil {
			return 0, true, false, err
		}
		_, err = s.createTagAt(ctx, &ateapi.Tag{Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: t.name}, SourceActor: ref},
			ateapi.TagScope_TAG_SCOPE_PUBLISHED, key)
		switch status.Code(err) {
		case codes.OK, codes.AlreadyExists:
			return 0, true, true, nil
		}
		return 0, false, false, g.attemptFailed(ctx, t, status.Convert(err).Message())
	case ateapi.ActorState_ACTOR_STATE_RESUMING, ateapi.ActorState_ACTOR_STATE_SUSPENDING, ateapi.ActorState_ACTOR_STATE_DELETING:
		// An owed boot or suspend, or the delete of a failed build's golden actor before a retry: Reconcile settles
		// the actor, and the next pass goes on from there.
		return 0, true, false, fmt.Errorf("golden actor is %v: %s", st, owedReason(ar))
	default:
		return 0, false, false, t.fail(ctx, reasonUnexpectedState, fmt.Sprintf("golden actor in unexpected state %v", st))
	}
}

func owedReason(ar *actorRec) string {
	if o := ar.st.Owed; o != nil {
		return o.Op + " owed: " + o.Reason
	}
	return "no step owed"
}

// beginAttempt records a golden snapshot attempt for the golden named name and returns its snapshot object's key.
func (g *goldenBuild) beginAttempt(ctx context.Context, name string) (string, error) {
	st, err := parseTemplateState(g.rec)
	if err != nil {
		return "", err
	}
	nonce, err := store.NewUID()
	if err != nil {
		return "", err
	}
	st.Attempt = &goldenAttempt{Name: name, Key: snapshotKey("tags", g.rec.Space, name, nonce, 1)}
	if err := g.saveState(ctx, st); err != nil {
		return "", err
	}
	return st.Attempt.Key, nil
}

// endAttempt forgets the attempt for the golden named name in g.rec, for the next write of the template to record.
func (g *goldenBuild) endAttempt(name string) error {
	st, err := parseTemplateState(g.rec)
	if err != nil || st.Attempt == nil || st.Attempt.Name != name {
		return err
	}
	st.Attempt = nil
	g.rec.State, err = json.Marshal(st)
	return err
}

// attemptFailed ends the recorded attempt with no golden tag: its snapshot object, if the herdlet wrote one, is
// deleted (an error leaves the attempt recorded, and the next pass deletes it again), and the attempt is a failed
// build of t, which retries it while it has retries left.
func (g *goldenBuild) attemptFailed(ctx context.Context, t goldenTarget, msg string) error {
	st, err := parseTemplateState(g.rec)
	if err != nil {
		return err
	}
	if a := st.Attempt; a != nil && a.Name == t.name {
		if err := g.s.cfg.Snapshots.Delete(ctx, a.Key); err != nil {
			return fmt.Errorf("deleting the snapshot of a failed golden attempt: %w", err)
		}
		if err := g.endAttempt(t.name); err != nil {
			return err
		}
	}
	return t.fail(ctx, reasonGoldenSnapshotFailed, msg)
}

// saveTag deletes the golden actor and only then records the tag, so a retry finds the tag again if either fails.
func (g *goldenBuild) saveTag(ctx context.Context, ref *ateapi.ObjectRef, guest string) error {
	if _, err := g.s.deleteActor(ctx, ref.GetAtespace(), ref.GetName(), "", 0); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("deleting golden actor: %w", err)
	}
	st, err := parseTemplateState(g.rec)
	if err != nil {
		return err
	}
	st.GoldenGuest = guest
	if g.rec.State, err = json.Marshal(st); err != nil {
		return err
	}
	if err := g.checkpoint(ctx, func(gs *ateapi.GoldenSnapshotStatus) { gs.GoldenTag = ref }); err != nil {
		return err
	}
	g.s.dropPull(g.rec.UID)
	log.Printf("herd-api: template %s/%s: golden snapshot built: tag %s, guest %q", g.rec.Space, g.rec.Name, ref.GetName(), guest)
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
		g.s.dropPull(g.rec.UID)
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

// checkpoint writes a change of the template's golden snapshot status (none for nil), and its state as g.rec holds
// it, at the version it was read at.
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
	if mutate != nil {
		mutate(tpl.Status.GoldenSnapshotStatus)
	}
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

// deleteGolden deletes the golden actors and golden tags of the template with uid under names (goldenNames),
// whichever exist.
func (s *Server) deleteGolden(ctx context.Context, space, uid string, names []string) error {
	for _, name := range names {
		ar, err := s.loadActor(ctx, space, name)
		switch {
		case err == nil && ar.st.Golden && ar.st.TemplateUID == uid:
			if _, err := s.deleteActor(ctx, space, name, ar.rec.UID, 0); err != nil && status.Code(err) != codes.NotFound {
				return status.Errorf(codes.Unavailable, "golden actor not deleted yet: %v", err)
			}
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return toStatus(err)
		}
		tr, err := s.loadTag(ctx, space, name)
		switch {
		case err == nil && tr.tag.GetStatus().GetActorTemplateUid() == uid:
			if _, err := s.deleteTag(ctx, space, name, tr.rec.UID, 0); err != nil && status.Code(err) != codes.NotFound {
				return status.Errorf(codes.Unavailable, "golden tag not deleted yet: %v", err)
			}
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return toStatus(err)
		}
	}
	return nil
}

// goldenPull is what a template's create gave for its golden build's pull only: its image_pull_auth (PLT-330) and
// its image_plain_http_hosts (DEFAUL-1600).
type goldenPull struct {
	auth  link.Secret
	plain []string
}

// keepPull holds a template's goldenPull for its golden build: in this process's memory only, so a golden build
// herd-api restarts in the middle of goes on without it.
func (s *Server) keepPull(uid string, p goldenPull) {
	s.goldenMu.Lock()
	defer s.goldenMu.Unlock()
	s.pulls[uid] = p
}

// pull is the goldenPull the golden build of the template with uid boots with (zero without one).
func (s *Server) pull(uid string) goldenPull {
	s.goldenMu.Lock()
	defer s.goldenMu.Unlock()
	return s.pulls[uid]
}

// dropPull forgets it: the golden build ended, or the template is gone.
func (s *Server) dropPull(uid string) {
	s.goldenMu.Lock()
	defer s.goldenMu.Unlock()
	delete(s.pulls, uid)
}
