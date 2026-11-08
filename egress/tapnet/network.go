package tapnet

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"strings"
	"sync"

	"github.com/everdict/herd/egress/nft"
	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/vmm"
)

// Actor names the actor a VM serves, as herd-egress's bindings do.
type Actor struct {
	Space, Name, UID string
}

// Network is a Linux host's VM networks (machine.Network): a lease per VM id, a TAP and egress table per running
// VM, and the VM's binding to its actor.
//   - Lease leases the VM its link and answers the guest's half for herd-guest's plan (address, default route and
//     resolver at the host's address, herd-egress's CA) and the NIC Cloud Hypervisor attaches.
//   - Up creates the TAP, loads the table, then sets the link up (nft.Apply), and binds the VM's address to its
//     actor once it has one, answering only when herd-egress has acknowledged the binding (Bindings.Set); before Boot
//     and Restore, so the guest cannot send before herd-egress knows its address.
//   - Down unbinds and removes the TAP and the table, after the VM was verified gone, and answers gone only once all
//     three are observed absent; the lease stays (a suspended VM restarts or restores on its link).
//   - Release is Down and frees the lease (the VM is deleted).
//
// Assign tells it which actor a VM is (herdlet's OnActor); a VM up before its actor (a warm VM) is bound when it
// gets one.
type Network struct {
	Leases   *Leases
	Bindings *Bindings
	// Host runs ip(8) and nft(8): nft.ExecHost{} on a host, a fake in tests.
	Host nft.Host
	// ProxyPort and DNSPort are herd-egress's transparent listener and synthetic DNS on the host.
	ProxyPort, DNSPort uint16
	// TAPOwner is the uid that owns the TAPs (-1: root, as herdlet runs).
	TAPOwner int
	// CA, if set, answers herd-egress's CA certificate (PEM) for the guests to trust; it is read at each lease, so a
	// herd-egress that wrote its CA after the herdlet started is picked up.
	CA func() ([]byte, error)
	// Firewall is what Up does when another table's input chain drops by policy (nft.InputDropChains), which would drop
	// the VM's redirected flows whatever herd's table accepts: FirewallInsert (also "") puts the VM's accepts
	// (nft.HoleRules) at the top of each such chain and Down removes them, verified; FirewallRefuse fails Up, naming
	// the chains and the commands that would let the flows in.
	Firewall string

	mu     sync.Mutex
	actors map[string]Actor
	up     map[string]bool
}

func (n *Network) vm(id string, l Lease) nft.VM {
	return nft.VM{ID: tableID(id, l), TAP: l.TAP, Host: l.Host, VM: l.VM, ProxyPort: n.ProxyPort, DNSPort: n.DNSPort}
}

// tableID is the VM's table name after herd_: its id with dashes as underscores, or its slot when the id is too long
// for a table name.
func tableID(id string, l Lease) string {
	t := strings.ReplaceAll(id, "-", "_")
	if len(t) > 40 {
		return fmt.Sprintf("slot%d", l.Slot)
	}
	return t
}

// Guest is the guest's half of lease l: herd-guest's plan.
func (n *Network) guest(l Lease) (initd.Network, error) {
	g := initd.Network{MAC: l.MAC, Address: netip.PrefixFrom(l.VM, l.Host.Bits()).String(), Gateway: l.Host.Addr().String(),
		// UDP 53 to any address is herd-egress's synthetic DNS (the table's redirect).
		DNS: []string{l.Host.Addr().String()}}
	if n.CA != nil {
		ca, err := n.CA()
		if err != nil {
			return initd.Network{}, fmt.Errorf("tapnet: herd-egress CA: %w", err)
		}
		g.CA = string(ca)
	}
	return g, nil
}

// Lease leases VM id its link (or answers the one it holds). want, if set, is the link a MEMORY snapshot of the VM
// was taken on: the VM's memory keeps that address, so the lease must be exactly that link, else ErrTaken.
func (n *Network) Lease(ctx context.Context, id string, want *initd.Network) (initd.Network, *vmm.NIC, error) {
	if err := ctx.Err(); err != nil {
		return initd.Network{}, nil, err
	}
	var l Lease
	var err error
	if want == nil {
		l, err = n.Leases.Get(id)
	} else {
		var wl Lease
		if wl, err = n.leaseOf(*want); err == nil {
			l, err = n.Leases.Claim(id, wl)
		}
	}
	if err != nil {
		return initd.Network{}, nil, err
	}
	g, err := n.guest(l)
	if err != nil {
		return initd.Network{}, nil, err
	}
	log.Printf("tapnet: %s: leased %s, guest %s via %s, tap %s mac %s (claimed: %v, CA: %v)", id, l.Host.Masked(), g.Address, g.Gateway, l.TAP, l.MAC, want != nil, g.CA != "")
	return g, &vmm.NIC{TAP: l.TAP, MAC: l.MAC}, nil
}

