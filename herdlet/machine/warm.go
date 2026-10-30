package machine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
)

// Health checks that a warm VM can serve an actor.
type Health interface {
	Check(ctx context.Context, vmID string) error
}

// GuestHealth is the default check: the VMM reports the VM present and herd-guest accepts a connection on its port.
type GuestHealth struct {
	VMM    vmm.VMM
	Guests GuestDialer
	// Port is herd-guest's API port (8080).
	Port uint32
}

func (h GuestHealth) Check(ctx context.Context, vmID string) error {
	p, err := h.VMM.Inspect(ctx, vmID)
	if p != vmm.PresencePresent {
		return fmt.Errorf("VM is %s: %v", p, err)
	}
	if h.Guests == nil {
		return nil
	}
	c, err := h.Guests.DialGuest(ctx, vmID, h.Port)
	if err != nil {
		return fmt.Errorf("herd-guest does not answer: %w", err)
	}
	return c.Close()
}

// Pool keeps Warm booted VMs of each template it was given, hands one to an actor (Take) and refills in the
// background. A warm VM is checked before it is handed out and on every pass; one that fails is deleted and replaced,
// never handed out.
type Pool struct {
	Machines *Machines
	Health   Health
	// Interval is how often Run checks and refills (default 5 s).
	Interval time.Duration
	// Changed, if set, is called with the ready count per template after it changed.
	Changed func(map[string]int)

	mu        sync.Mutex
	templates map[string]link.Template
	ready     map[string][]string
	booting   map[string]int
	stale     []string
	kick      chan struct{}
}

func (p *Pool) init() {
	if p.templates == nil {
		p.templates, p.ready, p.booting = map[string]link.Template{}, map[string][]string{}, map[string]int{}
		p.kick = make(chan struct{}, 1)
	}
}

// Set declares the templates to keep warm (a template with Warm 0 keeps none). VMs of a template no longer declared
// are deleted on the next pass.
func (p *Pool) Set(ts []link.Template) {
	p.mu.Lock()
	p.init()
	p.templates = map[string]link.Template{}
	for _, t := range ts {
		p.templates[t.UID] = t
	}
	p.mu.Unlock()
	p.poke()
}

func (p *Pool) poke() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Counts is the number of ready warm VMs per template.
func (p *Pool) Counts() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]int{}
	for uid, ids := range p.ready {
		if len(ids) > 0 {
			out[uid] = len(ids)
		}
	}
	return out
}

func (p *Pool) changed() {
	if p.Changed != nil {
		p.Changed(p.Counts())
	}
}

// Take hands out a healthy warm VM of template t: its VMM id. The pool learns t's spec from herd-api here: a
// template declared by uid and count only starts warming after its first boot, and warm VMs booted from a spec
// that differs from t are never handed out (they are deleted on the next pass). A VM that fails its check is
// deleted and the next one is tried; false when none is left. A refill is started either way.
func (p *Pool) Take(ctx context.Context, t link.Template) (string, bool) {
	uid := t.UID
	p.learn(t)
	defer p.poke()
	took := false
	defer func() {
		if took {
			p.changed()
		}
	}()
	for {
		p.mu.Lock()
		p.init()
		ids := p.ready[uid]
		if len(ids) == 0 {
			p.mu.Unlock()
			return "", false
		}
		id := ids[0]
		p.ready[uid] = ids[1:]
		p.mu.Unlock()
		took = true
		if err := p.Health.Check(ctx, id); err != nil {
			p.discard(ctx, id)
			continue
		}
		return id, true
	}
}

func (p *Pool) discard(ctx context.Context, id string) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, _ = p.Machines.Delete(dctx, id)
}

// learn records t as the spec of a declared template; VMs of an older spec become stale.
func (p *Pool) learn(t link.Template) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.init()
	cur, ok := p.templates[t.UID]
	if !ok || sameSpec(cur, t) {
		return
	}
	t.Warm = cur.Warm
	p.templates[t.UID] = t
	p.stale = append(p.stale, p.ready[t.UID]...)
	p.ready[t.UID] = nil
}

func sameSpec(a, b link.Template) bool {
	a.Warm, b.Warm = 0, 0
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// Pass checks every ready VM, deletes the ones that fail or whose template is no longer declared, and boots
// replacements up to each template's Warm. It returns the first boot error.
func (p *Pool) Pass(ctx context.Context) error {
	p.mu.Lock()
	p.init()
	var check []struct{ uid, id string }
	for uid, ids := range p.ready {
		for _, id := range ids {
			check = append(check, struct{ uid, id string }{uid, id})
		}
	}
	stale := p.stale
	p.stale = nil
	p.mu.Unlock()
	dirty := false
	for _, id := range stale {
		p.discard(ctx, id)
	}
	for _, c := range check {
		p.mu.Lock()
		_, declared := p.templates[c.uid]
		p.mu.Unlock()
		if declared && p.Health.Check(ctx, c.id) == nil {
			continue
		}
		p.mu.Lock()
		p.ready[c.uid] = without(p.ready[c.uid], c.id)
		p.mu.Unlock()
		p.discard(ctx, c.id)
		dirty = true
	}
	p.mu.Lock()
	var want []link.Template
	uids := make([]string, 0, len(p.templates))
	for uid := range p.templates {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	for _, uid := range uids {
		t := p.templates[uid]
		if len(t.Containers) == 0 {
			continue // declared by count only; its spec arrives with its first boot
		}
		for n := len(p.ready[uid]) + p.booting[uid]; n < t.Warm; n++ {
			want = append(want, t)
			p.booting[uid]++
		}
	}
	p.mu.Unlock()
	var first error
	for _, t := range want {
		id, err := warmID()
		if err == nil {
			err = p.Machines.Boot(ctx, id, t)
		}
		if err == nil {
			err = p.Health.Check(ctx, id)
			if err != nil {
				p.discard(ctx, id)
			}
		}
		p.mu.Lock()
		p.booting[t.UID]--
		if err == nil {
			p.ready[t.UID] = append(p.ready[t.UID], id)
			dirty = true
		}
		p.mu.Unlock()
		if err != nil && first == nil {
			first = fmt.Errorf("machine: warm VM of template %s: %w", t.UID, err)
		}
	}
	if dirty {
		p.changed()
	}
	return first
}

// Run refills and checks the pool every Interval, and at once after Set or Take, until ctx ends. Warm VMs are left
// running when it returns; Drain deletes them.
func (p *Pool) Run(ctx context.Context) {
	p.mu.Lock()
	p.init()
	kick := p.kick
	p.mu.Unlock()
	iv := p.Interval
	if iv <= 0 {
		iv = 5 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		_ = p.Pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
	}
}

// Drain deletes every ready warm VM.
func (p *Pool) Drain(ctx context.Context) {
	p.mu.Lock()
	ready := p.ready
	p.ready = map[string][]string{}
	p.mu.Unlock()
	for _, ids := range ready {
		for _, id := range ids {
			p.discard(ctx, id)
		}
	}
}

func without(ids []string, id string) []string {
	out := ids[:0:0]
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

func warmID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "warm-" + hex.EncodeToString(b[:]), nil
}
