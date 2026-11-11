package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Stream is the herd-api side of one herdlet's link, carrying encoded messages.
type Stream interface {
	Send([]byte) error
	Recv() ([]byte, error)
}

// Worker is a registered herdlet as herd-api sees it.
type Worker struct {
	Register
	Connected bool
}

// Hub holds the herdlets connected to this herd-api and routes work to them.
type Hub struct {
	// Timeout bounds a call with no deadline of its own.
	Timeout time.Duration
	// OnRegister, if set, is called with each registration, capacity update and capabilities update before it is
	// acknowledged.
	OnRegister func(ctx context.Context, r Register) error
	// OnExit, if set, is called with each VM exit a herdlet reports, with the worker the link registered. It must not
	// block on work it sends to the same herdlet: the link reads nothing else until it returns.
	OnExit func(ctx context.Context, worker string, e Exited)
	// OnLinked, if set, is called once a herdlet's link is up: its registration is the worker's link (Instance
	// answers it), and any earlier link of the worker has been closed, its pending calls ended unknown. It must not
	// block: the link reads nothing until it returns.
	OnLinked func(ctx context.Context, r Register)
	// StoreID names herd-api's record store; every herdlet's Registered carries it (PLT-458).
	StoreID string

	mu      sync.Mutex
	workers map[string]*conn
	seq     uint64
}

type conn struct {
	reg     Register
	stream  Stream
	sendMu  sync.Mutex
	mu      sync.Mutex // guards pending and done
	pending map[string]*pending
	done    chan struct{}

	// instance is reg.Instance, which no update changes.
	instance string
}

// pending is a call waiting for its outcome.
type pending struct {
	out chan Outcome
	// progress holds the latest bytes the herdlet reported for the call.
	progress chan int64
}

// NewHub returns a hub with no herdlets.
func NewHub() *Hub { return &Hub{Timeout: 30 * time.Second, workers: map[string]*conn{}} }

// ErrNotRegistered is what Serve returns when the first message is not a registration.
var ErrNotRegistered = errors.New("link: first message must be register")

// Serve runs one herdlet's link until it ends. The herdlet connected to us; Serve never dials.
func (h *Hub) Serve(ctx context.Context, s Stream) error {
	b, err := s.Recv()
	if err != nil {
		return err
	}
	var up Up
	if err := json.Unmarshal(b, &up); err != nil || up.Register == nil || up.Register.Worker == "" {
		return ErrNotRegistered
	}
	if h.OnRegister != nil {
		if err := h.OnRegister(ctx, *up.Register); err != nil {
			return err
		}
	}
	c := &conn{reg: *up.Register, instance: up.Register.Instance, stream: s, pending: map[string]*pending{}, done: make(chan struct{})}
	// Registered goes before any work can (the link is not the worker's until below): the herdlet knows the store of
	// every VM it starts on this link (Registered.StoreID).
	if err := c.send(Down{Registered: &Registered{Worker: c.reg.Worker, StoreID: h.StoreID}}); err != nil {
		return err
	}
	h.mu.Lock()
	if old, ok := h.workers[c.reg.Worker]; ok {
		// A reconnect replaces the old link; its pending calls end unknown.
		old.close()
	}
	h.workers[c.reg.Worker] = c
	h.mu.Unlock()
	if h.OnLinked != nil {
		h.OnLinked(ctx, *up.Register)
	}
	defer func() {
		h.mu.Lock()
		if h.workers[c.reg.Worker] == c {
			delete(h.workers, c.reg.Worker)
		}
		h.mu.Unlock()
		c.close()
	}()
	for {
		b, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var up Up
		if err := json.Unmarshal(b, &up); err != nil {
			return fmt.Errorf("link: decode: %w", err)
		}
		switch {
		case up.Outcome != nil:
			c.deliver(*up.Outcome)
		case up.Progress != nil:
			c.progress(*up.Progress)
		case up.Exited != nil:
			if h.OnExit != nil {
				h.OnExit(ctx, c.reg.Worker, *up.Exited)
			}
		case up.Capacity != nil || up.Capabilities != nil:
			h.mu.Lock()
			if up.Capacity != nil {
				c.reg.Capacity = *up.Capacity
			}
			if up.Capabilities != nil {
				c.reg.Capabilities = *up.Capabilities
			}
			reg := c.reg
			h.mu.Unlock()
			if h.OnRegister != nil {
				if err := h.OnRegister(ctx, reg); err != nil {
					return err
				}
			}
		}
	}
}

