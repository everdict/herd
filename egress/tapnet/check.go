package tapnet

import (
	"context"
	"fmt"
	"strings"

	"github.com/everdict/herd/egress/nft"
)

// Attached checks that VM id's TAP is up with a carrier: the hypervisor holds it open. A TAP nobody holds reads
// NO-CARRIER and "state DOWN" and carries nothing, so a VM booted on it has no network.
func (n *Network) Attached(ctx context.Context, id string) error {
	l, held, err := n.Leases.Held(id)
	if err != nil {
		return err
	}
	if !held {
		return fmt.Errorf("tapnet: %s holds no lease", id)
	}
	out, err := n.Host.Run(ctx, nft.Command{Name: "ip", Args: []string{"link", "show", "dev", l.TAP}})
	if err != nil {
		return fmt.Errorf("tapnet: %s: %w", id, err)
	}
	flags := string(out)
	if i, j := strings.Index(flags, "<"), strings.Index(flags, ">"); i >= 0 && j > i {
		flags = flags[i+1 : j]
	}
	for _, f := range strings.Split(flags, ",") {
		if f == "LOWER_UP" {
			return nil
		}
	}
	return fmt.Errorf("tapnet: %s: tap %s has no carrier (<%s>): no hypervisor holds it open, so the VM's NIC is not on it", id, l.TAP, flags)
}

// InputDrops lists the base chains on the input hook, in tables other than herd's own, whose policy is drop: such a
// chain (ufw's, firewalld's, an operator's) also sees every redirected flow from a VM's TAP, and nftables drops a
// packet any base chain drops, whatever herd's table accepted. ruleset is "nft list ruleset" output.
func InputDrops(ruleset string) []string {
	var out []string
	for _, c := range nft.InputDropChains(ruleset) {
		out = append(out, "table "+c.Family+" "+c.Table+" chain "+c.Name)
	}
	return out
}

// HostFirewall reads the host's ruleset and answers InputDrops of it.
func (n *Network) HostFirewall(ctx context.Context) ([]string, error) {
	out, err := n.Host.Run(ctx, nft.Command{Name: "nft", Args: []string{"list", "ruleset"}})
	if err != nil {
		return nil, err
	}
	return InputDrops(string(out)), nil
}
