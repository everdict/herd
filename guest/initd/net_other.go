//go:build !linux

package initd

import (
	"fmt"
	"net/netip"
	"runtime"
)

// KernelNetlinker is available on linux only, where herd-guest boots.
type KernelNetlinker struct{}

func (KernelNetlinker) SetAddr(string, netip.Prefix) error { return unsupported() }
func (KernelNetlinker) Up(string) error                    { return unsupported() }

func (KernelNetlinker) AddDefaultRoute(string, netip.Addr) error { return unsupported() }

func unsupported() error {
	return fmt.Errorf("initd: network configuration is not available on %s", runtime.GOOS)
}
