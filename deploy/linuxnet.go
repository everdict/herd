package deploy

import (
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/everdict/herd/egress/nft"
	"github.com/everdict/herd/egress/tapnet"
	"github.com/everdict/herd/link"
)

// DefaultEgressBindings is where a Linux host's herd-egress reads its bindings (-bindings) and its herdlet writes them.
const DefaultEgressBindings = "/run/herd/egress-bindings.json"

// linuxNet is a Linux host's VM networks (ADR A-18): each VM leases a /30 of -net-pool with a TAP, gets its own
// egress table (egress/nft) redirecting everything to herd-egress's ports, and is bound to its actor in herd-egress's
// bindings file; host runs ip(8) and nft(8) (nft.ExecHost on a host, a fake in tests). The guest trusts the CA
// herd-egress wrote to -egress-ca.
func (h *Herdlet) linuxNet(host nft.Host) (*tapnet.Network, error) {
	pool := tapnet.DefaultPool
	if h.NetPool != "" {
		p, err := netip.ParsePrefix(h.NetPool)
		if err != nil {
			return nil, fmt.Errorf("-net-pool: %w", err)
		}
		pool = p
	}
	proxy, dns := h.EgressProxyPort, h.EgressDNSPort
	if proxy == 0 {
		proxy = 15001
	}
	if dns == 0 {
		dns = 15053
	}
	if proxy > 65535 || dns > 65535 {
		return nil, fmt.Errorf("-egress-proxy-port %d and -egress-dns-port %d must be ports", proxy, dns)
	}
	if h.HostFirewall != "" && h.HostFirewall != tapnet.FirewallInsert && h.HostFirewall != tapnet.FirewallRefuse {
		return nil, fmt.Errorf("-host-firewall %q: want insert or refuse", h.HostFirewall)
	}
	bindings := h.EgressBindings
	if bindings == "" {
		bindings = DefaultEgressBindings
	}
	ca := h.EgressCA
	if ca == "" {
		ca = filepath.Join(h.StateDir, "egress-ca.pem")
	}
	return &tapnet.Network{
		Leases:   &tapnet.Leases{Path: filepath.Join(h.StateDir, "net", "leases.json"), Pool: pool},
		Bindings: &tapnet.Bindings{Path: bindings},
		Host:     host, ProxyPort: uint16(proxy), DNSPort: uint16(dns), TAPOwner: -1,
		Firewall: h.HostFirewall,
		CA: func() ([]byte, error) {
			b, err := os.ReadFile(ca)
			if errors.Is(err, os.ErrNotExist) {
				// No herd-egress CA yet (none runs, or it has not started): the guest trusts its image's CAs only, and an
				// intercepted https rule fails its TLS check.
				log.Printf("herdlet: no herd-egress CA at %s (-egress-ca): guests boot without it", ca)
				return nil, nil
			}
			return b, err
		},
	}, nil
}

// assign is the herdlet's OnActor for a Linux host's VMs: the VM's link is bound to the actor, and then next, if
// set, is told too.
func assign(n *tapnet.Network, next func(string, link.ActorRef)) func(string, link.ActorRef) {
	return func(vm string, a link.ActorRef) {
		n.Assign(vm, tapnet.Actor{Space: a.Space, Name: a.Name, UID: a.UID})
		if next != nil {
			next(vm, a)
		}
	}
}
