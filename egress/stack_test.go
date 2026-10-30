package egress_test

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/egress/stacktest"
)

// TestUserspaceStackPath (criterion platform-plumbing, macOS half): the same gateway behind a userspace stack, as
// on a Mac where the VM's network is a file-handle attachment. The VM resolves through the stack's synthetic DNS,
// reaches only the listed host by name, is refused unlisted and host-side targets, gets every non-TCP, non-DNS
// packet dropped, and sees a policy update on its open connection.
func TestUserspaceStackPath(t *testing.T) {
	w := newWorld(t)
	w.resolve("api.listed.example", []string{"198.51.100.60"})
	w.resolve("other.example", []string{"198.51.100.61"})
	g := newGateway(t, w)
	a := egress.Actor{Space: "space-1", Name: "mac-actor"}
	bind(t, g, a, compile(t, httpRule(nil, "api.listed.example"), httpsRule(nil, "api.listed.example")))

	gwAddr := netip.MustParseAddr("192.168.127.1")
	var f stacktest.Factory
	st, file, err := f.NewStack(a, egress.StackConfig{Gateway: gwAddr, VM: netip.MustParseAddr("192.168.127.2"), DNS: egress.SyntheticDNS(gwAddr)})
	if err != nil {
		t.Fatal(err)
	}
	if file != nil {
		t.Errorf("fake stack returned a device file")
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- g.ServeStack(ctx, a, st) }()
	vm := f.Stacks[a]

	// DNS: any name, even one meant for exfiltration, resolves to the gateway; nothing else answers.
	if got := aRecord(t, vm, gwAddr, "api.listed.example"); got != gwAddr {
		t.Errorf("DNS answer %v, want the gateway %v", got, gwAddr)
	}
	if _, ok := vm.SendUDP(netip.MustParseAddrPort("8.8.8.8:53"), dnsQuery("x.exfil.example")); ok {
		t.Errorf("DNS to an outside resolver was answered")
	}
	if _, ok := vm.SendUDP(netip.MustParseAddrPort("198.51.100.60:443"), []byte("quic")); ok {
		t.Errorf("UDP (QUIC) was forwarded")
	}
	vm.SendOther()

	tr := &http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
		_, ps, _ := net.SplitHostPort(addr)
		port := uint16(80)
		if ps == "443" {
			port = 443
		}
		return vm.DialTCP(ctx, netip.AddrPortFrom(gwAddr, port))
	}, TLSClientConfig: g.client(t, a, "0.0.0.0").Transport.(*http.Transport).TLSClientConfig}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	var refused int
	for _, tc := range []struct {
		url  string
		want int
	}{
		{"http://api.listed.example/", 200},
		{"https://api.listed.example/", 200},
		{"http://other.example/", 403},
		{"http://169.254.169.254/latest/meta-data/", 403},
		{"http://192.168.127.1:8080/", 403},
		{"http://localhost/", 403},
	} {
		code, body := get(t, c, tc.url, nil)
		if code != tc.want {
			t.Errorf("%s: %d %q, want %d", tc.url, code, body, tc.want)
		}
		if code == 403 {
			refused++
		}
	}
	if code, _ := get(t, c, "https://other.example/", nil); code == 200 {
		t.Errorf("https://other.example reached the upstream")
	} else {
		refused++
	}

	// Policy update reaches the open connection.
	if err := g.SetPolicy(a, compile(t, httpRule(nil, "other.example"))); err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, c, "http://api.listed.example/", nil); code != 403 {
		t.Errorf("after the update, api.listed.example: %d, want 403", code)
	}
	if code, _ := get(t, c, "http://other.example/", nil); code != 200 {
		t.Errorf("after the update, other.example: %d, want 200", code)
	}
	if vm.Dropped() != 3 {
		t.Errorf("stack dropped %d packets, want 3", vm.Dropped())
	}
	t.Logf("refusals measured on the stack path: %d at the gateway (%+v), %d packets dropped in the stack", refused, g.Stats(), vm.Dropped())

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("ServeStack: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeStack did not return after cancel")
	}
}

func dnsQuery(name string) []byte {
	q := []byte{0xab, 0xcd, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range strings.Split(name, ".") {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	return append(q, 0, 0, 1, 0, 1)
}

func aRecord(t *testing.T, vm *stacktest.Stack, gw netip.Addr, name string) netip.Addr {
	t.Helper()
	r, ok := vm.SendUDP(netip.AddrPortFrom(gw, 53), dnsQuery(name))
	if !ok {
		t.Fatalf("DNS query for %s dropped", name)
	}
	if binary.BigEndian.Uint16(r[0:2]) != 0xabcd || binary.BigEndian.Uint16(r[6:8]) != 1 {
		t.Fatalf("DNS response %x", r)
	}
	return netip.AddrFrom4([4]byte(r[len(r)-4:]))
}

func TestSyntheticDNS(t *testing.T) {
	ans := egress.SyntheticDNS(netip.MustParseAddr("10.88.0.1"))
	r, err := ans(dnsQuery("api.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if r[2]&0x80 == 0 || r[3]&0x0f != 0 || netip.AddrFrom4([4]byte(r[len(r)-4:])) != netip.MustParseAddr("10.88.0.1") {
		t.Errorf("A response %x", r)
	}
	q := dnsQuery("api.example.com")
	q[len(q)-3] = 28 // AAAA
	r, err = ans(q)
	if err != nil || binary.BigEndian.Uint16(r[6:8]) != 0 || r[3]&0x0f != 0 {
		t.Errorf("AAAA: %x %v, want NOERROR with no answer", r, err)
	}
	q = dnsQuery("a.example")
	q[2] |= 0x10 // opcode 2 (status)
	if r, err := ans(q); err != nil || r[3]&0x0f != 4 {
		t.Errorf("non-query opcode: %x %v, want NOTIMP", r, err)
	}
	for _, bad := range [][]byte{nil, {1, 2, 3}, dnsQuery("a.example")[:15]} {
		if _, err := ans(bad); err == nil {
			t.Errorf("malformed %x answered", bad)
		}
	}
	// Over a real UDP socket.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = egress.ServeDNS(ctx, pc, ans) }()
	c, err := net.Dial("udp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write(dnsQuery("x.example")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 512)
	n, err := c.Read(b)
	if err != nil || netip.AddrFrom4([4]byte(b[n-4:n])) != netip.MustParseAddr("10.88.0.1") {
		t.Errorf("ServeDNS: %x %v", b[:n], err)
	}
}
