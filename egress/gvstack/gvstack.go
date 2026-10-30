// Package gvstack is herd-egress's userspace stack for macOS hosts (ADR 0001 A-18, R7): gvisor-tap-vsock's
// Ethernet switch and link endpoint in front of a gVisor netstack that has exactly two ways out, both into
// herd-egress:
//   - every TCP connection the VM opens, to any address and port, is terminated in the netstack and handed to
//     herd-egress as an egress.Flow (Accept);
//   - a UDP datagram to the gateway's port 53 is answered by the configured DNS function (herd-egress's
//     SyntheticDNS).
//
// gvisor-tap-vsock's own services are not used: its forwarder is a NAT that dials whatever the VM names from the
// host, which is the bypass herd-egress exists to close. Every other frame (UDP elsewhere, ICMP, IPv6, IPv4
// fragments, packets whose source is not the VM's address) is dropped before the netstack sees it, and counted.
//
// The VM's side is one end of a datagram socket pair carrying one Ethernet frame per datagram — what
// Virtualization.framework's VZFileHandleNetworkDeviceAttachment reads and writes (gvisor-tap-vsock's vfkit
// protocol). Nothing here needs macOS: the package builds and is tested on every unix.
package gvstack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/everdict/herd/egress"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// DefaultMTU is the VM link's MTU when Factory.MTU is zero.
const DefaultMTU = 1500

// GatewayMAC is the gateway's Ethernet address on every VM's link.
const GatewayMAC = "5a:94:ef:e4:0c:dd"

const nic tcpip.NICID = 1

// Factory makes one Stack per VM; it implements egress.StackFactory.
type Factory struct {
	// MTU of the VM's link; zero means DefaultMTU.
	MTU int
}

var _ egress.StackFactory = Factory{}

// NewStack makes a datagram socket pair, serves one end with a new Stack and returns the other end: the file
// vmm/vz attaches as the VM's network device.
func (f Factory) NewStack(_ egress.Actor, cfg egress.StackConfig) (egress.Stack, *os.File, error) {
	host, vm, err := socketPair()
	if err != nil {
		return nil, nil, fmt.Errorf("gvstack: %w", err)
	}
	s, err := New(host, cfg, f.MTU)
	if err != nil {
		_ = host.Close()
		_ = vm.Close()
		return nil, nil, err
	}
	return s, vm, nil
}

// Stack is one VM's userspace network.
type Stack struct {
	cfg  egress.StackConfig
	conn net.Conn
	ns   *stack.Stack
	dns  *gonet.UDPConn

	flows   chan egress.Flow
	done    chan struct{}
	once    sync.Once
	dropped atomic.Int64
	wg      sync.WaitGroup
}

var _ egress.Stack = (*Stack)(nil)

// New serves conn — a datagram connection carrying one Ethernet frame per datagram — as the VM's link. mtu zero
// means DefaultMTU.
func New(conn net.Conn, cfg egress.StackConfig, mtu int) (*Stack, error) {
	if !cfg.Gateway.Is4() || !cfg.VM.Is4() || cfg.DNS == nil {
		return nil, errors.New("gvstack: StackConfig needs IPv4 Gateway and VM addresses and DNS")
	}
	if mtu <= 0 {
		mtu = DefaultMTU
	}
	st := &Stack{cfg: cfg, conn: conn, flows: make(chan egress.Flow), done: make(chan struct{})}

	link, err := tap.NewLinkEndpoint(false, uint32(mtu), GatewayMAC, cfg.Gateway.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("gvstack: link: %w", err)
	}
	sw := tap.NewSwitch(false, mtu)
	link.Connect(sw)
	sw.Connect(link)

	ns := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	st.ns = ns
	if terr := ns.CreateNIC(nic, &filteredLink{LinkEndpoint: link, st: st}); terr != nil {
		return nil, fmt.Errorf("gvstack: nic: %s", terr)
	}
	gw := tcpip.AddrFrom4(cfg.Gateway.As4())
	if terr := ns.AddProtocolAddress(nic, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: gw.WithPrefix()}, stack.AddressProperties{}); terr != nil {
		ns.Close()
		return nil, fmt.Errorf("gvstack: address: %s", terr)
	}
	// Promiscuous: the netstack terminates TCP to every address, not only the gateway's. Spoofing: it answers as
	// that address.
	ns.SetPromiscuousMode(nic, true)
	ns.SetSpoofing(nic, true)
	ns.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nic}})

	// The only TCP handler: hand the connection to herd-egress. There is no dial here.
	fwd := tcp.NewForwarder(ns, 0, 64, st.forward)
	ns.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)

	dns, err := gonet.DialUDP(ns, &tcpip.FullAddress{NIC: nic, Addr: gw, Port: 53}, nil, ipv4.ProtocolNumber)
	if err != nil {
		ns.Close()
		return nil, fmt.Errorf("gvstack: dns: %w", err)
	}
	st.dns = dns

	ctx, cancel := context.WithCancel(context.Background())
	st.wg.Add(2)
	go func() {
		defer st.wg.Done()
		defer cancel()
		// Returns when conn is closed (Close) or the VM's end goes away.
		_ = sw.Accept(ctx, SwitchConn(conn), types.VfkitProtocol)
	}()
	go func() {
		defer st.wg.Done()
		st.serveDNS()
	}()
	go func() {
		<-ctx.Done()
		_ = st.Close()
	}()
	return st, nil
}

