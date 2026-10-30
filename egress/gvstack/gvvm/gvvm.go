// Package gvvm is a guest's side of a gvstack link for tests: a gVisor netstack with the VM's address and a default
// route via the gateway, speaking Ethernet frames over the file a gvstack.Factory hands the hypervisor — what the
// Linux guest's virtio-net device does under Virtualization.framework.
package gvvm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// MAC is the guest's Ethernet address.
const MAC = "5a:94:ef:e4:0c:ee"

// VM is the guest's network.
type VM struct {
	ns     *stack.Stack
	conn   net.Conn
	cancel context.CancelFunc
}

// New attaches a guest with address addr and default gateway gw to the VM's end of the link.
func New(file *os.File, addr, gw netip.Addr) (*VM, error) {
	conn, err := net.FileConn(file)
	if err != nil {
		return nil, err
	}
	link, err := tap.NewLinkEndpoint(false, 1500, MAC, addr.String(), nil)
	if err != nil {
		return nil, err
	}
	sw := tap.NewSwitch(false, 1500)
	link.Connect(sw)
	sw.Connect(link)
	ns := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	if terr := ns.CreateNIC(1, link); terr != nil {
		return nil, fmt.Errorf("nic: %s", terr)
	}
	a := tcpip.AddrFrom4(addr.As4())
	if terr := ns.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: a, PrefixLen: 24}}, stack.AddressProperties{}); terr != nil {
		return nil, fmt.Errorf("address: %s", terr)
	}
	sub, err := tcpip.NewSubnet(tcpip.AddrFrom4(netip.PrefixFrom(addr, 24).Masked().Addr().As4()), tcpip.MaskFromBytes([]byte{255, 255, 255, 0}))
	if err != nil {
		return nil, err
	}
	ns.SetRouteTable([]tcpip.Route{
		{Destination: sub, NIC: 1},
		{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4(gw.As4()), NIC: 1},
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = sw.Accept(ctx, conn, types.VfkitProtocol) }()
	return &VM{ns: ns, conn: conn, cancel: cancel}, nil
}

// DialTCP opens a TCP connection from the guest to dst.
func (v *VM) DialTCP(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
	return gonet.DialContextTCP(ctx, v.ns, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(dst.Addr().As4()), Port: dst.Port()}, ipv4.ProtocolNumber)
}

// ErrNoAnswer is what Exchange returns when no datagram came back within its wait.
var ErrNoAnswer = errors.New("gvvm: no answer")

// Exchange sends one UDP datagram from the guest to dst and waits up to wait for one answer.
func (v *VM) Exchange(dst netip.AddrPort, payload []byte, wait time.Duration) ([]byte, error) {
	c, err := gonet.DialUDP(v.ns, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(dst.Addr().As4()), Port: dst.Port()}, ipv4.ProtocolNumber)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if _, err := c.Write(payload); err != nil {
		return nil, err
	}
	_ = c.SetReadDeadline(time.Now().Add(wait))
	b := make([]byte, 65535)
	n, err := c.Read(b)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, ErrNoAnswer
		}
		return nil, err
	}
	return b[:n], nil
}

// SendFrame writes one raw Ethernet frame onto the link, as a guest crafting its own packets would.
func (v *VM) SendFrame(frame []byte) error {
	_, err := v.conn.Write(frame)
	return err
}

// Close detaches the guest.
func (v *VM) Close() {
	v.cancel()
	_ = v.conn.Close()
	v.ns.Close()
}