func (c *conn) send(d Down) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.stream.Send(b)
}

func (c *conn) deliver(o Outcome) {
	c.mu.Lock()
	p, ok := c.pending[o.ID]
	delete(c.pending, o.ID)
	c.mu.Unlock()
	if ok {
		p.out <- o
	}
}

// progress hands the call its latest report; an older one not yet read is replaced.
func (c *conn) progress(r Progress) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pending[r.ID]
	if !ok {
		return
	}
	select {
	case <-p.progress:
	default:
	}
	p.progress <- r.Bytes
}

func (c *conn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

// Workers returns the connected herdlets ordered by name.
func (h *Hub) Workers() []Worker {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Worker, 0, len(h.workers))
	for _, c := range h.workers {
		out = append(out, Worker{Register: c.reg, Connected: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Worker < out[j].Worker })
	return out
}

// Instance is the Register.Instance of the worker's link, and whether the worker is connected.
func (h *Hub) Instance(worker string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.workers[worker]
	if !ok {
		return "", false
	}
	return c.instance, true
}

// Call sends w to worker and waits for its outcome. It never fails open: a worker that is not connected, a link that
// breaks, or no answer before the deadline all come back as ResultUnknown with the reason.
func (h *Hub) Call(ctx context.Context, worker string, w Work) Outcome {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.Timeout)
		defer cancel()
	}
	dl, _ := ctx.Deadline()
	return h.call(ctx, worker, w, dl, nil)
}

// CallSized is Call for work whose time follows the bytes it moves: the call waits timeout plus grow(n), where n is
// the most bytes the herdlet has reported for it (Progress; none at first). Each report that moves the deadline later
// is sent to the herdlet (Extend), so it works on as long as the call waits. A deadline of ctx still bounds the call.
func (h *Hub) CallSized(ctx context.Context, worker string, w Work, timeout time.Duration, grow func(bytes int64) time.Duration) Outcome {
	start := time.Now()
	return h.call(ctx, worker, w, start.Add(timeout+grow(0)), func(n int64) time.Time { return start.Add(timeout + grow(n)) })
}

func (h *Hub) call(ctx context.Context, worker string, w Work, dl time.Time, sized func(int64) time.Time) Outcome {
	if cdl, ok := ctx.Deadline(); ok && cdl.Before(dl) {
		dl = cdl
	}
	h.mu.Lock()
	c, ok := h.workers[worker]
	h.seq++
	w.ID = worker + "-" + strconv.FormatUint(h.seq, 10)
	h.mu.Unlock()
	unknown := func(reason string) Outcome {
		return Outcome{ID: w.ID, Result: ResultUnknown, Reason: reason}
	}
	if !ok {
		return unknown(fmt.Sprintf("herdlet %q is not connected", worker))
	}
	w.DeadlineUnixMilli = dl.UnixMilli()
	p := &pending{out: make(chan Outcome, 1), progress: make(chan int64, 1)}
	c.mu.Lock()
	c.pending[w.ID] = p
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, w.ID)
		c.mu.Unlock()
	}()
	if err := c.send(Down{Work: &w}); err != nil {
		return unknown(fmt.Sprintf("herdlet %q: send %s: %v", worker, w.Op, err))
	}
	timer := time.NewTimer(time.Until(dl))
	defer timer.Stop()
	for {
		select {
		case o := <-p.out:
			o.Instance = c.instance
			return o
		case <-c.done:
			return unknown(fmt.Sprintf("herdlet %q: link closed before it answered %s", worker, w.Op))
		case <-ctx.Done():
			return unknown(fmt.Sprintf("herdlet %q: no answer to %s: %v", worker, w.Op, ctx.Err()))
		case <-timer.C:
			return unknown(fmt.Sprintf("herdlet %q: no answer to %s: %v", worker, w.Op, context.DeadlineExceeded))
		case n := <-p.progress:
			if sized == nil {
				continue
			}
			next := sized(n)
			if cdl, ok := ctx.Deadline(); ok && cdl.Before(next) {
				next = cdl
			}
			if next.UnixMilli() <= dl.UnixMilli() {
				continue
			}
			dl = next
			timer.Reset(time.Until(dl))
			// A herdlet that misses this works to the old deadline and answers unknown, as without the report.
			_ = c.send(Down{Extend: &Extend{ID: w.ID, DeadlineUnixMilli: dl.UnixMilli()}})
		}
	}
}