// SwitchConn is conn as a gvisor-tap-vsock switch must be handed it: its writes never fail. On a failed write the
// switch drops the connection from its own table while its reader may still be handling a frame it had read; that
// frame puts the sender's MAC back into the switch's address table with no connection behind it, and the next frame
// to that MAC dereferences nil in tap.(*Switch).txBuf (PLT-197). A write that fails closes conn instead and counts
// the frame as sent, so the reader fails next and the switch's Accept removes the connection and its addresses
// together. ENOBUFS is passed through: the switch retries that frame.
func SwitchConn(conn net.Conn) net.Conn { return switchConn{conn} }

type switchConn struct{ net.Conn }

func (c switchConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if err == nil || errors.Is(err, syscall.ENOBUFS) {
		return n, err
	}
	_ = c.Conn.Close()
	return len(b), nil
}

// forward runs for each TCP SYN the VM sends: complete the handshake and hand the connection to Accept.
func (st *Stack) forward(r *tcp.ForwarderRequest) {
	id := r.ID()
	select {
	case <-st.done:
		r.Complete(true)
		return
	default:
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		r.Complete(true)
		return
	}
	r.Complete(false)
	c := gonet.NewTCPConn(&wq, ep)
	dst := netip.AddrPortFrom(netip.AddrFrom4(id.LocalAddress.As4()), id.LocalPort)
	select {
	case st.flows <- egress.Flow{Conn: c, Dst: dst}:
	case <-st.done:
		_ = c.Close()
	}
}

func (st *Stack) serveDNS() {
	buf := make([]byte, 65535)
	for {
		n, from, err := st.dns.ReadFrom(buf)
		if err != nil {
			return
		}
		reply, err := st.cfg.DNS(buf[:n])
		if err != nil {
			continue
		}
		_, _ = st.dns.WriteTo(reply, from)
	}
}

// Accept implements egress.Stack.
func (st *Stack) Accept(ctx context.Context) (egress.Flow, error) {
	select {
	case f := <-st.flows:
		return f, nil
	case <-st.done:
		return egress.Flow{}, net.ErrClosed
	case <-ctx.Done():
		return egress.Flow{}, ctx.Err()
	}
}

// Dropped implements egress.Stack.
func (st *Stack) Dropped() int64 { return st.dropped.Load() }

// Close implements egress.Stack: the link, the DNS endpoint and the netstack go away.
func (st *Stack) Close() error {
	st.once.Do(func() {
		close(st.done)
		_ = st.conn.Close()
		_ = st.dns.Close()
		st.ns.Close()
	})
	return nil
}

// filteredLink is gvisor-tap-vsock's link endpoint with a filter between it and the netstack.
type filteredLink struct {
	*tap.LinkEndpoint
	st *Stack
}

func (l *filteredLink) Attach(d stack.NetworkDispatcher) {
	if d == nil {
		l.LinkEndpoint.Attach(nil)
		return
	}
	l.LinkEndpoint.Attach(&filter{NetworkDispatcher: d, st: l.st})
}

// filter passes ARP, TCP from the VM's own address, and UDP from it to the gateway's port 53. Everything else is
// dropped and counted.
type filter struct {
	stack.NetworkDispatcher
	st *Stack
}

func (f *filter) DeliverNetworkPacket(p tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	if !f.st.admit(p, pkt) {
		f.st.dropped.Add(1)
		return
	}
	f.NetworkDispatcher.DeliverNetworkPacket(p, pkt)
}

func (st *Stack) admit(p tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) bool {
	switch p {
	case header.ARPProtocolNumber:
		return true
	case header.IPv4ProtocolNumber:
	default:
		return false
	}
	v := pkt.ToView()
	defer v.Release()
	b := v.AsSlice()
	ip := header.IPv4(b)
	if !ip.IsValid(len(b)) || ip.More() || ip.FragmentOffset() != 0 {
		return false
	}
	if ip.SourceAddress() != tcpip.AddrFrom4(st.cfg.VM.As4()) {
		return false
	}
	switch ip.TransportProtocol() {
	case header.TCPProtocolNumber:
		return true
	case header.UDPProtocolNumber:
		u := header.UDP(ip.Payload())
		return len(u) >= header.UDPMinimumSize && u.DestinationPort() == 53 &&
			ip.DestinationAddress() == tcpip.AddrFrom4(st.cfg.Gateway.As4())
	}
	return false
}
