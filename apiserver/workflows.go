package apiserver

import (
	"context"
	"fmt"
	"log"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/store"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func presenceName(p string) string {
	if p == "" {
		return "unknown"
	}
	return p
}

// takeSnapshot asks the actor's herdlet for a snapshot at f under key and verifies the object in the snapshot store
// against the digest the herdlet answered. Only a verified object comes back as done; an answer without one is
// unknown, since a snapshot may or may not exist.
func (s *Server) takeSnapshot(ctx context.Context, ar *actorRec, key string, f vmm.Fidelity, cause link.Cause) (*link.SnapshotRef, link.Result, string) {
	o := s.step(ctx, ar, link.Work{Op: link.OpSnapshot, VM: vmID(ar), Fidelity: f, SnapshotKey: key, Cause: cause})
	if o.Result != link.ResultDone {
		return nil, o.Result, o.Reason
	}
	if o.Snapshot == nil || o.Snapshot.Key != key {
		return nil, link.ResultUnknown, fmt.Sprintf("herdlet answered done but named no snapshot under %q", key)
	}
	obj, err := s.cfg.Snapshots.Verify(ctx, key, o.Snapshot.Digest)
	if err != nil {
		return nil, link.ResultUnknown, fmt.Sprintf("%v: %v", errNotDurable, err)
	}
	r := *o.Snapshot
	r.Digest = obj.Digest
	return &r, link.ResultDone, ""
}

// SuspendActor snapshots the actor at the fidelity its worker declared (capped by the template's preference),
// records the snapshot's key+digest durably, and only then releases the worker. Synchronous unless the request asks
// for DURABILITY_MODE_ASYNCHRONOUS, in which case it answers SUSPENDING and runs the same steps in the background.
// An unknown or missing herdlet answer leaves the actor SUSPENDING with the reason (Unavailable), never SUSPENDED.
func (s *Server) SuspendActor(ctx context.Context, req *ateapi.SuspendActorRequest) (*ateapi.SuspendActorResponse, error) {
	space, name, err := ref(req.GetActor())
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, space, auth.ActionWrite); err != nil {
		return nil, err
	}
	unlock := s.lock(actorKey(space, name))
	ar, err := s.loadActor(ctx, space, name)
	if err != nil {
		unlock()
		return nil, toStatus(err)
	}
	switch ar.actor.Status.GetState() {
	case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
		unlock()
		return &ateapi.SuspendActorResponse{Actor: ar.view()}, nil
	case ateapi.ActorState_ACTOR_STATE_RUNNING, ateapi.ActorState_ACTOR_STATE_SUSPENDING:
	default:
		unlock()
		return nil, status.Errorf(codes.FailedPrecondition, "actor %s/%s is %v", space, name, ar.actor.Status.GetState())
	}
	if req.GetOptions().GetMode() == ateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS {
		logBegun(ar, "asynchronous suspend")
		if ar.st.Owed == nil || ar.st.Owed.Op != owedRelease {
			ar.setState(ateapi.ActorState_ACTOR_STATE_SUSPENDING)
			ar.owe(owedSuspend, "asynchronous suspend in progress", time.Now())
			if err := s.save(ctx, ar); err != nil {
				unlock()
				return nil, toStatus(err)
			}
		}
		view := ar.view()
		s.bg.Add(1)
		go func() {
			defer s.bg.Done()
			defer unlock()
			// No caller waits for this answer: the log is where a failed or owed suspend shows.
			err := s.suspendSteps(context.WithoutCancel(ctx), ar)
			logEnded(ar, "asynchronous suspend", err)
		}()
		return &ateapi.SuspendActorResponse{Actor: view}, nil
	}
	defer unlock()
	logBegun(ar, "suspend")
	err = s.suspendSteps(ctx, ar)
	logEnded(ar, "suspend", err)
	showOwed(ctx, ar)
	if err != nil {
		return nil, toStatus(err)
	}
	return &ateapi.SuspendActorResponse{Actor: ar.view()}, nil
}

