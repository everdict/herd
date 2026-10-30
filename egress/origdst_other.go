//go:build !linux

package egress

import (
	"errors"
	"net"
	"net/netip"
)

// SockOriginalDst is Linux-only (nftables redirect); macOS hosts use a Stack, whose flows carry their destination.
func SockOriginalDst(net.Conn) (netip.AddrPort, error) {
	return netip.AddrPort{}, errors.New("egress: SO_ORIGINAL_DST is available on Linux only")
}
