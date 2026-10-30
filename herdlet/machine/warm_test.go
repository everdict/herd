package machine

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
)

// health fails the VMs it was told to fail, and otherwise asks the VMM.
type health struct {
	vm   vmm.VMM
	mu   sync.Mutex
	sick map[string]bool
}

func (h *health) fail(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sick[id] = true
}

func (h *health) Check(ctx context.Context, id string) error {
	h.mu.Lock()
	sick := h.sick[id]
	h.mu.Unlock()
	if sick {
		return errors.New("herd-guest does not answer")
	}
	if p, _ := h.vm.Inspect(ctx, id); p != vmm.PresencePresent {
		return errors.New("not running")
	}
	return nil
}

func newPool(t *testing.T, warm int) (*Pool, *health, host) {
	h := newHost(t, ubuntuCaps, registry())
	hc := &health{vm: h.vm, sick: map[string]bool{}}
	p := &Pool{Machines: h.m, Health: hc}
	tpl := template()
	tpl.Warm = warm
	p.Set([]link.Template{tpl})
	return p, hc, h
}

func TestPoolKeepsNWarm(t *testing.T) {
	ctx := context.Background()
	p, _, h := newPool(t, 2)
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if c := p.Counts()["tpl-1"]; c != 2 || h.vm.starts() != 2 {
		t.Fatalf("%d warm, %d booted; want 2, 2", c, h.vm.starts())
	}
	id, ok := p.Take(ctx, template())
	if !ok {
		t.Fatal("no warm VM handed out")
	}
	h.m.Adopt("actor-1", id)
	if p, _ := h.m.Inspect(ctx, "actor-1"); p != vmm.PresencePresent {
		t.Fatalf("adopted VM is %v", p)
	}
	if c := p.Counts()["tpl-1"]; c != 1 {
		t.Fatalf("%d warm after a take", c)
	}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if c := p.Counts()["tpl-1"]; c != 2 || h.vm.starts() != 3 {
		t.Fatalf("after refill %d warm, %d booted; want 2, 3", c, h.vm.starts())
	}
	if _, ok := p.Take(ctx, link.Template{UID: "other-template"}); ok {
		t.Fatal("a VM of another template was handed out")
	}
}

// TestPoolNeverHandsOutAFailedVM is the warm-vms counterexample: the warm VM at the head of the pool fails its
// health check after it was booted; Take must not hand it to the actor, must delete it, and the pool replaces it.
func TestPoolNeverHandsOutAFailedVM(t *testing.T) {
	ctx := context.Background()
	p, hc, h := newPool(t, 2)
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	first, second := p.ready["tpl-1"][0], p.ready["tpl-1"][1]
	p.mu.Unlock()
	hc.fail(first)
	id, ok := p.Take(ctx, template())
	if !ok || id != second {
		t.Fatalf("Take = %q, %v; want the healthy %q, not the failed %q", id, ok, second, first)
	}
	if pr, _ := h.vm.Inspect(ctx, first); pr != vmm.PresenceGone {
		t.Fatalf("the failed warm VM is %v, want deleted", pr)
	}
	hc.fail(second)
	if id, ok := p.Take(ctx, template()); ok {
		t.Fatalf("Take handed out %q with no healthy VM left", id)
	}
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if c := p.Counts()["tpl-1"]; c != 2 {
		t.Fatalf("%d warm after replacing, want 2", c)
	}
}

// A warm VM that fails while it waits is replaced on the next pass.
func TestPoolReplacesAFailedVMOnAPass(t *testing.T) {
	ctx := context.Background()
	p, hc, h := newPool(t, 1)
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	first := p.ready["tpl-1"][0]
	p.mu.Unlock()
	hc.fail(first)
	if err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	now := p.ready["tpl-1"]
	p.mu.Unlock()
	if len(now) != 1 || now[0] == first {
		t.Fatalf("ready %v; want one VM other than %s", now, first)
	}
	if pr, _ := h.vm.Inspect(ctx, first); pr != vmm.PresenceGone {
		t.Fatalf("the failed warm VM is %v", pr)
	}
}