// leaseOf is the pool's lease whose guest half is g.
func (n *Network) leaseOf(g initd.Network) (Lease, error) {
	pfx, err := netip.ParsePrefix(g.Address)
	if err != nil {
		return Lease{}, fmt.Errorf("%w: address %q: %v", ErrTaken, g.Address, err)
	}
	p, size, err := n.Leases.pool()
	if err != nil {
		return Lease{}, err
	}
	if !p.Contains(pfx.Addr()) {
		return Lease{}, fmt.Errorf("%w: %v is not in this host's pool %v", ErrTaken, pfx.Addr(), p)
	}
	a, b := pfx.Addr().As4(), p.Addr().As4()
	off := (uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])) - (uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]))
	s := int(off / 4)
	if s >= size {
		return Lease{}, fmt.Errorf("%w: %v is not in this host's pool %v", ErrTaken, pfx.Addr(), p)
	}
	l := lease(p, s)
	if l.MAC != g.MAC || l.VM != pfx.Addr() || l.Host.Addr().String() != g.Gateway || l.Host.Bits() != pfx.Bits() {
		return Lease{}, fmt.Errorf("%w: %+v is not a link of pool %v", ErrTaken, g, p)
	}
	return l, nil
}

// Assign records that VM id is actor a's; a VM that is up is bound at once.
func (n *Network) Assign(id string, a Actor) {
	n.mu.Lock()
	if n.actors == nil {
		n.actors = map[string]Actor{}
	}
	n.actors[id] = a
	up := n.up[id]
	n.mu.Unlock()
	if !up {
		return
	}
	if err := n.bind(id); err != nil {
		log.Printf("tapnet: binding %s to %s/%s: %v", id, a.Space, a.Name, err)
	}
}

func (n *Network) bind(id string) error {
	n.mu.Lock()
	a, ok := n.actors[id]
	n.mu.Unlock()
	if !ok {
		return nil
	}
	l, held, err := n.Leases.Held(id)
	if err != nil || !held {
		return err
	}
	if err := n.Bindings.Set(Binding{Space: a.Space, Name: a.Name, UID: a.UID, Source: l.VM.String()}); err != nil {
		return err
	}
	log.Printf("tapnet: %s: bound %s to %s/%s (%s) in %s", id, l.VM, a.Space, a.Name, a.UID, n.Bindings.Path)
	return nil
}

// Up creates the VM's TAP and table (removing what a VM of this lease left, e.g. after a host crash) and binds the
// VM to its actor. On failure nothing it made is left.
func (n *Network) Up(ctx context.Context, id string) error {
	l, held, err := n.Leases.Held(id)
	if err != nil {
		return err
	}
	if !held {
		return fmt.Errorf("tapnet: %s holds no lease", id)
	}
	v := n.vm(id, l)
	if p, err := n.down(ctx, id, l); p != vmm.PresenceGone {
		return fmt.Errorf("tapnet: %s: what an earlier VM left on %s is %v: %w", id, l.TAP, p, err)
	}
	if err := n.openFirewall(ctx, id, v); err != nil {
		return err
	}
	if err := nft.Apply(ctx, n.Host, v, n.TAPOwner); err != nil {
		_ = nft.RemoveHoles(context.WithoutCancel(ctx), n.Host, v)
		return fmt.Errorf("tapnet: %s: %w", id, err)
	}
	n.mu.Lock()
	if n.up == nil {
		n.up = map[string]bool{}
	}
	n.up[id] = true
	n.mu.Unlock()
	log.Printf("tapnet: %s: up: tap %s %s, table inet %s (tcp to :%d, udp 53 to :%d) loaded before link up", id, l.TAP, l.Host, v.Table(), n.ProxyPort, n.DNSPort)
	if err := n.bind(id); err != nil {
		_, _ = n.Down(context.WithoutCancel(ctx), id)
		return err
	}
	return nil
}

// Down unbinds the VM and removes its TAP and table, then observes all three gone. Present means something is
// observed still there (vmm.ErrDeleteNotVerified); unknown means it could not be observed.
func (n *Network) Down(ctx context.Context, id string) (vmm.Presence, error) {
	l, held, err := n.Leases.Held(id)
	if err != nil {
		return vmm.PresenceUnknown, err
	}
	if !held {
		return vmm.PresenceGone, nil
	}
	return n.down(ctx, id, l)
}

