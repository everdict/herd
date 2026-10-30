//go:build linux

package initd

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"golang.org/x/sys/unix"
)

// KernelNetlinker configures interfaces with SIOCSIF* ioctls and adds the route over rtnetlink.
type KernelNetlinker struct{}

// ifreqDo calls f with an AF_INET datagram socket and an ifreq naming iface, for f's ioctls.
func ifreqDo(iface string, f func(fd int, ifr *unix.Ifreq) error) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(iface)
	if err != nil {
		return err
	}
	return f(fd, ifr)
}

func (KernelNetlinker) SetAddr(iface string, p netip.Prefix) error {
	if !p.Addr().Is4() {
		return fmt.Errorf("%s is not IPv4", p)
	}
	return ifreqDo(iface, func(fd int, ifr *unix.Ifreq) error {
		if err := ifr.SetInet4Addr(p.Addr().AsSlice()); err != nil {
			return err
		}
		if err := unix.IoctlIfreq(fd, unix.SIOCSIFADDR, ifr); err != nil {
			return fmt.Errorf("SIOCSIFADDR: %w", err)
		}
		if err := ifr.SetInet4Addr(net.CIDRMask(p.Bits(), 32)); err != nil {
			return err
		}
		if err := unix.IoctlIfreq(fd, unix.SIOCSIFNETMASK, ifr); err != nil {
			return fmt.Errorf("SIOCSIFNETMASK: %w", err)
		}
		return nil
	})
}

func (KernelNetlinker) Up(iface string) error {
	return ifreqDo(iface, func(fd int, ifr *unix.Ifreq) error {
		if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
			return fmt.Errorf("SIOCGIFFLAGS: %w", err)
		}
		ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
		if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
			return fmt.Errorf("SIOCSIFFLAGS: %w", err)
		}
		return nil
	})
}

func (KernelNetlinker) AddDefaultRoute(iface string, gw netip.Addr) error {
	if !gw.Is4() {
		return fmt.Errorf("%s is not IPv4", gw)
	}
	var oif uint32
	if err := ifreqDo(iface, func(fd int, ifr *unix.Ifreq) error {
		if err := unix.IoctlIfreq(fd, unix.SIOCGIFINDEX, ifr); err != nil {
			return fmt.Errorf("SIOCGIFINDEX: %w", err)
		}
		oif = ifr.Uint32()
		return nil
	}); err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	kernel := &unix.SockaddrNetlink{Family: unix.AF_NETLINK}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	const seq = 1
	if err := unix.Sendto(fd, routeRequest(seq, gw, oif), 0, kernel); err != nil {
		return fmt.Errorf("RTM_NEWROUTE: %w", err)
	}
	buf := make([]byte, 8192)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return fmt.Errorf("RTM_NEWROUTE: %w", err)
	}
	if err := parseAck(buf[:n], seq); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("RTM_NEWROUTE: %w", err)
	}
	return nil
}