func (s *Server) suspendSteps(ctx context.Context, ar *actorRec) error {
	if ar.st.Owed != nil && ar.st.Owed.Op == owedRelease {
		return s.release(ctx, ar)
	}
	ar.setState(ateapi.ActorState_ACTOR_STATE_SUSPENDING)
	caps, ok := s.workerCaps(ar.st.Worker)
	if !ok {
		ar.owe(owedSuspend, fmt.Sprintf("herdlet %q is not connected", ar.st.Worker), time.Now())
		if err := s.save(ctx, ar); err != nil {
			return err
		}
		return status.Errorf(codes.Unavailable, "suspend owed: %s", ar.st.Owed.Reason)
	}
	tpl, _, err := s.loadTemplate(ctx, ar.rec.Space, &ateapi.ObjectRef{Name: ar.st.TemplateName})
	if err != nil {
		return err
	}
	ar.owe(owedSuspend, "snapshot not taken yet", time.Now())
	if err := s.save(ctx, ar); err != nil {
		return err
	}
	gen := len(ar.actor.Status.Snapshots) + 1
	key := snapshotKey("actors", ar.rec.Space, ar.rec.Name, ar.rec.UID, gen)
	snap, res, reason := s.takeSnapshot(ctx, ar, key, fidelityFor(tpl, caps), link.CauseSuspend)
	switch res {
	case link.ResultFailed:
		// No snapshot was taken and the VM was left running.
		ar.st.Owed = nil
		ar.setState(ateapi.ActorState_ACTOR_STATE_RUNNING)
		if err := s.save(ctx, ar); err != nil {
			return err
		}
		return status.Errorf(codes.FailedPrecondition, "snapshot failed: %s", reason)
	case link.ResultUnknown:
		ar.owe(owedSuspend, "snapshot unknown: "+reason, time.Now())
		if err := s.save(ctx, ar); err != nil {
			return err
		}
		return status.Errorf(codes.Unavailable, "suspend owed: %s", ar.st.Owed.Reason)
	}
	// Durable before release: the verified key+digest is recorded before the worker is asked to let the VM go.
	ar.st.Snapshot = snap
	ar.st.Keys = append(ar.st.Keys, key)
	ar.actor.Status.Snapshots = append(ar.actor.Status.Snapshots, snapshotProto(snap, ateapi.SnapshotOwner_SNAPSHOT_OWNER_ACTOR, int32(gen), ar.st.TemplateUID))
	ar.owe(owedRelease, "snapshot durable; worker not released yet", time.Now())
	if err := s.save(ctx, ar); err != nil {
		return err
	}
	return s.release(ctx, ar)
}

// release deletes a suspended actor's VM on its worker; the actor is SUSPENDED and the slot free only once the
// herdlet answered the VM verified gone.
func (s *Server) release(ctx context.Context, ar *actorRec) error {
	o := s.step(ctx, ar, link.Work{Op: link.OpDelete, VM: vmID(ar), Cause: link.CauseSuspend})
	if o.Result == link.ResultDone && o.Presence == link.PresenceGone {
		ar.st.Worker, ar.st.Owed = "", nil
		ar.setState(ateapi.ActorState_ACTOR_STATE_SUSPENDED)
		ar.actor.Status.WorkerAssignment, ar.actor.Status.AssignedNode = nil, ""
		return s.save(ctx, ar)
	}
	ar.owe(owedRelease, fmt.Sprintf("snapshot durable; worker release not verified (VM %s): %s", presenceName(o.Presence), o.Reason), time.Now())
	if err := s.save(ctx, ar); err != nil {
		return err
	}
	return status.Errorf(codes.Unavailable, "suspend owed: %s", ar.st.Owed.Reason)
}

// ResumeActor restores a suspended actor from its latest durable snapshot on a worker that can restore it: a MEMORY
// snapshot only within its restore family, a VOLUMES one anywhere.
func (s *Server) ResumeActor(ctx context.Context, req *ateapi.ResumeActorRequest) (*ateapi.ResumeActorResponse, error) {
	space, name, err := ref(req.GetActor())
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, space, auth.ActionWrite); err != nil {
		return nil, err
	}
	unlock := s.lock(actorKey(space, name))
	defer unlock()
	ar, err := s.loadActor(ctx, space, name)
	if err != nil {
		return nil, toStatus(err)
	}
	switch ar.actor.Status.GetState() {
	case ateapi.ActorState_ACTOR_STATE_RUNNING:
		return &ateapi.ResumeActorResponse{Actor: ar.view(), Resumed: false}, nil
	case ateapi.ActorState_ACTOR_STATE_RESUMING:
		logBegun(ar, "resume")
		err := s.reconcileStart(ctx, ar)
		logEnded(ar, "resume", err)
		if err != nil {
			showOwed(ctx, ar)
			return nil, toStatus(err)
		}
		return &ateapi.ResumeActorResponse{Actor: ar.view(), Resumed: true}, nil
	case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "actor %s/%s is %v", space, name, ar.actor.Status.GetState())
	}
	if ar.st.Snapshot == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "actor %s/%s has no durable snapshot", space, name)
	}
	tpl, _, err := s.loadTemplate(ctx, space, &ateapi.ObjectRef{Name: ar.st.TemplateName})
	if err != nil {
		return nil, toStatus(err)
	}
	logBegun(ar, "resume")
	ar.st.Restore = ar.st.Snapshot
	ar.setState(ateapi.ActorState_ACTOR_STATE_RESUMING)
	ar.owe(owedResume, "placed, not restored yet", time.Now())
	if err := s.place(ctx, ar, tpl, false); err != nil {
		log.Printf("herd-api: actor %s/%s: resume not placed: %v", space, name, err)
		return nil, toStatus(err)
	}
	err = s.start(ctx, ar, owedResume, ateapi.ActorState_ACTOR_STATE_SUSPENDED)
	logEnded(ar, "resume", err)
	if err != nil {
		showOwed(ctx, ar)
		return nil, toStatus(err)
	}
	return &ateapi.ResumeActorResponse{Actor: ar.view(), Resumed: true}, nil
}

