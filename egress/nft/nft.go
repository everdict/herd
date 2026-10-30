// Package nft renders and applies the Ubuntu half of herd-egress's plumbing (ADR 0001 A-16): each VM's TAP and an
// nftables table of its own that is default-deny for everything the TAP carries, except what is redirected to
// herd-egress. Every TCP connection the VM opens, to any address and port, is redirected to herd-egress's transparent
// listener (which reads the original port with SO_ORIGINAL_DST and decides by name); UDP to port 53 is redirected to
// herd-egress's synthetic DNS. Nothing from the TAP is forwarded or reaches another host socket, IPv6 and ICMP
// included, and a packet with a source address other than the VM's is dropped. Refusals are counted in the table's
// named counters.
//
// Applying needs root, ip(8) and nft(8); Host is the exec boundary, faked in tests. The real-host test
// (HERD_EGRESS_REAL=1) applies a rendered table in network namespaces and measures the refusals.
package nft

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"regexp"
	"strings"
	"text/template"
)

// ErrInvalid wraps every reason a VM's network cannot be rendered.
var ErrInvalid = errors.New("nft: invalid VM network")

// VM is one VM's network on an Ubuntu host.
type VM struct {
	// ID names the VM's table, herd_<ID>: lower-case letters, digits and underscores.
	ID string
	// TAP is the TAP device Cloud Hypervisor attaches (at most 15 characters).
	TAP string
	// Host is the host's address on the TAP with the link's prefix length, e.g. 10.88.0.1/30. herd-egress's
	// redirect targets land on it.
	Host netip.Prefix
	// VM is the VM's address on the link, e.g. 10.88.0.2.
	VM netip.Addr
	// ProxyPort is herd-egress's transparent TCP listener; DNSPort its synthetic DNS (UDP).
	ProxyPort uint16
	DNSPort   uint16
}

var (
	idRE  = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
	tapRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)
)

// Table is the VM's table name.
func (v VM) Table() string { return "herd_" + v.ID }

func (v VM) validate() error {
	switch {
	case !idRE.MatchString(v.ID):
		return fmt.Errorf("%w: id %q", ErrInvalid, v.ID)
	case !tapRE.MatchString(v.TAP):
		return fmt.Errorf("%w: tap %q", ErrInvalid, v.TAP)
	case !v.Host.IsValid() || !v.Host.Addr().Is4() || !v.VM.Is4():
		return fmt.Errorf("%w: host %v and vm %v must be IPv4", ErrInvalid, v.Host, v.VM)
	case !v.Host.Masked().Contains(v.VM) || v.Host.Addr() == v.VM:
		return fmt.Errorf("%w: vm %v is not another address on %v", ErrInvalid, v.VM, v.Host)
	case v.ProxyPort == 0 || v.DNSPort == 0:
		return fmt.Errorf("%w: proxy and dns ports are required", ErrInvalid)
	}
	return nil
}

var rulesetTmpl = template.Must(template.New("ruleset").Parse(`# herd-egress: VM {{.ID}} on {{.TAP}} ({{.VMAddr}}); generated, do not edit.
table inet {{.Table}} {
	counter refused_input {
	}
	counter refused_forward {
	}
	counter refused_spoofed {
	}
	counter refused_output {
	}

	chain screen {
		type filter hook prerouting priority raw; policy accept;
		iifname "{{.TAP}}" meta nfproto != ipv4 counter name "refused_input" drop
		iifname "{{.TAP}}" ip saddr != {{.VMAddr}} counter name "refused_spoofed" drop
	}

	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		iifname "{{.TAP}}" ip saddr {{.VMAddr}} meta l4proto tcp redirect to :{{.ProxyPort}}
		iifname "{{.TAP}}" ip saddr {{.VMAddr}} udp dport 53 redirect to :{{.DNSPort}}
	}

	chain input {
		type filter hook input priority filter; policy accept;
		iifname "{{.TAP}}" ct state established,related ct status dnat accept
		iifname "{{.TAP}}" ct state new ct status dnat ip daddr {{.HostAddr}} tcp dport {{.ProxyPort}} accept
		iifname "{{.TAP}}" ct state new ct status dnat ip daddr {{.HostAddr}} udp dport {{.DNSPort}} accept
		iifname "{{.TAP}}" counter name "refused_input" drop
	}

	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "{{.TAP}}" counter name "refused_forward" drop
		oifname "{{.TAP}}" counter name "refused_forward" drop
	}

	chain output {
		type filter hook output priority filter; policy accept;
		oifname "{{.TAP}}" ct state established,related accept
		oifname "{{.TAP}}" counter name "refused_output" drop
	}
}
`))

