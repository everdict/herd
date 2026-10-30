package tapnet_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/everdict/herd/egress/nft/nfttest"
	"github.com/everdict/herd/egress/tapnet"
	"github.com/everdict/herd/vmm"
)

func newNet(t *testing.T, h *nfttest.Host) *tapnet.Network {
	t.Helper()
	dir := t.TempDir()
	return &tapnet.Network{Leases: &tapnet.Leases{Path: filepath.Join(dir, "leases.json")},
		Bindings: &tapnet.Bindings{Path: filepath.Join(dir, "egress-bindings.json")},
		Host:     h, ProxyPort: 15001, DNSPort: 15053, TAPOwner: -1,
		CA: func() ([]byte, error) {
			return []byte("-----BEGIN CERTIFICATE-----\nMA==\n-----END CERTIFICATE-----\n"), nil
		}}
}

// TestLeasesNoCollision (criterion tap-table): every VM gets its own /30, TAP and MAC, also when VM ids prefer the
// same slot and across a restart (a second Leases on the same file); a released slot is leased again; a full pool
// is an error, not a shared link.
func TestLeasesNoCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.json")
	small := netip.MustParsePrefix("10.99.0.0/28") // 4 links
	l := &tapnet.Leases{Path: path, Pool: small}
	seen := map[string]string{}
	var ids []string
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("vm-%d", i)
		ids = append(ids, id)
		got, err := l.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{got.TAP, got.MAC, got.VM.String(), got.Host.String()} {
			if other, ok := seen[k]; ok {
				t.Fatalf("%s and %s share %s", id, other, k)
			}
			seen[k] = id
		}
		if !small.Contains(got.VM) || got.Host.Bits() != 30 || !got.Host.Masked().Contains(got.VM) {
			t.Errorf("%s: %+v is not a /30 of %v", id, got, small)
		}
	}
	if _, err := l.Get("vm-4"); err == nil {
		t.Error("a fifth VM on four links was leased one")
	}
	again := &tapnet.Leases{Path: path, Pool: small}
	for _, id := range ids {
		a, _ := l.Get(id)
		b, err := again.Get(id)
		if err != nil || a != b {
			t.Errorf("%s after a restart: %+v (%v), want %+v", id, b, err, a)
		}
	}
	if err := again.Release("vm-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := again.Get("vm-4"); err != nil {
		t.Errorf("a released link was not leased again: %v", err)
	}
	if _, err := (&tapnet.Leases{Path: path, Pool: netip.MustParsePrefix("10.77.0.0/16")}).Get("x"); err == nil {
		t.Error("leases of one pool were read as another's")
	}
}