// reconcileStart settles an owed boot or resume: it asks the herdlet whether the VM is there, and starts it again only
// when the herdlet says it is gone (a herdlet treats a start of a VM it already holds as done).
func (s *Server) reconcileStart(ctx context.Context, ar *actorRec) error {
	failed := ateapi.ActorState_ACTOR_STATE_SUSPENDED
	op := owedResume
	if ar.st.Owed != nil && ar.st.Owed.Op == owedBoot {
		failed, op = ateapi.ActorState_ACTOR_STATE_UNSPECIFIED, owedBoot
	}
	o := s.step(ctx, ar, link.Work{Op: link.OpInspect, VM: vmID(ar)})
	if o.Result == link.ResultUnknown || (o.Presence != link.PresenceGone && o.Presence != link.PresencePresent) {
		ar.owe(op, "start unknown; inspect unknown: "+o.Reason, time.Now())
		if err := s.save(ctx, ar); err != nil {
			return err
		}
		return status.Errorf(codes.Unavailable, "%s owed: %s", op, ar.st.Owed.Reason)
	}
	return s.start(ctx, ar, op, failed)
}

// DeleteActor deletes the actor. The record goes only once the herdlet answered its VM verified gone and its snapshot
// objects are verified deleted; until then the actor stays DELETING with the delete owed (Unavailable, the reason in
// the Herd-Owed header) and Reconcile retries it.
func (s *Server) DeleteActor(ctx context.Context, req *ateapi.DeleteActorRequest) (*ateapi.Actor, error) {
	space, name, err := ref(req.GetActor())
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, space, auth.ActionWrite); err != nil {
		return nil, err
	}
	return s.deleteActor(ctx, space, name, req.GetOptions().GetUid(), req.GetOptions().GetVersion())
}

// deleteActor is DeleteActor once the caller is authorized.
func (s *Server) deleteActor(ctx context.Context, space, name, uid string, version int64) (*ateapi.Actor, error) {
	unlock := s.lock(actorKey(space, name))
	defer unlock()
	ar, err := s.loadActor(ctx, space, name)
	if err != nil {
		return nil, toStatus(err)
	}
	if err := precondition(ar.rec, uid, version, false); err != nil {
		return nil, err
	}
	logBegun(ar, "delete")
	if ar.st.DeleteTime == nil {
		now := time.Now().UTC()
		ar.st.DeleteTime = &now
	}
	ar.setState(ateapi.ActorState_ACTOR_STATE_DELETING)
	ar.owe(owedDelete, "VM not verified gone yet", time.Now())
	if err := s.save(ctx, ar); err != nil {
		return nil, toStatus(err)
	}
	if err := s.deleteSteps(ctx, ar); err != nil {
		logEnded(ar, "delete", err)
		showOwed(ctx, ar)
		return nil, toStatus(err)
	}
	log.Printf("herd-api: actor %s/%s: delete ends: record deleted", space, name)
	return ar.view(), nil
}

func (s *Server) deleteSteps(ctx context.Context, ar *actorRec) error {
	if ar.st.Worker != "" {
		o := s.step(ctx, ar, link.Work{Op: link.OpDelete, VM: vmID(ar), Cause: link.CauseDelete})
		if o.Result != link.ResultDone || o.Presence != link.PresenceGone {
			ar.owe(owedDelete, fmt.Sprintf("VM not verified gone (%s): %s", presenceName(o.Presence), o.Reason), time.Now())
			if err := s.save(ctx, ar); err != nil {
				return err
			}
			return status.Errorf(codes.Unavailable, "delete owed: %s", ar.st.Owed.Reason)
		}
		ar.st.Worker = ""
		ar.actor.Status.WorkerAssignment, ar.actor.Status.AssignedNode = nil, ""
		if err := s.save(ctx, ar); err != nil {
			return err
		}
	}
	for len(ar.st.Keys) > 0 {
		if err := s.cfg.Snapshots.Delete(ctx, ar.st.Keys[0]); err != nil {
			ar.owe(owedDelete, fmt.Sprintf("snapshot %s not verified deleted: %v", ar.st.Keys[0], err), time.Now())
			if err := s.save(ctx, ar); err != nil {
				return err
			}
			return status.Errorf(codes.Unavailable, "delete owed: %s", ar.st.Owed.Reason)
		}
		ar.st.Keys = ar.st.Keys[1:]
	}
	if err := s.deleteEgressOf(ctx, ar.rec.Space, ar.rec.Name); err != nil {
		return err
	}
	if err := s.cfg.Store.Delete(ctx, ar.rec.Key, ar.rec.UID, ar.rec.Version); err != nil {
		return err
	}
	ar.st.Owed = nil
	return nil
}

