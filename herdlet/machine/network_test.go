package machine

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/everdict/herd/egress/nft/nfttest"
	"github.com/everdict/herd/egress/tapnet"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
)

// netHost is a Linux host whose VMs get TAP links (egress/tapnet) through a fake ip(8)/nft(8).
type netHost struct {
	host
	net *tapnet.Network
	ip  *nfttest.Host
}

func newNetHost(t *testing.T, reg *imagetest.Registry, pool string, store ...*fake.Store) netHost {
	t.Helper()
	dir := t.TempDir()
	ip := &nfttest.Host{}
	n := &tapnet.Network{Leases: &tapnet.Leases{Path: filepath.Join(dir, "leases.json"), Pool: netip.MustParsePrefix(pool)},
		Bindings: &tapnet.Bindings{Path: filepath.Join(dir, "egress-bindings.json")}, Host: ip, ProxyPort: 15001, DNSPort: 15053, TAPOwner: -1}
	var s *fake.Store
	if len(store) > 0 {
		s = store[0]
	}
	r := &recorder{Fake: fake.New(ubuntuCaps, s)}
	cache := &image.Cache{Dir: filepath.Join(dir, "images"), Source: reg, Builder: &imagetest.Builder{}, Platform: image.Platform{OS: "linux", Architecture: "amd64"}}
	m, err := New(Config{VMM: r, Images: cache, ShareDir: filepath.Join(dir, "shares"), Base: vmm.Spec{Kernel: "vmlinux"}, Network: n})
	if err != nil {
		t.Fatal(err)
	}
	return netHost{host: host{vm: r, m: m}, net: n, ip: ip}
}

// state is what the host's network holds for vm: its TAP (up or not), its table, the actor its address is bound to.
func (h netHost) state(t *testing.T, vm string) (tap string, up, table bool, bound string) {
	t.Helper()
	l, held, err := h.net.Leases.Held(vm)
	if err != nil || !held {
		return "", false, false, ""
	}
	up, ok := h.ip.TAPs()[l.TAP]
	if ok {
		tap = l.TAP
	}
	for _, tb := range h.ip.Tables() {
		table = table || tb == "herd_"+strings.ReplaceAll(vm, "-", "_")
	}
	if b, ok, _ := h.net.Bindings.Get(l.VM.String()); ok {
		bound = b.Space + "/" + b.Name
	}
	return tap, up, table, bound
}

func (h netHost) wantUp(t *testing.T, vm, actor string) {
	t.Helper()
	tap, up, table, bound := h.state(t, vm)
	if tap == "" || !up || !table || bound != actor {
		t.Errorf("%s: tap %q up %v table %v bound to %q, want its TAP up with its table and bound to %s", vm, tap, up, table, bound, actor)
	}
}

func (h netHost) wantDown(t *testing.T, vm string) {
	t.Helper()
	if tap, _, table, bound := h.state(t, vm); tap != "" || table || bound != "" {
		t.Errorf("%s: tap %q table %v bound to %q after its VM stopped, want none", vm, tap, table, bound)
	}
}

// TestLinkFollowsTheActor (criteria tap-table, binding, teardown): a booted VM's plan carries its link and its spec
// the TAP NIC; its TAP and table are up and its address bound to its actor while it runs; a suspend takes all three
// down, verified, and keeps the lease; a MEMORY restore comes back on the same link and binding; a second fork of
// the same memory cold-boots on a link of its own; delete releases everything.
func TestLinkFollowsTheActor(t *testing.T) {
	ctx := context.Background()
	h := newNetHost(t, registry(), "10.88.0.0/16")
	h.net.Assign("actor-1", tapnet.Actor{Space: "s", Name: "one", UID: "u1"})
	if err := h.m.Boot(ctx, "actor-1", template()); err != nil {
		t.Fatal(err)
	}
	plan := readPlan(t, h.m.ShareDir("actor-1"))
	nic := h.vm.booted[0].NIC
	if plan.Network == nil || nic == nil || plan.Network.MAC != nic.MAC || plan.Network.Gateway == "" || len(plan.Network.DNS) == 0 {
		t.Fatalf("plan network %+v, spec NIC %+v: want the guest's link in the plan and the TAP in the spec", plan.Network, nic)
	}
	h.wantUp(t, "actor-1", "s/one")

	var bundle bytes.Buffer
	if _, err := h.m.Snapshot(ctx, "actor-1", vmm.FidelityMemory, true, &bundle); err != nil {
		t.Fatal(err)
	}
	h.wantDown(t, "actor-1")
	if _, held, _ := h.net.Leases.Held("actor-1"); !held {
		t.Error("a suspend released the lease a Restart needs")
	}
	if p, err := h.m.Delete(ctx, "actor-1"); p != vmm.PresenceGone {
		t.Fatal(p, err)
	}
	if _, held, _ := h.net.Leases.Held("actor-1"); held {
		t.Error("delete kept the lease")
	}

	// Resume: the herdlet tells the network the actor, then restores the memory on the link it was taken on.
	h.net.Assign("actor-1", tapnet.Actor{Space: "s", Name: "one", UID: "u1"})
	if err := h.m.Restore(ctx, "actor-1", bytes.NewReader(bundle.Bytes())); err != nil {
		t.Fatal(err)
	}
	if got := h.vm.restored[0].NIC; got == nil || *got != *nic {
		t.Errorf("MEMORY restore on NIC %+v, want the one its memory was taken on, %+v", got, nic)
	}
	h.wantUp(t, "actor-1", "s/one")

	// Fork: the same memory again, while actor-1 runs: a cold boot on a link of its own.
	h.net.Assign("fork-2", tapnet.Actor{Space: "s", Name: "two", UID: "u2"})
	if err := h.m.Restore(ctx, "fork-2", bytes.NewReader(bundle.Bytes())); err != nil {
		t.Fatal(err)
	}
	if got := h.vm.booted[len(h.vm.booted)-1].NIC; got == nil || *got == *nic {
		t.Errorf("the second fork boots on %+v, want a link of its own (not %+v)", got, nic)
	}
	h.wantUp(t, "fork-2", "s/two")
	h.wantUp(t, "actor-1", "s/one")

	for _, id := range []string{"actor-1", "fork-2"} {
		if p, err := h.m.Delete(ctx, id); p != vmm.PresenceGone {
			t.Fatalf("delete %s: %v %v", id, p, err)
		}
		h.wantDown(t, id)
	}
	if len(h.ip.TAPs()) != 0 || len(h.ip.Tables()) != 0 {
		t.Errorf("after every delete: taps %v tables %v", h.ip.TAPs(), h.ip.Tables())
	}
}

