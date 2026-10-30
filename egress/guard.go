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
		g.prefixes = append(g.prefixes, p)
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

// CheckAddr refuses an address in a denied range.
func (g *Guard) CheckAddr(a netip.Addr) error {
	if !a.IsValid() {
		return &RefusedError{HostSide: true, Reason: "invalid address"}
	}
	for _, x := range []netip.Addr{a, a.Unmap()} {
		for _, p := range g.prefixes {
			if p.Contains(x) {
				return &RefusedError{HostSide: true, Reason: fmt.Sprintf("address %s is host-side (%s)", a, p)}
			}
		}
	}
	return nil
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
