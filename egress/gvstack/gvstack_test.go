package gvstack_test

import (
	"context"
	"io"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/egress/gvstack"
	"github.com/everdict/herd/egress/gvstack/gvvm"
	"github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

var (
	gwAddr = netip.MustParseAddr("192.168.127.1")
	vmAddr = netip.MustParseAddr("192.168.127.2")
	// hostVIP stands for an address a NAT in the stack would carry to the host (gvisor-tap-vsock's forwarder maps
	// configured addresses to the host's loopback); the neutralized run maps it to 127.0.0.1.
	hostVIP = netip.MustParseAddr("198.18.0.254")
)

func TestMain(m *testing.M) {
	// gvisor-tap-vsock's switch logs every closed link at error level.
	logrus.SetOutput(io.Discard)
	os.Exit(m.Run())
}

func newPair(t *testing.T) (*gvstack.Stack, *gvvm.VM) {
	t.Helper()
	s, file, err := gvstack.Factory{}.NewStack(egress.Actor{Space: "s", Name: "a"}, egress.StackConfig{Gateway: gwAddr, VM: vmAddr, DNS: egress.SyntheticDNS(gwAddr)})
	if err != nil {
		t.Fatal(err)
	}
	if file == nil {
		t.Fatal("NewStack returned no device file")
	}
	vm, err := gvvm.New(file, vmAddr, gwAddr)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		vm.Close()
		_ = s.Close()
	})
	return s.(*gvstack.Stack), vm
}

// TestEveryTCPConnectionIsAFlow (criterion vz-egress): a guest's TCP connection to any address — a public one, the
// gateway, or the address gvisor-tap-vsock's NAT would carry to the host — comes out of Accept with the address the
// guest dialed, and nothing on the host is dialed. A stack wired with gvisor-tap-vsock's forwarder (its own NAT) lets
// the connection to hostVIP reach the host listener instead: red.
func TestEveryTCPConnectionIsAFlow(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var reached atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			reached.Add(1)
			_, _ = c.Write([]byte("host"))
			_ = c.Close()
		}
	}()
	hostPort := uint16(ln.Addr().(*net.TCPAddr).Port)

	s, vm := newPair(t)
	for _, dst := range []netip.AddrPort{
		netip.AddrPortFrom(hostVIP, hostPort),
		netip.MustParseAddrPort("203.0.113.9:443"),
		netip.AddrPortFrom(gwAddr, 8080),
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		dialed := make(chan net.Conn, 1)
		go func() {
			c, err := vm.DialTCP(ctx, dst)
			if err != nil {
				t.Errorf("guest dial %v: %v", dst, err)
				dialed <- nil
				return
			}
			dialed <- c
		}()
		f, err := s.Accept(ctx)
		if err != nil {
			cancel()
			t.Fatalf("guest connection to %v never reached herd-egress (%v); the host listener was reached %d times", dst, err, reached.Load())
		}
		if f.Dst != dst {
			t.Errorf("flow Dst = %v, want %v", f.Dst, dst)
		}
		c := <-dialed
		if c == nil {
			cancel()
			t.FailNow()
		}
		// Bytes flow both ways between the guest and herd-egress's end.
		go func() { _, _ = f.Conn.Write([]byte("egress")); _ = f.Conn.Close() }()
		got, _ := io.ReadAll(c)
		if string(got) != "egress" {
			t.Errorf("guest read %q from %v, want herd-egress's bytes", got, dst)
		}
		_ = c.Close()
		cancel()
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("the host listener was reached %d times: the stack dialed out on its own", n)
	}
}

