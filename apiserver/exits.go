package apiserver

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// vmExited is the hub's sink for a VM exit a herdlet observed. The actor is moved in the background: its workflow
// lock may be held by a call that waits for this same herdlet's next answer, which the link reads only after this
// returns.
func (s *Server) vmExited(ctx context.Context, worker string, e link.Exited) {
	log.Printf("herd-api: herdlet %s reports VM %s exited (%s): %s", worker, e.VM, e.Reason, e.Detail)
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		if err := s.crash(context.WithoutCancel(ctx), worker, e); err != nil {
			log.Printf("herd-api: VM %s on %s exited, but its actor was not moved: %v", e.VM, worker, err)
		}
	}()
}

// crash moves the RUNNING actor whose VM exited on worker to CRASHED, with the reason. An actor in a workflow
// (SUSPENDING, RESUMING, DELETING) is left to it: what the herdlet answers that workflow decides. The actor keeps its
// worker until a delete verifies the VM's remains gone, so the slot stays taken until then.
func (s *Server) crash(ctx context.Context, worker string, e link.Exited) error {
	recs, err := s.cfg.Store.List(ctx, store.KindActor, "")
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.UID != e.VM {
			continue
		}
		ar, err := parseActor(r)
		if err != nil {
			return err
		}
		if ar.st.Worker != worker {
			continue
		}
		return s.crashActor(ctx, r.Key, worker, e)
	}
	log.Printf("herd-api: VM %s on %s exited; no actor holds it there", e.VM, worker)
	return nil
}

func (s *Server) crashActor(ctx context.Context, k store.Key, worker string, e link.Exited) error {
	unlock := s.lock(k)
	defer unlock()
	ar, err := s.loadActor(ctx, k.Space, k.Name)
	if err != nil {
		return err
	}
	if vmID(ar) != e.VM || ar.st.Worker != worker {
		return nil
	}
	if st := ar.actor.Status.GetState(); st != ateapi.ActorState_ACTOR_STATE_RUNNING || ar.st.Owed != nil {
		log.Printf("herd-api: actor %s/%s (VM %s) is %v; its workflow settles it", k.Space, k.Name, e.VM, st)
		return nil
	}
	now := time.Now()
	msg := fmt.Sprintf("%s: VM %s on worker %s exited while the actor was running", e.Reason, e.VM, worker)
	if e.Reason == link.ExitContainerExited {
		msg = fmt.Sprintf("%s: a container of VM %s on worker %s ended while the actor was running", e.Reason, e.VM, worker)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if len(msg) > 4096 {
		msg = strings.ToValidUTF8(msg[:4096], "")
	}
	ar.setState(ateapi.ActorState_ACTOR_STATE_CRASHED)
	ar.actor.Status.Crash = &ateapi.ActorCrash{Message: msg, CrashTime: timestamppb.New(now)}
	ar.actor.Status.WorkerAssignment, ar.actor.Status.AssignedNode = nil, ""
	if err := s.save(ctx, ar); err != nil {
		return err
	}
	log.Printf("herd-api: actor %s/%s RUNNING -> CRASHED: %s", k.Space, k.Name, msg)
	return nil
}

// reasonHerdletRestarted is the crash reason of an actor whose VM ended with its herdlet's process (PLT-371).
const reasonHerdletRestarted = "herdlet-restarted"

// herdletLinked is the hub's sink for a herdlet's link coming up. A herdlet process registering under a worker name
// whose actors' VMs another process holds is a restarted herdlet, and a restarted herdlet does not adopt the VMs it
// left (ADR A-19): those VMs are gone. Bound: the actors are settled as the new link comes up, in the background, each
// as soon as its workflow lock is free (a call still waiting on the old link ended when this link replaced it; a call
// on the new one waits at most its call timeout); Reconcile's next pass settles any this pass missed.
func (s *Server) herdletLinked(ctx context.Context, r link.Register) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		if err := s.settleRestart(context.WithoutCancel(ctx), r.Worker); err != nil {
			log.Printf("herd-api: herdlet %s linked, but its actors were not settled: %v (Reconcile tries again)", r.Worker, err)
		}
	}()
}

// settleRestart crashes each actor on worker whose VM a herdlet process other than the connected one holds
// (crashRestarted).
func (s *Server) settleRestart(ctx context.Context, worker string) error {
	recs, err := s.cfg.Store.List(ctx, store.KindActor, "")
	if err != nil {
		return err
	}
	for _, r := range recs {
		ar, err := parseActor(r)
		if err != nil {
			return err
		}
		if ar.st.Worker != worker || !s.leftByRestart(ar) {
			continue
		}
		if err := s.crashRestarted(ctx, r.Key); err != nil {
			return err
		}
	}
	return nil
}

// leftByRestart is whether the actor's VM was started by a herdlet process other than the one connected for its
// worker now. A worker that is not connected, or an actor whose process is not known, is not concluded on.
func (s *Server) leftByRestart(ar *actorRec) bool {
	if ar.st.Worker == "" || ar.st.Instance == "" {
		return false
	}
	cur, ok := s.cfg.Hub.Instance(ar.st.Worker)
	return ok && cur != ar.st.Instance
}

// crashRestarted moves the actor to CRASHED if its VM ended with an earlier herdlet process of its worker and it is
// RUNNING, or SUSPENDING with the snapshot owed (none is durable, and the new process cannot take one of a VM it never
// held). Any other workflow is left to itself: an owed release or delete finds the VM gone, and an owed boot or resume
// asks the new process and starts the VM there.
func (s *Server) crashRestarted(ctx context.Context, k store.Key) error {
	unlock := s.lock(k)
	defer unlock()
	ar, err := s.loadActor(ctx, k.Space, k.Name)
	if err != nil {
		return err
	}
	if !s.leftByRestart(ar) {
		return nil
	}
	st := ar.actor.Status.GetState()
	switch {
	case st == ateapi.ActorState_ACTOR_STATE_RUNNING && ar.st.Owed == nil:
	case st == ateapi.ActorState_ACTOR_STATE_SUSPENDING && ar.st.Owed != nil && ar.st.Owed.Op == owedSuspend:
	default:
		return nil
	}
	cur, _ := s.cfg.Hub.Instance(ar.st.Worker)
	msg := fmt.Sprintf("%s: the herdlet of worker %s restarted while the actor was %s: VM %s was held by herdlet process %q, "+
		"and the new process %q does not adopt the VMs the old one left", reasonHerdletRestarted, ar.st.Worker,
		strings.TrimPrefix(st.String(), "ACTOR_STATE_"), vmID(ar), ar.st.Instance, cur)
	ar.setState(ateapi.ActorState_ACTOR_STATE_CRASHED)
	ar.st.Owed = nil
	ar.actor.Status.Crash = &ateapi.ActorCrash{Message: msg, CrashTime: timestamppb.Now()}
	ar.actor.Status.WorkerAssignment, ar.actor.Status.AssignedNode = nil, ""
	if err := s.save(ctx, ar); err != nil {
		return err
	}
	log.Printf("herd-api: actor %s/%s %v -> CRASHED: %s", k.Space, k.Name, st, msg)
	return nil
}
