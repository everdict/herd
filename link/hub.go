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

	mu      sync.Mutex
	workers map[string]*conn
	seq     uint64
}

type conn struct {
	reg     Register
	stream  Stream
	sendMu  sync.Mutex
	mu      sync.Mutex // guards pending and done
	pending map[string]chan Outcome
	done    chan struct{}
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
	c := &conn{reg: *up.Register, stream: s, pending: map[string]chan Outcome{}, done: make(chan struct{})}
	h.mu.Lock()
	if old, ok := h.workers[c.reg.Worker]; ok {
		// A reconnect replaces the old link; its pending calls end unknown.
		old.close()
	}
	h.workers[c.reg.Worker] = c
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.workers[c.reg.Worker] == c {
			delete(h.workers, c.reg.Worker)
		}
		h.mu.Unlock()
		c.close()
	}()
	if err := c.send(Down{Registered: &Registered{Worker: c.reg.Worker}}); err != nil {
		return err
	}
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
	ch, ok := c.pending[o.ID]
	delete(c.pending, o.ID)
	c.mu.Unlock()
	if ok {
		ch <- o
	}
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

// Call sends w to worker and waits for its outcome. It never fails open: a worker that is not connected, a link that
// breaks, or no answer before the deadline all come back as ResultUnknown with the reason.
func (h *Hub) Call(ctx context.Context, worker string, w Work) Outcome {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.Timeout)
		defer cancel()
	}
	dl, _ := ctx.Deadline()
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
	ch := make(chan Outcome, 1)
	c.mu.Lock()
	c.pending[w.ID] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, w.ID)
		c.mu.Unlock()
	}()
	if err := c.send(Down{Work: &w}); err != nil {
		return unknown(fmt.Sprintf("herdlet %q: send %s: %v", worker, w.Op, err))
	}
	select {
	case o := <-ch:
		return o
	case <-c.done:
		return unknown(fmt.Sprintf("herdlet %q: link closed before it answered %s", worker, w.Op))
	case <-ctx.Done():
		return unknown(fmt.Sprintf("herdlet %q: no answer to %s: %v", worker, w.Op, ctx.Err()))
	}
}
