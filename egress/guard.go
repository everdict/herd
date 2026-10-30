package egress

import (
	"fmt"
	"net/netip"
	"strings"
)

// Guard refuses host-side targets before any rule of an actor's policy is consulted: the host itself, herd-api,
// loopback, private, link-local, shared, multicast and reserved ranges, and cloud metadata endpoints. It checks a
// destination twice: its name (an IP literal, "localhost", a metadata name, a host name) before the policy, and every
// address the name resolves to before a dial, so a listed name that resolves to a host-side address is refused too.
type Guard struct {
	prefixes []netip.Prefix
	// host are the host's own prefixes (GuardConfig.HostAddrs); a lab allowance never opens them.
	host []netip.Prefix
	// allow are the operator's lab allowances (GuardConfig.LabAllow): holes in prefixes.
	allow    []netip.Prefix
	names    map[string]bool
	suffixes []string
}

// DefaultDeniedPrefixes are refused on every host.
var DefaultDeniedPrefixes = []string{
	"0.0.0.0/8",          // this network
	"10.0.0.0/8",         // private
	"100.64.0.0/10",      // shared address space (CGNAT; also Alibaba's metadata 100.100.100.200)
	"127.0.0.0/8",        // loopback
	"169.254.0.0/16",     // link-local, incl. the 169.254.169.254 metadata endpoint
	"172.16.0.0/12",      // private
	"192.0.0.0/24",       // IETF protocol assignments
	"192.168.0.0/16",     // private
	"198.18.0.0/15",      // benchmarking
	"224.0.0.0/4",        // multicast
	"240.0.0.0/4",        // reserved, incl. broadcast
	"::/128",             // unspecified
	"::1/128",            // loopback
	"::ffff:0:0/96",      // IPv4-mapped (checked again as IPv4)
	"64:ff9b::/96",       // NAT64 (could reach a private IPv4)
	"64:ff9b:1::/48",     // local-use NAT64
	"100::/64",           // discard
	"fc00::/7",           // unique local, incl. AWS's fd00:ec2::254 metadata
	"fe80::/10",          // link-local
	"ff00::/8",           // multicast
	"2001:db8::/32",      // documentation
	"2002::/16",          // 6to4 (embeds an IPv4)
	"2001::/32",          // Teredo (embeds an IPv4)
	"fec0::/10",          // site-local (deprecated)
	"192.88.99.0/24",     // 6to4 relay anycast
	"255.255.255.255/32", // broadcast
}

// DefaultDeniedNames are refused on every host; a suffix entry starts with ".".
var DefaultDeniedNames = []string{
	"localhost", ".localhost",
	"metadata", "metadata.google.internal", ".internal",
	"instance-data", "instance-data.ec2.internal",
	".local", ".home.arpa",
}

// GuardConfig names what belongs to this host on top of the defaults.
type GuardConfig struct {
	// HostAddrs are the host's own addresses and herd-api's (single addresses or prefixes).
	HostAddrs []string
	// HostNames are the host's names and herd-api's; a name starting with "." denies its subdomains.
	HostNames []string
	// AllowPrefixes removes default prefixes (exactly as written in DefaultDeniedPrefixes). Only tests use it.
	AllowPrefixes []string
	// LabAllow are the operator's lab allowances (-egress-allow-prefix): each one opens exactly its prefix inside a
	// default denied prefix, e.g. 100.113.143.93/32 for a tailnet echo host, and nothing else (ParseLabAllow). A name
	// still has to be listed by the actor's policy, and an IP literal is still refused as a destination.
	LabAllow []string
}

// metadataAddrs are cloud metadata endpoints inside the default prefixes; no lab allowance may open one.
var metadataAddrs = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("100.100.100.200"), netip.MustParseAddr("fd00:ec2::254"),
}

