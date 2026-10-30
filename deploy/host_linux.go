package deploy

import (
	"context"
	"log"
	"path/filepath"
	"strings"

	"github.com/everdict/herd/egress/nft"
	"github.com/everdict/herd/egress/tapnet"
	"github.com/everdict/herd/vmm/cloudhypervisor"
)

// newHost is Cloud Hypervisor (ADR R-D8): one cloud-hypervisor process per VM, virtiofsd for the share, hybrid vsock,
// and each VM's NIC on a TAP of its own whose egress table sends everything to herd-egress (A-18, linuxNet).
func (h *Herdlet) newHost(ctx context.Context) (Backend, error) {
	v := cloudhypervisor.New(cloudhypervisor.Config{
		Binary: h.CHBinary, Virtiofsd: h.Virtiofsd, VirtiofsdArgs: strings.Fields(h.VirtiofsdArgs),
		StateDir: filepath.Join(h.StateDir, "vms"), SnapshotDir: filepath.Join(h.StateDir, "vm-snapshots"),
	})
	n, err := h.linuxNet(nft.ExecHost{})
	if err != nil {
		return Backend{}, err
	}
	// Another table's input chain that drops by policy (ufw, Tailscale, firewalld) also sees every flow a VM's table
	// redirects to herd-egress and wins over herd's accept: each VM's link gets accepts there (-host-firewall=insert),
	// or no VM starts until the operator lets the flows in (refuse).
	if drops, err := n.HostFirewall(ctx); err != nil {
		log.Printf("herdlet: cannot read the host's nftables ruleset: %v", err)
	} else if len(drops) > 0 && n.Firewall == tapnet.FirewallRefuse {
		log.Printf("herdlet: warning: %v drop input by policy and -host-firewall=refuse: VMs start only once those chains accept input from the herd TAPs to herd-egress (tcp %d, udp %d), e.g. iptables -I INPUT -i herd+ -p tcp --dport %d -j ACCEPT; iptables -I INPUT -i herd+ -p udp --dport %d -j ACCEPT", drops, n.ProxyPort, n.DNSPort, n.ProxyPort, n.DNSPort)
	} else if len(drops) > 0 {
		log.Printf("herdlet: %v drop input by policy: each VM's TAP gets accepts there for exactly its flows to herd-egress (tcp %d, udp %d), removed with the VM (-host-firewall=insert)", drops, n.ProxyPort, n.DNSPort)
	}
	return Backend{VMM: v, Guests: v, Version: "cloud-hypervisor v54.0", Network: n, OnActor: assign(n, nil)}, nil
}

// vzBuilt reports whether this build can drive Virtualization.framework.
func vzBuilt() bool { return false }