func (n *Network) down(ctx context.Context, id string, l Lease) (vmm.Presence, error) {
	n.mu.Lock()
	delete(n.up, id)
	n.mu.Unlock()
	if err := n.Bindings.Remove(l.VM.String()); err != nil {
		return vmm.PresenceUnknown, fmt.Errorf("tapnet: %s: binding: %w", id, err)
	}
	v := n.vm(id, l)
	tap, table, holes, err := n.present(ctx, v)
	if err != nil {
		return vmm.PresenceUnknown, fmt.Errorf("tapnet: %s: %w", id, err)
	}
	if !tap && !table && holes == 0 {
		return vmm.PresenceGone, nil
	}
	var rmErr error
	if tap || table {
		rmErr = nft.Remove(ctx, n.Host, v)
	}
	if holes > 0 {
		if err := nft.RemoveHoles(ctx, n.Host, v); err != nil && rmErr == nil {
			rmErr = err
		}
	}
	tap, table, holes, err = n.present(ctx, v)
	switch {
	case err != nil:
		return vmm.PresenceUnknown, fmt.Errorf("tapnet: %s: after removing %s, %s and its firewall accepts: %w", id, v.TAP, v.Table(), err)
	case tap || table || holes > 0:
		return vmm.PresencePresent, fmt.Errorf("tapnet: %s: tap %s present %v, table %s present %v, %d firewall accepts left after removal (%v): %w",
			id, v.TAP, tap, v.Table(), table, holes, rmErr, vmm.ErrDeleteNotVerified)
	}
	log.Printf("tapnet: %s: down: tap %s, table inet %s, its firewall accepts and the binding of %s verified gone", id, v.TAP, v.Table(), l.VM)
	return vmm.PresenceGone, nil
}

// Release is Down, then frees the VM's lease and forgets its actor.
func (n *Network) Release(ctx context.Context, id string) (vmm.Presence, error) {
	if p, err := n.Down(ctx, id); p != vmm.PresenceGone {
		return p, err
	}
	if err := n.Leases.Release(id); err != nil {
		return vmm.PresenceUnknown, err
	}
	log.Printf("tapnet: %s: released its lease", id)
	n.mu.Lock()
	delete(n.actors, id)
	n.mu.Unlock()
	return vmm.PresenceGone, nil
}

// The host-firewall modes (Network.Firewall).
const (
	FirewallInsert = "insert"
	FirewallRefuse = "refuse"
)

// present observes the VM's TAP, table and inserted firewall accepts.
func (n *Network) present(ctx context.Context, v nft.VM) (tap, table bool, holes int, err error) {
	tap, table, err = nft.Present(ctx, n.Host, v)
	if err != nil {
		return false, false, 0, err
	}
	hs, err := nft.Holes(ctx, n.Host, v)
	return tap, table, len(hs), err
}

// openFirewall lets the VM's redirected flows past other tables' input chains that drop by policy: it inserts the
// VM's accepts there (FirewallInsert), or refuses (FirewallRefuse).
func (n *Network) openFirewall(ctx context.Context, id string, v nft.VM) error {
	out, err := n.Host.Run(ctx, nft.Command{Name: "nft", Args: []string{"list", "ruleset"}})
	if err != nil {
		return fmt.Errorf("tapnet: %s: reading the host's ruleset: %w", id, err)
	}
	var chains []nft.Chain
	for _, c := range nft.InputDropChains(string(out)) {
		if c.Holed() {
			chains = append(chains, c)
		}
	}
	if len(chains) == 0 {
		return nil
	}
	switch n.Firewall {
	case "", FirewallInsert:
	case FirewallRefuse:
		return fmt.Errorf("tapnet: %s: %v drop input by policy, which drops the VM's DNS and TCP to herd-egress, and -host-firewall=refuse: let them in, e.g. "+
			"iptables -I INPUT -i herd+ -p tcp --dport %d -j ACCEPT; iptables -I INPUT -i herd+ -p udp --dport %d -j ACCEPT "+
			"(or ufw allow in on %s proto tcp to %s port %d; ufw allow in on %s proto udp to %s port %d), or run with -host-firewall=insert",
			id, chains, v.ProxyPort, v.DNSPort, v.TAP, v.Host.Addr(), v.ProxyPort, v.TAP, v.Host.Addr(), v.DNSPort)
	default:
		return fmt.Errorf("tapnet: -host-firewall %q: want insert or refuse", n.Firewall)
	}
	if err := nft.InsertHoles(ctx, n.Host, v, chains); err != nil {
		return fmt.Errorf("tapnet: %s: %w", id, err)
	}
	log.Printf("tapnet: %s: %v drop input by policy: inserted accepts for %s to %s tcp %d and udp %d (comment %s)", id, chains, v.TAP, v.Host.Addr(), v.ProxyPort, v.DNSPort, nft.HoleComment(v))
	return nil
}
