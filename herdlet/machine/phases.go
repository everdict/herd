package machine

import (
	"context"
	"time"
)

// Phases is what one snapshot's steps on this host took (PLT-591); herdlet logs them in one line with the hash and the
// upload of the bundle.
type Phases struct {
	// Flush is the guest's flush.
	Flush time.Duration
	// VMM is the VMM's snapshot, from the paused VM to its files (and, for a suspend, the VM stopped).
	VMM time.Duration
	// Bundle is the bundle's write, the VMM's files and the volumes read into it.
	Bundle time.Duration
	// Memory is how many bytes the VMM's snapshot files the bundle carries hold.
	Memory int64
}

type phasesKey struct{}

// WithPhases returns a context on which a Snapshot records its Phases in the one returned.
func WithPhases(ctx context.Context) (context.Context, *Phases) {
	p := &Phases{}
	return context.WithValue(ctx, phasesKey{}, p), p
}

// phasesOf is ctx's Phases, or one nobody reads.
func phasesOf(ctx context.Context) *Phases {
	if p, ok := ctx.Value(phasesKey{}).(*Phases); ok {
		return p
	}
	return &Phases{}
}