// Ruleset renders the VM's nftables table (input for nft -f).
func Ruleset(v VM) (string, error) {
	if err := v.validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	err := rulesetTmpl.Execute(&b, struct {
		ID, TAP, Table     string
		VMAddr, HostAddr   netip.Addr
		ProxyPort, DNSPort uint16
	}{v.ID, v.TAP, v.Table(), v.VM, v.Host.Addr(), v.ProxyPort, v.DNSPort})
	return b.String(), err
}

// Command is one host command.
type Command struct {
	Name  string
	Args  []string
	Stdin string
}

func (c Command) String() string {
	s := c.Name + " " + strings.Join(c.Args, " ")
	if c.Stdin != "" {
		s += " <<ruleset"
	}
	return s
}

// Plan is the commands that create the VM's TAP and load its table. A TAP that already exists is an error: herd
// owns its TAPs.
func Plan(v VM, tapOwnerUID int) ([]Command, error) {
	rs, err := Ruleset(v)
	if err != nil {
		return nil, err
	}
	add := []string{"tuntap", "add", "dev", v.TAP, "mode", "tap"}
	if tapOwnerUID >= 0 {
		add = append(add, "user", fmt.Sprint(tapOwnerUID))
	}
	return []Command{
		{Name: "ip", Args: add},
		{Name: "ip", Args: []string{"addr", "add", v.Host.String(), "dev", v.TAP}},
		// The table goes in before the link comes up, so the TAP never carries a packet unfiltered.
		{Name: "nft", Args: []string{"-f", "-"}, Stdin: rs},
		{Name: "ip", Args: []string{"link", "set", v.TAP, "up"}},
	}, nil
}

// TeardownPlan removes the VM's table and TAP.
func TeardownPlan(v VM) ([]Command, error) {
	if err := v.validate(); err != nil {
		return nil, err
	}
	return []Command{
		{Name: "ip", Args: []string{"link", "del", v.TAP}},
		{Name: "nft", Args: []string{"delete", "table", "inet", v.Table()}},
	}, nil
}

// Host runs host commands; ExecHost is the real one.
type Host interface {
	Run(ctx context.Context, c Command) ([]byte, error)
}

// ExecHost runs commands with os/exec, each prefixed by Prefix (e.g. ip netns exec <ns>).
type ExecHost struct {
	Prefix []string
}

// Run implements Host.
func (h ExecHost) Run(ctx context.Context, c Command) ([]byte, error) {
	argv := append(append(append([]string(nil), h.Prefix...), c.Name), c.Args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Apply creates the VM's TAP and table. On failure it removes what it made.
func Apply(ctx context.Context, h Host, v VM, tapOwnerUID int) error {
	plan, err := Plan(v, tapOwnerUID)
	if err != nil {
		return err
	}
	for i, c := range plan {
		if _, err := h.Run(ctx, c); err != nil {
			if i > 0 {
				_ = Remove(context.WithoutCancel(ctx), h, v)
			}
			return err
		}
	}
	return nil
}

// Remove deletes the VM's TAP and table; both are attempted and the first error is returned.
func Remove(ctx context.Context, h Host, v VM) error {
	plan, err := TeardownPlan(v)
	if err != nil {
		return err
	}
	var first error
	for _, c := range plan {
		if _, err := h.Run(ctx, c); err != nil && first == nil {
			first = err
		}
	}
	return first
}