// TestClaimIsExact: a MEMORY restore's link is leased exactly or refused with ErrTaken.
func TestClaimIsExact(t *testing.T) {
	n := newNet(t, &nfttest.Host{})
	ctx := context.Background()
	g, nic, err := n.Lease(ctx, "src", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Release(ctx, "src"); err != nil {
		t.Fatal(err)
	}
	g2, nic2, err := n.Lease(ctx, "src", &g)
	if err != nil || g2.Address != g.Address || g2.MAC != g.MAC || *nic2 != *nic {
		t.Fatalf("claim of a free link: %+v %+v %v, want %+v %+v", g2, nic2, err, g, nic)
	}
	if _, _, err := n.Lease(ctx, "other", &g); !errors.Is(err, tapnet.ErrTaken) {
		t.Errorf("claim of a held link by another VM: %v, want ErrTaken", err)
	}
	bad := g
	bad.Address = "192.0.2.2/30"
	if _, _, err := n.Lease(ctx, "third", &bad); !errors.Is(err, tapnet.ErrTaken) {
		t.Errorf("claim of a link outside the pool: %v, want ErrTaken", err)
	}
}

// TestUpLoadsTableBeforeLinkUp (criterion tap-table): Up creates the TAP, addresses it, loads the VM's table
// herd_<id> and only then sets the link up; the guest's half routes and resolves through the host's address.
func TestUpLoadsTableBeforeLinkUp(t *testing.T) {
	h := &nfttest.Host{}
	n := newNet(t, h)
	ctx := context.Background()
	g, nic, err := n.Lease(ctx, "a1b2-c3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Up(ctx, "a1b2-c3"); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range h.Commands() {
		switch {
		case strings.HasPrefix(c, "ip tuntap add dev "+nic.TAP+" mode tap"):
			order = append(order, "tap")
		case strings.HasPrefix(c, "ip addr add "):
			order = append(order, "addr")
		case c == "nft -f - <<ruleset herd_a1b2_c3":
			order = append(order, "table")
		case c == "ip link set "+nic.TAP+" up":
			order = append(order, "up")
		}
	}
	if got := strings.Join(order, ","); got != "tap,addr,table,up" {
		t.Errorf("Up ran %q (%v), want the table loaded before the link is up", got, h.Commands())
	}
	if up, ok := h.TAPs()[nic.TAP]; !ok || !up {
		t.Errorf("TAP %s: exists %v up %v", nic.TAP, ok, up)
	}
	gw := strings.Split(g.Address, "/")[0]
	if g.Gateway == "" || len(g.DNS) != 1 || g.DNS[0] != g.Gateway || gw == g.Gateway || !strings.HasSuffix(g.Address, "/30") || g.CA == "" || g.MAC != nic.MAC {
		t.Errorf("guest half %+v: want an address on a /30, the default route and resolver at the host's address, the CA, the NIC's MAC", g)
	}
}

// TestBindingFollowsTheVM (criterion binding): the actor is bound to the VM's address while the VM's link is up —
// assigned before Up (a boot) or after it (a warm VM adopted) — and unbound by Down.
func TestBindingFollowsTheVM(t *testing.T) {
	ctx := context.Background()
	for _, before := range []bool{true, false} {
		t.Run(fmt.Sprintf("assigned before up %v", before), func(t *testing.T) {
			n := newNet(t, &nfttest.Host{})
			a := tapnet.Actor{Space: "s", Name: "agent", UID: "u1"}
			if before {
				n.Assign("vm1", a)
			}
			g, _, err := n.Lease(ctx, "vm1", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := n.Up(ctx, "vm1"); err != nil {
				t.Fatal(err)
			}
			if !before {
				n.Assign("vm1", a)
			}
			src := strings.Split(g.Address, "/")[0]
			b, ok, err := n.Bindings.Get(src)
			if err != nil || !ok || b != (tapnet.Binding{Space: "s", Name: "agent", UID: "u1", Source: src}) {
				t.Fatalf("binding of %s: %+v %v %v", src, b, ok, err)
			}
			if p, err := n.Down(ctx, "vm1"); p != vmm.PresenceGone {
				t.Fatal(p, err)
			}
			if _, ok, _ := n.Bindings.Get(src); ok {
				t.Errorf("%s still bound after Down", src)
			}
		})
	}
}

// TestDownVerifiesGone (criterion teardown): Down answers gone only when the TAP, the table and the binding are
// observed absent; a TAP or table left behind is present (ErrDeleteNotVerified), a host that does not answer is
// unknown, never gone.
func TestDownVerifiesGone(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		host *nfttest.Host
		want vmm.Presence
	}{
		{"clean", &nfttest.Host{}, vmm.PresenceGone},
		{"tap left", &nfttest.Host{KeepTAP: true}, vmm.PresencePresent},
		{"table left", &nfttest.Host{KeepTable: true}, vmm.PresencePresent},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := newNet(t, c.host)
			n.Assign("vm1", tapnet.Actor{Space: "s", Name: "a", UID: "u"})
			_, nic, err := n.Lease(ctx, "vm1", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := n.Up(ctx, "vm1"); err != nil {
				t.Fatal(err)
			}
			p, err := n.Release(ctx, "vm1")
			if p != c.want {
				t.Fatalf("Release: %v (%v), want %v", p, err, c.want)
			}
			if c.want == vmm.PresencePresent && !errors.Is(err, vmm.ErrDeleteNotVerified) {
				t.Errorf("present without ErrDeleteNotVerified: %v", err)
			}
			if c.want == vmm.PresenceGone {
				if _, ok := c.host.TAPs()[nic.TAP]; ok || len(c.host.Tables()) != 0 {
					t.Errorf("after a gone Release: taps %v tables %v", c.host.TAPs(), c.host.Tables())
				}
				if _, held, _ := n.Leases.Held("vm1"); held {
					t.Error("the lease outlived Release")
				}
			} else if _, held, _ := n.Leases.Held("vm1"); !held {
				t.Error("a Release not verified freed the lease")
			}
		})
	}
	t.Run("no answer", func(t *testing.T) {
		h := &nfttest.Host{}
		n := newNet(t, h)
		if _, _, err := n.Lease(ctx, "vm1", nil); err != nil {
			t.Fatal(err)
		}
		if err := n.Up(ctx, "vm1"); err != nil {
			t.Fatal(err)
		}
		h.Blind = true
		if p, err := n.Down(ctx, "vm1"); p != vmm.PresenceUnknown {
			t.Errorf("Down on a host that does not answer: %v (%v), want unknown", p, err)
		}
	})
}

// TestUpFailureLeavesNothing: a table that does not load leaves no TAP behind, and the VM is not bound.
func TestUpFailureLeavesNothing(t *testing.T) {
	h := &nfttest.Host{FailApply: "nft -f"}
	n := newNet(t, h)
	ctx := context.Background()
	n.Assign("vm1", tapnet.Actor{Space: "s", Name: "a", UID: "u"})
	g, _, err := n.Lease(ctx, "vm1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Up(ctx, "vm1"); err == nil {
		t.Fatal("Up with a table that does not load succeeded")
	}
	if len(h.TAPs()) != 0 {
		t.Errorf("TAPs left: %v", h.TAPs())
	}
	if _, ok, _ := n.Bindings.Get(strings.Split(g.Address, "/")[0]); ok {
		t.Error("a VM whose link failed is bound")
	}
}

