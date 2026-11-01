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
