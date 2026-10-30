package apiserver

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/everdict/herd/link"
)

// step makes one herdlet call of an actor's workflow and logs it before and after, with the actor and the outcome,
// so a workflow that stalls shows which call it waits on (PLT-200).
func (s *Server) step(ctx context.Context, ar *actorRec, w link.Work) link.Outcome {
	what := string(w.Op)
	if w.Cause != "" {
		what += " (" + string(w.Cause) + ")"
	}
	log.Printf("herd-api: actor %s/%s: %s of VM %s on herdlet %s", ar.rec.Space, ar.rec.Name, what, w.VM, ar.st.Worker)
	start := time.Now()
	o := s.call(ctx, ar.st.Worker, w)
	log.Printf("herd-api: actor %s/%s: %s of VM %s on herdlet %s: %s after %v", ar.rec.Space, ar.rec.Name, what, w.VM,
		ar.st.Worker, outcomeText(o), time.Since(start).Round(time.Millisecond))
	return o
}

// outcomeText is a herdlet's answer as a log reads it: the result, the VM's presence if named, and the reason.
func outcomeText(o link.Outcome) string {
	t := string(o.Result)
	if o.Result == link.ResultUnknown {
		t = "unknown"
	}
	if o.Presence != "" {
		t += ", VM " + o.Presence
	}
	if o.Reason != "" {
		t += ": " + o.Reason
	}
	return t
}

// logBegun logs that a workflow of the actor begins, in the state the actor is in.
func logBegun(ar *actorRec, workflow string) {
	log.Printf("herd-api: actor %s/%s: %s begins (%v)", ar.rec.Space, ar.rec.Name, workflow, ar.actor.GetStatus().GetState())
}

// logEnded logs where a workflow of the actor left it: its state, what is still owed, and the error the caller got
// (an asynchronous suspend has no caller to get it).
func logEnded(ar *actorRec, workflow string, err error) {
	msg := fmt.Sprintf("herd-api: actor %s/%s: %s ends %v", ar.rec.Space, ar.rec.Name, workflow, ar.actor.GetStatus().GetState())
	if o := ar.st.Owed; o != nil {
		msg += fmt.Sprintf(", %s owed: %s", o.Op, o.Reason)
	}
	if err != nil {
		msg += fmt.Sprintf(" (%v)", err)
	}
	log.Print(msg)
}
