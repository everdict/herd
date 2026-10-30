package initd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Netlinker is the network-configuration boundary: the linux implementation (KernelNetlinker) talks to the kernel
// with ioctls and rtnetlink, tests record.
type Netlinker interface {
	// SetAddr sets iface's IPv4 address and netmask from p.
	SetAddr(iface string, p netip.Prefix) error
	// Up sets IFF_UP on iface.
	Up(iface string) error
	// AddDefaultRoute adds the IPv4 default route via gw out of iface; one already there is not an error.
	AddDefaultRoute(iface string, gw netip.Addr) error
}

// SysClassNet is where sysfs lists the network interfaces.
const SysClassNet = "/sys/class/net"

// ConfigureNetwork brings lo up, then gives n's interface (found in sysNet, a /sys/class/net, by MAC, or the only
// non-loopback interface when n has none) its address, brings it up and routes everything via n's gateway.
func ConfigureNetwork(nl Netlinker, sysNet string, n *Network) error {
	if nl == nil {
		return fmt.Errorf("initd: network: the plan has a network and herd-guest no way to configure it")
	}
	if err := n.Validate(); err != nil {
		return err
	}
	pfx, err := netip.ParsePrefix(n.Address)
	if err != nil {
		return fmt.Errorf("initd: network: %w", err)
	}
	gw, err := netip.ParseAddr(n.Gateway)
	if err != nil {
		return fmt.Errorf("initd: network: %w", err)
	}
	iface, err := findInterface(sysNet, n.MAC)
	if err != nil {
		return err
	}
	if err := nl.Up("lo"); err != nil {
		return fmt.Errorf("initd: network: bringing lo up: %w", err)
	}
	if err := nl.SetAddr(iface, pfx); err != nil {
		return fmt.Errorf("initd: network: %s: setting address %s: %w", iface, pfx, err)
	}
	if err := nl.Up(iface); err != nil {
		return fmt.Errorf("initd: network: bringing %s up: %w", iface, err)
	}
	if err := nl.AddDefaultRoute(iface, gw); err != nil {
		return fmt.Errorf("initd: network: default route via %s on %s: %w", gw, iface, err)
	}
	log.Printf("herd-guest: network: %s (mac %s) is %s, up, default route via %s", iface, macOf(sysNet, iface), pfx, gw)
	return nil
}

// ethernet reports whether sysfs lists iface as an Ethernet device backed by a device (the VM's virtio-net NIC): its
// type is ARPHRD_ETHER (1) and it has a device link. lo (772), sit0 (776) and other tunnels or virtual devices are not.
func ethernet(sysNet, iface string) bool {
	b, err := os.ReadFile(filepath.Join(sysNet, iface, "type"))
	if err != nil || strings.TrimSpace(string(b)) != "1" {
		return false
	}
	_, err = os.Stat(filepath.Join(sysNet, iface, "device"))
	return err == nil
}

// macOf is iface's MAC as sysfs answers it, for the log.
func macOf(sysNet, iface string) string {
	b, err := os.ReadFile(filepath.Join(sysNet, iface, "address"))
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}

// ProbeDNS asks server (an address, port 53) for the A record of name, waiting at most d, and answers the first
// address in the answer. herd-guest runs it once its network is up and logs the result to the console: on a host
// whose herd-egress DNS cannot be reached from the VM, the console says so before any container asks.
func ProbeDNS(server, name string, d time.Duration) (netip.Addr, error) {
	return probeDNS(net.JoinHostPort(server, "53"), name, d)
}

func probeDNS(server, name string, d time.Duration) (netip.Addr, error) {
	q := []byte{0x68, 0x64, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if l == "" || len(l) > 63 {
			return netip.Addr{}, fmt.Errorf("dns probe: bad name %q", name)
		}
		q = append(append(q, byte(len(l))), l...)
	}
	q = append(q, 0, 0, 1, 0, 1)
	c, err := net.DialTimeout("udp4", server, d)
	if err != nil {
		return netip.Addr{}, err
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(d)); err != nil {
		return netip.Addr{}, err
	}
	if _, err := c.Write(q); err != nil {
		return netip.Addr{}, err
	}
	b := make([]byte, 512)
	n, err := c.Read(b)
	if err != nil {
		return netip.Addr{}, err
	}
	b = b[:n]
	if n < 12 || b[0] != q[0] || b[1] != q[1] {
		return netip.Addr{}, fmt.Errorf("dns probe: answer of %d bytes is not this query's", n)
	}
	if rcode := b[3] & 0xf; rcode != 0 {
		return netip.Addr{}, fmt.Errorf("dns probe: rcode %d", rcode)
	}
	if binary.BigEndian.Uint16(b[6:8]) == 0 || n < len(q)+16 {
		return netip.Addr{}, errors.New("dns probe: no answer record")
	}
	return netip.AddrFrom4([4]byte(b[n-4:])), nil
}