// TestOnlyDNSToTheGatewayIsAnswered: a DNS query to the gateway is answered by herd-egress's synthetic DNS; DNS to
// an outside resolver, other UDP, IPv6 and packets with a forged source are dropped and counted.
func TestOnlyDNSToTheGatewayIsAnswered(t *testing.T) {
	s, vm := newPair(t)
	r, err := vm.Exchange(netip.AddrPortFrom(gwAddr, 53), dnsQuery("api.example.com"), 5*time.Second)
	if err != nil {
		t.Fatalf("DNS to the gateway: %v", err)
	}
	if got := netip.AddrFrom4([4]byte(r[len(r)-4:])); got != gwAddr {
		t.Errorf("A answer %v, want the gateway %v", got, gwAddr)
	}
	before := s.Dropped()
	if _, err := vm.Exchange(netip.MustParseAddrPort("8.8.8.8:53"), dnsQuery("x.exfil.example"), 300*time.Millisecond); err != gvvm.ErrNoAnswer {
		t.Errorf("DNS to 8.8.8.8: %v, want no answer", err)
	}
	if _, err := vm.Exchange(netip.MustParseAddrPort("198.51.100.60:443"), []byte("quic"), 300*time.Millisecond); err != gvvm.ErrNoAnswer {
		t.Errorf("UDP 443: %v, want no answer", err)
	}
	if _, err := vm.Exchange(netip.AddrPortFrom(gwAddr, 5353), []byte("x"), 300*time.Millisecond); err != gvvm.ErrNoAnswer {
		t.Errorf("UDP to the gateway's 5353: %v, want no answer", err)
	}
	if err := vm.SendFrame(ipv6Frame()); err != nil {
		t.Fatal(err)
	}
	if err := vm.SendFrame(udpFrame(netip.MustParseAddr("192.168.127.9"), gwAddr, 53, dnsQuery("spoofed.example"))); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.Dropped()-before < 5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if d := s.Dropped() - before; d != 5 {
		t.Errorf("dropped %d packets, want 5 (outside DNS, UDP 443, gateway UDP 5353, IPv6, forged source)", d)
	}
}

// TestCloseEndsAccept: a closed stack answers net.ErrClosed, and so does one whose VM end went away.
func TestCloseEndsAccept(t *testing.T) {
	s, _ := newPair(t)
	done := make(chan error, 1)
	go func() {
		_, err := s.Accept(context.Background())
		done <- err
	}()
	_ = s.Close()
	select {
	case err := <-done:
		if err != net.ErrClosed {
			t.Errorf("Accept after Close: %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept did not return after Close")
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	for name, cfg := range map[string]egress.StackConfig{
		"no DNS":       {Gateway: gwAddr, VM: vmAddr},
		"no gateway":   {VM: vmAddr, DNS: egress.SyntheticDNS(gwAddr)},
		"IPv6 gateway": {Gateway: netip.MustParseAddr("fd00::1"), VM: vmAddr, DNS: egress.SyntheticDNS(gwAddr)},
	} {
		if _, _, err := (gvstack.Factory{}).NewStack(egress.Actor{}, cfg); err == nil {
			t.Errorf("%s: NewStack accepted it", name)
		}
	}
}

func dnsQuery(name string) []byte {
	q := []byte{0xab, 0xcd, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	start := 0
	for i := 0; i <= len(name); i++ {
		if i == len(name) || name[i] == '.' {
			q = append(q, byte(i-start))
			q = append(q, name[start:i]...)
			start = i + 1
		}
	}
	return append(q, 0, 0, 1, 0, 1)
}

func ethernet(t tcpip.NetworkProtocolNumber, payload []byte) []byte {
	b := make([]byte, header.EthernetMinimumSize+len(payload))
	header.Ethernet(b).Encode(&header.EthernetFields{
		SrcAddr: tcpip.LinkAddress(mustMAC(gvvm.MAC)), DstAddr: tcpip.LinkAddress(mustMAC(gvstack.GatewayMAC)), Type: t,
	})
	copy(b[header.EthernetMinimumSize:], payload)
	return b
}

func mustMAC(s string) []byte {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return m
}

func udpFrame(src, dst netip.Addr, port uint16, payload []byte) []byte {
	n := header.IPv4MinimumSize + header.UDPMinimumSize + len(payload)
	b := make([]byte, n)
	ip := header.IPv4(b)
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(n), TTL: 64, Protocol: uint8(header.UDPProtocolNumber),
		SrcAddr: tcpip.AddrFrom4(src.As4()), DstAddr: tcpip.AddrFrom4(dst.As4()),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	u := header.UDP(b[header.IPv4MinimumSize:])
	u.Encode(&header.UDPFields{SrcPort: 40000, DstPort: port, Length: uint16(header.UDPMinimumSize + len(payload))})
	copy(u.Payload(), payload)
	u.SetChecksum(^u.CalculateChecksum(checksum.Checksum(payload, header.PseudoHeaderChecksum(header.UDPProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(u))))))
	return ethernet(header.IPv4ProtocolNumber, b)
}

func ipv6Frame() []byte {
	b := make([]byte, header.IPv6MinimumSize)
	header.IPv6(b).Encode(&header.IPv6Fields{TransportProtocol: header.UDPProtocolNumber, HopLimit: 64,
		SrcAddr: tcpip.AddrFrom16([16]byte{0xfd, 15: 2}), DstAddr: tcpip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 15: 1})})
	return ethernet(header.IPv6ProtocolNumber, b)
}