// Reconcile retries every owed workflow step once, then takes every template's unfinished golden snapshot build one
// pass further, and returns how many steps and builds are still owed. Each herdlet call it makes waits up to
// ReconcileCallTimeout.
func (s *Server) Reconcile(ctx context.Context) (int, error) {
	ctx = context.WithValue(ctx, reconciling{}, true)
	recs, err := s.cfg.Store.List(ctx, store.KindActor, "")
	if err != nil {
		return 0, err
	}
	left := 0
	for _, r := range recs {
		ar, err := parseActor(r)
		if err != nil {
			return left, err
		}
		if ar.st.Owed == nil {
			continue
		}
		if s.reconcileOne(ctx, r.Key) {
			left++
		}
	}
	if ctx.Err() != nil {
		// The pass ran out of time on the actors; golden builds wait for the next one.
		return left, nil
	}
	tpls, err := s.cfg.Store.List(ctx, store.KindActorTemplate, "")
	if err != nil {
		return left, err
	}
	// Golden rebuilds for the guests the workers run now (DEFAUL-1606): at most GoldenRebuilds in progress at once,
	// counted over every template, and only this loop begins one.
	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()
	pool, rebuilds := s.guestPool(), 0
	for _, r := range tpls {
		if rebuilding(r) {
			rebuilds++
		}
	}
	for _, r := range tpls {
		t, err := decode(r, &ateapi.ActorTemplate{})
		if err != nil {
			return left, err
		}
		gs := t.GetStatus().GetGoldenSnapshotStatus()
		if goldenEnabled(t) && gs.GetGoldenTag() != nil && rebuilds < s.rebuildLimit() {
			st, err := parseTemplateState(r)
			if err != nil {
				return left, err
			}
			if goldenRebuildDue(st, pool) {
				started, err := s.startRebuild(ctx, r.Key, pool)
				if err != nil {
					log.Printf("herd-api: template %s/%s: golden rebuild not begun yet: %v", r.Space, r.Name, err)
				}
				if started {
					rebuilds++
					if r, err = s.cfg.Store.Get(ctx, r.Key); err != nil {
						return left, err
					}
				}
			}
		}
		renewing := gs.GetGoldenTag() != nil && (rebuilding(r) || retiring(r))
		if !goldenEnabled(t) || goldenDone(gs) && !goldenRetryDue(r, gs, time.Now()) && !renewing {
			continue
		}
		again, pending, err := s.goldenPass(ctx, r.Key)
		if err != nil {
			log.Printf("herd-api: template %s/%s: golden snapshot not built yet: %v", r.Space, r.Name, err)
		}
		if again > 0 {
			s.kickGolden(r.Key, again)
		}
		if pending {
			left++
		}
	}
	return left, nil
}

func (s *Server) reconcileOne(ctx context.Context, k store.Key) (stillOwed bool) {
	unlock := s.lock(k)
	defer unlock()
	ar, err := s.loadActor(ctx, k.Space, k.Name)
	if err != nil || ar.st.Owed == nil {
		return false
	}
	what := "reconcile of owed " + ar.st.Owed.Op
	logBegun(ar, what)
	switch ar.st.Owed.Op {
	case owedDelete:
		err = s.deleteSteps(ctx, ar)
	case owedRelease, owedSuspend:
		err = s.suspendSteps(ctx, ar)
	case owedBoot, owedResume:
		err = s.reconcileStart(ctx, ar)
	}
	if ar.st.Owed == nil && ar.actor.Status.GetState() == ateapi.ActorState_ACTOR_STATE_DELETING && err == nil {
		log.Printf("herd-api: actor %s/%s: %s ends: record deleted", k.Space, k.Name, what)
	} else {
		logEnded(ar, what, err)
	}
	return err != nil && ar.st.Owed != nil
}