// findInterface returns the name of sysNet's interface whose address is mac, or with mac empty the only interface
// besides lo.
func findInterface(sysNet, mac string) (string, error) {
	var want net.HardwareAddr
	if mac != "" {
		var err error
		if want, err = net.ParseMAC(mac); err != nil {
			return "", fmt.Errorf("initd: network: mac %q: %w", mac, err)
		}
	}
	ents, err := os.ReadDir(sysNet)
	if err != nil {
		return "", fmt.Errorf("initd: network: listing interfaces: %w", err)
	}
	var found []string
	for _, e := range ents {
		if !ethernet(sysNet, e.Name()) {
			continue // lo, and the tunnels a kernel makes on its own (sit0, tunl0, ip6tnl0): only a NIC carries the link
		}
		if want == nil {
			found = append(found, e.Name())
			continue
		}
		b, err := os.ReadFile(filepath.Join(sysNet, e.Name(), "address"))
		if err != nil {
			continue
		}
		if hw, err := net.ParseMAC(strings.TrimSpace(string(b))); err == nil && hw.String() == want.String() {
			found = append(found, e.Name())
		}
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case want != nil && len(found) == 0:
		return "", fmt.Errorf("initd: network: no interface has mac %s", want)
	case want != nil:
		return "", fmt.Errorf("initd: network: interfaces %v all have mac %s", found, want)
	case len(found) == 0:
		return "", fmt.Errorf("initd: network: no Ethernet interface")
	default:
		return "", fmt.Errorf("initd: network: no mac given and %d Ethernet interfaces: %v", len(found), found)
	}
}

// The linux values of the rtnetlink constants routeRequest uses, spelled out so it builds and is tested anywhere.
const (
	rtmNewRoute   = 24
	nlmsgError    = 2
	nlmFRequest   = 0x1
	nlmFAck       = 0x4
	nlmFExcl      = 0x200
	nlmFCreate    = 0x400
	afInet        = 2
	rtTableMain   = 254
	rtprotBoot    = 3
	rtScopeUniver = 0
	rtnUnicast    = 1
	rtaOIF        = 4
	rtaGateway    = 5
	nlmsgHdrLen   = 16
	rtmsgLen      = 12
)

// routeRequest is the RTM_NEWROUTE message adding the IPv4 default route via gw out of interface index oif: an
// nlmsghdr, an rtmsg (AF_INET, dst_len 0, main table, RTPROT_BOOT, universe scope, unicast) and the RTA_GATEWAY
// and RTA_OIF attributes, in host byte order as netlink wants.
func routeRequest(seq uint32, gw netip.Addr, oif uint32) []byte {
	const n = nlmsgHdrLen + rtmsgLen + 8 + 8
	b := make([]byte, n)
	e := binary.NativeEndian
	e.PutUint32(b[0:], n)
	e.PutUint16(b[4:], rtmNewRoute)
	e.PutUint16(b[6:], nlmFRequest|nlmFAck|nlmFExcl|nlmFCreate)
	e.PutUint32(b[8:], seq)
	// pid 0: the kernel. rtmsg:
	r := b[nlmsgHdrLen:]
	r[0], r[1], r[2], r[3] = afInet, 0, 0, 0 // family, dst_len, src_len, tos
	r[4], r[5], r[6], r[7] = rtTableMain, rtprotBoot, rtScopeUniver, rtnUnicast
	// r[8:12]: rtm_flags 0.
	a := r[rtmsgLen:]
	e.PutUint16(a[0:], 8)
	e.PutUint16(a[2:], rtaGateway)
	g := gw.As4()
	copy(a[4:8], g[:])
	e.PutUint16(a[8:], 8)
	e.PutUint16(a[10:], rtaOIF)
	e.PutUint32(a[12:], oif)
	return b
}

// parseAck reads the kernel's answer to request seq: an NLMSG_ERROR whose error is 0 (an ACK) or a negative errno.
func parseAck(b []byte, seq uint32) error {
	e := binary.NativeEndian
	for len(b) >= nlmsgHdrLen {
		l := e.Uint32(b[0:])
		if l < nlmsgHdrLen || int(l) > len(b) {
			return errors.New("netlink: truncated message")
		}
		if e.Uint16(b[4:]) == nlmsgError && e.Uint32(b[8:]) == seq {
			if l < nlmsgHdrLen+4 {
				return errors.New("netlink: truncated error message")
			}
			if errno := int32(e.Uint32(b[nlmsgHdrLen:])); errno != 0 {
				return syscall.Errno(-errno)
			}
			return nil
		}
		if next := int(l+3) &^ 3; next < len(b) {
			b = b[next:]
		} else {
			break
		}
	}
	return errors.New("netlink: no acknowledgement")
}