// TestMoveClaimsTheLink (criterion binding): a MEMORY bundle moved to another host of its family restores on the
// link it was taken on there; when another VM holds that link on the target, the actor cold-boots on a link of its
// own instead of running its memory on another's address.
func TestMoveClaimsTheLink(t *testing.T) {
	ctx := context.Background()
	reg := registry()
	store := fake.NewStore()
	src := newNetHost(t, reg, "10.88.0.0/16", store)
	if err := src.m.Boot(ctx, "actor-1", template()); err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	if _, err := src.m.Snapshot(ctx, "actor-1", vmm.FidelityMemory, true, &bundle); err != nil {
		t.Fatal(err)
	}
	l, _, _ := src.net.Leases.Held("actor-1")

	dst := newNetHost(t, reg, "10.88.0.0/16", store)
	dst.net.Assign("actor-1", tapnet.Actor{Space: "s", Name: "one", UID: "u1"})
	if err := dst.m.Restore(ctx, "actor-1", bytes.NewReader(bundle.Bytes())); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := dst.net.Leases.Held("actor-1"); got != l || len(dst.vm.restored) != 1 {
		t.Errorf("moved memory runs on %+v (restored %d), want %+v", got, len(dst.vm.restored), l)
	}
	dst.wantUp(t, "actor-1", "s/one")

	busy := newNetHost(t, reg, "10.88.0.0/16", store)
	if _, err := busy.net.Leases.Claim("other", l); err != nil {
		t.Fatal(err)
	}
	if err := busy.m.Restore(ctx, "actor-1", bytes.NewReader(bundle.Bytes())); err != nil {
		t.Fatal(err)
	}
	got, _, _ := busy.net.Leases.Held("actor-1")
	if len(busy.vm.restored) != 0 || got == l {
		t.Errorf("memory restored %d times on link %+v while another VM holds %+v, want a cold boot on its own", len(busy.vm.restored), got, l)
	}
}

// TestDeleteWithLinkLeftIsNotGone (criterion teardown): a TAP that survives its removal makes the delete present
// (ErrDeleteNotVerified), never gone, and so does one that cannot be observed (unknown).
func TestDeleteWithLinkLeftIsNotGone(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		fault func(*nfttest.Host)
		want  vmm.Presence
	}{
		{"tap left", func(h *nfttest.Host) { h.KeepTAP = true }, vmm.PresencePresent},
		{"no answer", func(h *nfttest.Host) { h.Blind = true }, vmm.PresenceUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newNetHost(t, registry(), "10.88.0.0/16")
			if err := h.m.Boot(ctx, "actor-1", template()); err != nil {
				t.Fatal(err)
			}
			c.fault(h.ip)
			p, err := h.m.Delete(ctx, "actor-1")
			if p != c.want {
				t.Fatalf("Delete: %v (%v), want %v", p, err, c.want)
			}
			if c.want == vmm.PresencePresent && !errors.Is(err, vmm.ErrDeleteNotVerified) {
				t.Errorf("present without ErrDeleteNotVerified: %v", err)
			}
		})
	}
}
