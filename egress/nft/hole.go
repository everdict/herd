package nft

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Chain is a base chain of another table, e.g. iptables-nft's (ufw's, Tailscale's) "ip filter INPUT".
type Chain struct {
	Family, Table, Name string
}

func (c Chain) String() string { return c.Family + " " + c.Table + " " + c.Name }

// InputDropChains are the base chains on the input hook whose policy is drop, in tables other than herd's own: such a
// chain also sees every flow a VM's table redirects to herd-egress, and nftables drops a packet any base chain drops,
// whatever herd's table accepted. ruleset is "nft list ruleset" output.
func InputDropChains(ruleset string) []Chain {
	var out []Chain
	var fam, table, chain string
	for _, line := range strings.Split(ruleset, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) >= 3 && f[0] == "table":
			fam, table, chain = f[1], f[2], ""
		case len(f) >= 2 && f[0] == "chain":
			chain = f[1]
		case chain != "" && strings.Contains(line, "hook input") && strings.Contains(line, "policy drop"):
			if !strings.HasPrefix(table, "herd_") {
				out = append(out, Chain{Family: fam, Table: table, Name: chain})
			}
		}
	}
	return out
}

// Holed reports whether a hole for a VM goes into c: VMs speak IPv4 only (herd's table drops everything else), so
// only ip and inet chains see their flows; an ip6 chain needs none.
func (c Chain) Holed() bool { return c.Family == "ip" || c.Family == "inet" }

// HoleComment marks the accepts herd inserts for v, so they are found again (also after a herdlet restart) and
// removed.
func HoleComment(v VM) string { return "herd-egress:" + v.Table() }

// HoleRules are the accepts herd inserts for v into another table's input chain that drops by policy: from the VM's
// TAP to herd-egress's two ports on the host's TAP address, nothing else. That herd-egress's ports are reached only
// by redirected flows stays enforced by herd's own table (its input chain accepts only ct status dnat), which every
// packet passes too; leaving ct out keeps the rule expressible in iptables-nft, whose tables ufw and Tailscale
// manage (iptables -S shows it as -i <tap> -d <host> -p tcp --dport <port> -m comment ... -j ACCEPT).
func HoleRules(v VM) []string {
	c := HoleComment(v)
	return []string{
		fmt.Sprintf(`iifname "%s" ip daddr %s tcp dport %d accept comment "%s"`, v.TAP, v.Host.Addr(), v.ProxyPort, c),
		fmt.Sprintf(`iifname "%s" ip daddr %s udp dport %d accept comment "%s"`, v.TAP, v.Host.Addr(), v.DNSPort, c),
	}
}

// Hole is one inserted accept: its chain and rule handle.
type Hole struct {
	Chain  Chain
	Handle string
}

var handleRE = regexp.MustCompile(`# handle (\d+)\s*$`)

// Holes finds v's accepts in the host's ruleset ("nft -a list ruleset").
func Holes(ctx context.Context, h Host, v VM) ([]Hole, error) {
	if err := v.validate(); err != nil {
		return nil, err
	}
	out, err := h.Run(ctx, Command{Name: "nft", Args: []string{"-a", "list", "ruleset"}})
	if err != nil {
		return nil, fmt.Errorf("nft: list ruleset: %w", err)
	}
	mark := `comment "` + HoleComment(v) + `"`
	var holes []Hole
	var c Chain
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) >= 3 && f[0] == "table":
			c = Chain{Family: f[1], Table: f[2]}
		case len(f) >= 2 && f[0] == "chain":
			c.Name = f[1]
		case strings.Contains(line, mark):
			if m := handleRE.FindStringSubmatch(line); m != nil {
				holes = append(holes, Hole{Chain: c, Handle: m[1]})
			}
		}
	}
	return holes, nil
}

// InsertHoles puts v's accepts at the top of every chain that needs them, in one nft transaction: all or none.
func InsertHoles(ctx context.Context, h Host, v VM, chains []Chain) error {
	if err := v.validate(); err != nil {
		return err
	}
	var b strings.Builder
	for _, c := range chains {
		if !c.Holed() {
			continue
		}
		for _, r := range HoleRules(v) {
			fmt.Fprintf(&b, "insert rule %s %s %s %s\n", c.Family, c.Table, c.Name, r)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	if _, err := h.Run(ctx, Command{Name: "nft", Args: []string{"-f", "-"}, Stdin: b.String()}); err != nil {
		return fmt.Errorf("nft: inserting %s's accepts: %w", v.TAP, err)
	}
	return nil
}

// RemoveHoles deletes v's accepts, wherever they are, in one nft transaction.
func RemoveHoles(ctx context.Context, h Host, v VM) error {
	holes, err := Holes(ctx, h, v)
	if err != nil || len(holes) == 0 {
		return err
	}
	var b strings.Builder
	for _, o := range holes {
		fmt.Fprintf(&b, "delete rule %s %s %s handle %s\n", o.Chain.Family, o.Chain.Table, o.Chain.Name, o.Handle)
	}
	if _, err := h.Run(ctx, Command{Name: "nft", Args: []string{"-f", "-"}, Stdin: b.String()}); err != nil {
		return fmt.Errorf("nft: removing %s's accepts: %w", v.TAP, err)
	}
	return nil
}