// ParseLabAllow parses one lab allowance. It refuses what would open more than the operator wrote: a prefix with
// host bits set (100.113.143.93/10 reads as one address but means a /10), one that lies in no default denied
// prefix (there is nothing to open), one that contains a whole default prefix (100.64.0.0/10 would remove it), and
// one that contains a cloud metadata endpoint.
func ParseLabAllow(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("egress allow prefix %q: want an address with its prefix length, e.g. 100.113.143.93/32: %w", s, err)
	}
	if p.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("egress allow prefix %q: write an IPv4 prefix as IPv4", s)
	}
	if p != p.Masked() {
		return netip.Prefix{}, fmt.Errorf("egress allow prefix %q has host bits set: it would mean %s; write the lab host's address with /%d", s, p.Masked(), p.Addr().BitLen())
	}
	inside := false
	for _, d := range DefaultDeniedPrefixes {
		dp, err := netip.ParsePrefix(d)
		if err != nil {
			return netip.Prefix{}, err
		}
		if dp.Addr().Is4() != p.Addr().Is4() || !dp.Overlaps(p) {
			continue
		}
		if p.Bits() <= dp.Bits() {
			return netip.Prefix{}, fmt.Errorf("egress allow prefix %s would remove the whole default denied prefix %s; name the lab host's own address, e.g. a /32", p, dp)
		}
		inside = true
	}
	if !inside {
		return netip.Prefix{}, fmt.Errorf("egress allow prefix %s is in no default denied prefix: there is nothing to allow", p)
	}
	for _, m := range metadataAddrs {
		if p.Contains(m) {
			return netip.Prefix{}, fmt.Errorf("egress allow prefix %s contains the metadata endpoint %s", p, m)
		}
	}
	return p, nil
}

// NewGuard builds a Guard from the defaults plus cfg.
func NewGuard(cfg GuardConfig) (*Guard, error) {
	g := &Guard{names: map[string]bool{}}
	skip := map[string]bool{}
	for _, p := range cfg.AllowPrefixes {
		skip[p] = true
	}
	for _, s := range DefaultDeniedPrefixes {
		if skip[s] {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, err
		}
		g.prefixes = append(g.prefixes, p)
	}
	for _, s := range cfg.HostAddrs {
		p, err := parsePrefixOrAddr(s)
		if err != nil {
			return nil, fmt.Errorf("guard: host address %q: %w", s, err)
		}
		g.host = append(g.host, p)
	}
	for _, s := range cfg.LabAllow {
		p, err := ParseLabAllow(s)
		if err != nil {
			return nil, fmt.Errorf("guard: %w", err)
		}
		g.allow = append(g.allow, p)
	}
	for _, n := range append(append([]string(nil), DefaultDeniedNames...), cfg.HostNames...) {
		n = strings.TrimSuffix(strings.ToLower(n), ".")
		if strings.HasPrefix(n, ".") {
			g.suffixes = append(g.suffixes, n)
		} else if n != "" {
			g.names[n] = true
			g.suffixes = append(g.suffixes, "."+n)
		}
	}
	return g, nil
}

func parsePrefixOrAddr(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()), nil
}

// CheckName refuses a destination name that is host-side: an IP literal (always — rules name hosts, and an address
// in a Host header or SNI is how an actor would aim at the host), a denied name, or a subdomain of one.
func (g *Guard) CheckName(name string) error {
	n := strings.TrimSuffix(strings.ToLower(strings.Trim(name, "[]")), ".")
	if a, err := netip.ParseAddr(n); err == nil {
		if err := g.CheckAddr(a); err != nil {
			return err
		}
		return &RefusedError{HostSide: true, Reason: fmt.Sprintf("destination %s is an address, not a name", n)}
	}
	if g.names[n] {
		return &RefusedError{HostSide: true, Reason: fmt.Sprintf("destination %s is host-side", n)}
	}
	for _, s := range g.suffixes {
		if strings.HasSuffix(n, s) {
			return &RefusedError{HostSide: true, Reason: fmt.Sprintf("destination %s is host-side (%s)", n, s)}
		}
	}
	return nil
}

// LabAllow returns the lab allowances the guard was built with.
func (g *Guard) LabAllow() []netip.Prefix { return append([]netip.Prefix(nil), g.allow...) }

// CheckAddr refuses an address in a denied range: a host prefix always, a default prefix unless a lab allowance
// contains the address.
func (g *Guard) CheckAddr(a netip.Addr) error {
	if !a.IsValid() {
		return &RefusedError{HostSide: true, Reason: "invalid address"}
	}
	for _, x := range []netip.Addr{a, a.Unmap()} {
		for _, p := range g.host {
			if p.Contains(x) {
				return &RefusedError{HostSide: true, Reason: fmt.Sprintf("address %s is host-side (%s)", a, p)}
			}
		}
		if g.allowed(x) {
			continue
		}
		for _, p := range g.prefixes {
			if p.Contains(x) {
				return &RefusedError{HostSide: true, Reason: fmt.Sprintf("address %s is host-side (%s)", a, p)}
			}
		}
	}
	return nil
}

func (g *Guard) allowed(a netip.Addr) bool {
	for _, p := range g.allow {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// RefusedError is a refusal; HostSide marks the guard's.
type RefusedError struct {
	HostSide bool
	Reason   string
}

func (e *RefusedError) Error() string {
	if e.HostSide {
		return "herd-egress: refused host-side target: " + e.Reason
	}
	return "herd-egress: refused: " + e.Reason
}