// TestAttachedReadsTheCarrier: a TAP the hypervisor holds (LOWER_UP) is attached; one nobody holds open (the
// NO-CARRIER "state DOWN" TAP a real run showed) is reported, naming the TAP.
func TestAttachedReadsTheCarrier(t *testing.T) {
	ctx := context.Background()
	h := &nfttest.Host{}
	n := newNet(t, h)
	if _, _, err := n.Lease(ctx, "vm1", nil); err != nil {
		t.Fatal(err)
	}
	if err := n.Up(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	if err := n.Attached(ctx, "vm1"); err != nil {
		t.Errorf("attached TAP: %v", err)
	}
	h.Detached = true
	if err := n.Attached(ctx, "vm1"); err == nil || !strings.Contains(err.Error(), "no carrier") {
		t.Errorf("TAP nobody holds: %v, want no carrier", err)
	}
}

// TestInputDrops: an input chain with policy drop in another table (ufw through iptables-nft, firewalld) is named;
// herd's own tables and accepting chains are not.
func TestInputDrops(t *testing.T) {
	rs := `table ip filter {
	chain INPUT {
		type filter hook input priority filter; policy drop;
		counter packets 0 bytes 0 jump ufw-before-input
	}
	chain FORWARD {
		type filter hook forward priority filter; policy drop;
	}
}
table inet open {
	chain input {
		type filter hook input priority 0; policy accept;
	}
}
table inet herd_a1 {
	chain input {
		type filter hook input priority filter; policy drop;
	}
}
`
	got := tapnet.InputDrops(rs)
	if len(got) != 1 || got[0] != "table ip filter chain INPUT" {
		t.Errorf("InputDrops = %q, want only ip filter INPUT", got)
	}
	n := newNet(t, &nfttest.Host{Ruleset: rs})
	if got, err := n.HostFirewall(context.Background()); err != nil || len(got) != 1 {
		t.Errorf("HostFirewall = %q, %v", got, err)
	}
}

// ufwRuleset is an Ubuntu host with ufw and Tailscale (iptables-nft): ip and ip6 filter INPUT drop by policy.
const ufwRuleset = `table ip filter {
	chain INPUT {
		type filter hook input priority filter; policy drop;
		counter packets 0 bytes 0 jump ts-input
		counter packets 0 bytes 0 jump ufw-before-logging-input
	}
}
table ip6 filter {
	chain INPUT {
		type filter hook input priority filter; policy drop;
	}
}
`

// TestFirewallHolesFollowTheLink (criteria tap-table, teardown): on a host whose ip filter INPUT drops by policy
// (ufw, Tailscale), Up inserts exactly the VM's two accepts there (none into ip6, which never sees the VM's IPv4),
// Down removes them and answers gone only once none is left; an accept that survives its removal is present.
func TestFirewallHolesFollowTheLink(t *testing.T) {
	ctx := context.Background()
	h := &nfttest.Host{Ruleset: ufwRuleset}
	n := newNet(t, h)
	_, nic, err := n.Lease(ctx, "vm-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Up(ctx, "vm-1"); err != nil {
		t.Fatal(err)
	}
	l, _, _ := n.Leases.Held("vm-1")
	want := []string{
		fmt.Sprintf(`ip filter INPUT: iifname "%s" ip daddr %s tcp dport 15001 accept comment "herd-egress:herd_vm_1"`, nic.TAP, l.Host.Addr()),
		fmt.Sprintf(`ip filter INPUT: iifname "%s" ip daddr %s udp dport 15053 accept comment "herd-egress:herd_vm_1"`, nic.TAP, l.Host.Addr()),
	}
	if got := h.Holes(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("accepts inserted:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if p, err := n.Down(ctx, "vm-1"); p != vmm.PresenceGone || len(h.Holes()) != 0 {
		t.Fatalf("Down: %v %v, accepts left %q", p, err, h.Holes())
	}

	// An accept left behind is not gone.
	if err := n.Up(ctx, "vm-1"); err != nil {
		t.Fatal(err)
	}
	h.KeepHoles = true
	if p, err := n.Down(ctx, "vm-1"); p != vmm.PresencePresent || !errors.Is(err, vmm.ErrDeleteNotVerified) {
		t.Errorf("Down with an accept left: %v %v, want present", p, err)
	}
}

// TestFirewallRefuse: with -host-firewall=refuse, Up on such a host fails before it makes anything, naming the
// chain and the commands that let the flows in.
func TestFirewallRefuse(t *testing.T) {
	ctx := context.Background()
	h := &nfttest.Host{Ruleset: ufwRuleset}
	n := newNet(t, h)
	n.Firewall = tapnet.FirewallRefuse
	if _, _, err := n.Lease(ctx, "vm-1", nil); err != nil {
		t.Fatal(err)
	}
	err := n.Up(ctx, "vm-1")
	if err == nil || !strings.Contains(err.Error(), "ip filter INPUT") || !strings.Contains(err.Error(), "iptables -I INPUT -i herd+ -p udp --dport 15053 -j ACCEPT") {
		t.Fatalf("Up refused with %v", err)
	}
	if len(h.TAPs()) != 0 || len(h.Tables()) != 0 || len(h.Holes()) != 0 {
		t.Errorf("a refused Up left taps %v tables %v accepts %v", h.TAPs(), h.Tables(), h.Holes())
	}
}
