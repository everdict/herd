package herdlet

import (
	"context"
	"log"
	"time"

	"github.com/everdict/herd/link"
)

// logged carries out w and logs it before and after, with the VM (and the actor, when herd-api named it) and the
// outcome, so work that stalls on this host shows which step it is in (PLT-200).
func (a *Agent) logged(ctx context.Context, w link.Work) link.Outcome {
	what := string(w.Op)
	if w.Cause != "" {
		what += " (" + string(w.Cause) + ")"
	}
	what += " of VM " + w.VM
	if w.Actor != nil {
		what += " (actor " + w.Actor.Space + "/" + w.Actor.Name + ")"
	}
	log.Printf("herdlet %s: %s begins", a.Worker, what)
	start := time.Now()
	o := a.do(ctx, w)
	r := string(o.Result)
	if o.Result == link.ResultUnknown {
		r = "unknown"
	}
	if o.Presence != "" {
		r += ", VM " + o.Presence
	}
	if o.Reason != "" {
		r += ": " + o.Reason
	}
	log.Printf("herdlet %s: %s: %s after %v", a.Worker, what, r, time.Since(start).Round(time.Millisecond))
	return o
}
