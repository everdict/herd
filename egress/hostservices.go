package egress

import "net/netip"

// HostService is one declared host service (DEFAUL-1599).
type HostService struct {
	Name     string
	Port     uint16
	Target   netip.AddrPort
	Loopback bool
}

// LoadHostServices is a stub.
func (g *Gateway) LoadHostServices(data []byte) ([]error, error) { return nil, nil }

// HostServices is a stub.
func (g *Gateway) HostServices() []HostService { return nil }
