package egress

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/everdict/herd/egress/policy"
)

// HostService is one declared host service (DEFAUL-1599): a flow an actor's policy allows to Name:Port is dialed at
// exactly Target, with no lookup of Name and no guard check of Target, so a service on the node's private address
// (a herder's relay) is reachable on any network. Name is matched exactly, on that port only; any other name, a
// public one resolving to Target included, is decided and dialed as before.
type HostService struct {
	Name   string
	Port   uint16
	Target netip.AddrPort
	// Loopback is the file's explicit leave for a loopback Target (a PC's all-in-one); without it one is refused.
	Loopback bool
}

func (s HostService) String() string {
	return net.JoinHostPort(s.Name, strconv.Itoa(int(s.Port))) + " -> " + s.Target.String()
}

type serviceKey struct {
	name string
	port uint16
}

// declaredTarget marks a dial's context with the declared target it dials, which the default dialer's Control
// passes without the guard's address check.
type declaredTarget struct{}

// ParseHostServices parses a host services file: one service per line, `<name>:<port> <host>:<port>`, the target an
// address literal, and a third field `loopback` that a loopback target needs; blank lines and # comments are
// skipped. Any malformed line, or a name:port declared twice, fails the whole file.
func ParseHostServices(data []byte) ([]HostService, error) {
	var out []HostService
	seen := map[serviceKey]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		bad := func(format string, a ...any) error {
			return fmt.Errorf("host services line %d %q: %s", n, strings.TrimSpace(line), fmt.Sprintf(format, a...))
		}
		if len(f) > 3 || len(f) < 2 || (len(f) == 3 && f[2] != "loopback") {
			return nil, bad("want <name>:<port> <host>:<port> [loopback]")
		}
		host, ps, err := net.SplitHostPort(f[0])
		if err != nil {
			return nil, bad("%v", err)
		}
		port, err := strconv.ParseUint(ps, 10, 16)
		if err != nil || port == 0 {
			return nil, bad("bad port %q", ps)
		}
		name, ok := policy.NormalizeName(host)
		if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil || !ok {
			return nil, bad("%q is not a DNS name", host)
		}
		target, err := netip.ParseAddrPort(f[1])
		if err != nil || target.Port() == 0 {
			return nil, bad("the target must be an address and a port, e.g. 10.0.0.5:8787")
		}
		k := serviceKey{name, uint16(port)}
		if seen[k] {
			return nil, bad("%s:%d is declared twice", name, port)
		}
		seen[k] = true
		out = append(out, HostService{Name: name, Port: uint16(port), Target: netip.AddrPortFrom(target.Addr().Unmap(), target.Port()), Loopback: len(f) == 3})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// checkHostService refuses a target a declaration may not name: a cloud metadata endpoint, an unspecified address,
// a host address (GuardConfig.HostAddrs), the Control or router endpoint (Config.Endpoints), and loopback unless the
// line says loopback.
func (g *Gateway) checkHostService(s HostService) error {
	a := s.Target.Addr()
	switch {
	case slices.Contains(metadataAddrs, a):
		return fmt.Errorf("host service %s: the target is a metadata endpoint", s)
	case a.IsUnspecified():
		return fmt.Errorf("host service %s: the target is an unspecified address", s)
	case a.IsLoopback() && !s.Loopback:
		return fmt.Errorf("host service %s: the target is loopback, and the line does not say loopback", s)
	}
	for _, p := range g.cfg.Guard.host {
		if p.Contains(a) {
			return fmt.Errorf("host service %s: the target is a host address (%s)", s, p)
		}
	}
	for _, e := range g.cfg.Endpoints {
		if netip.AddrPortFrom(e.Addr().Unmap(), e.Port()) == s.Target {
			return fmt.Errorf("host service %s: the target is the Control or router endpoint", s)
		}
	}
	return nil
}

// LoadHostServices replaces the declared host services with the file data. A malformed file changes nothing and is
// the error; a line whose target is refused (checkHostService) is dropped alone, and dropped says why.
func (g *Gateway) LoadHostServices(data []byte) (dropped []error, err error) {
	svcs, err := ParseHostServices(data)
	if err != nil {
		return nil, err
	}
	m := map[serviceKey]HostService{}
	for _, s := range svcs {
		if err := g.checkHostService(s); err != nil {
			dropped = append(dropped, err)
			continue
		}
		m[serviceKey{s.Name, s.Port}] = s
	}
	g.services.Store(&m)
	return dropped, nil
}

// HostServices lists the declared host services in force, by name and port.
func (g *Gateway) HostServices() []HostService {
	p := g.services.Load()
	if p == nil {
		return nil
	}
	out := make([]HostService, 0, len(*p))
	for _, s := range *p {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b HostService) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return int(a.Port) - int(b.Port)
	})
	return out
}

// declared answers the target of a declared name:port.
func (g *Gateway) declared(name string, port int) (netip.AddrPort, bool) {
	p := g.services.Load()
	if p == nil || port < 1 || port > 65535 {
		return netip.AddrPort{}, false
	}
	s, ok := (*p)[serviceKey{strings.TrimSuffix(strings.ToLower(name), "."), uint16(port)}]
	return s.Target, ok
}

// dialDeclared dials a declared service's target and nothing else.
func (g *Gateway) dialDeclared(ctx context.Context, network string, target netip.AddrPort) (net.Conn, error) {
	return g.cfg.Dialer.DialContext(context.WithValue(ctx, declaredTarget{}, target), network, target.String())
}
