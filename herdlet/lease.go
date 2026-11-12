package herdlet

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/everdict/herd/link"
)

// lease is the deadline of a piece of work in flight, which herd-api may move later (link.Extend) as the work reports
// the bytes it moves (link.Progress).
type lease struct {
	mu       sync.Mutex
	deadline time.Time
	timer    *time.Timer
}

// withLease bounds ctx by w's deadline for as long as the returned func is not called; Extend moves it.
func (a *Agent) withLease(ctx context.Context, w link.Work) (context.Context, func()) {
	if w.DeadlineUnixMilli == 0 {
		return ctx, func() {}
	}
	inner, cancel := context.WithCancelCause(ctx)
	l := &lease{deadline: time.UnixMilli(w.DeadlineUnixMilli)}
	l.timer = time.AfterFunc(time.Until(l.deadline), func() { cancel(context.DeadlineExceeded) })
	a.leases.Store(w.ID, l)
	return leaseCtx{inner, l}, func() {
		a.leases.Delete(w.ID)
		l.timer.Stop()
		cancel(context.Canceled)
	}
}

// leaseCtx is a context whose deadline is its lease's, as it stands: what waits within the work (a registry's
// Retry-After) sees how long it has.
type leaseCtx struct {
	context.Context
	l *lease
}

func (c leaseCtx) Deadline() (time.Time, bool) {
	c.l.mu.Lock()
	defer c.l.mu.Unlock()
	if d, ok := c.Context.Deadline(); ok && d.Before(c.l.deadline) {
		return d, true
	}
	return c.l.deadline, true
}

// leaseDone is a lease's context without its deadline: it ends when the lease is cancelled (at its deadline as Extends
// move it), and a timeout derived from it is its own, not the lease's deadline as it stood when it was derived.
type leaseDone struct{ context.Context }

func (leaseDone) Deadline() (time.Time, bool) { return time.Time{}, false }

// Err says DeadlineExceeded once the lease ran out, as a context.WithDeadline would.
func (c leaseCtx) Err() error {
	err := c.Context.Err()
	if err != nil && context.Cause(c.Context) == context.DeadlineExceeded {
		return context.DeadlineExceeded
	}
	return err
}

// extend moves a lease's deadline later; one that ran out already stays ended.
func (a *Agent) extend(e link.Extend) {
	v, ok := a.leases.Load(e.ID)
	if !ok {
		return
	}
	l := v.(*lease)
	t := time.UnixMilli(e.DeadlineUnixMilli)
	l.mu.Lock()
	defer l.mu.Unlock()
	if !t.After(l.deadline) || !l.timer.Stop() {
		return
	}
	l.deadline = t
	l.timer.Reset(time.Until(t))
}

type progressKey struct{}

// withProgress makes report the way work under ctx tells herd-api how many bytes it has moved.
func withProgress(ctx context.Context, report func(bytes int64)) context.Context {
	return context.WithValue(ctx, progressKey{}, report)
}

// progress reports n bytes moved so far for the work of ctx, if anyone listens.
func progress(ctx context.Context, n int64) {
	if r, ok := ctx.Value(progressKey{}).(func(int64)); ok {
		r(n)
	}
}

// progressEvery is how often a growing snapshot object's size is reported while it is written.
var progressEvery = time.Second

// progressWriter counts what goes through it and reports the count at most every progressEvery.
type progressWriter struct {
	ctx  context.Context
	w    io.Writer
	n    int64
	last time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.n += int64(n)
	if now := time.Now(); now.Sub(p.last) >= progressEvery {
		p.last = now
		progress(p.ctx, p.n)
	}
	return n, err
}
