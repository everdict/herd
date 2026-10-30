package egress_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/egress/gvstack"
	"github.com/everdict/herd/egress/gvstack/gvvm"
	"github.com/sirupsen/logrus"
)

// TestGVisorStackPath (criterion vz-egress): the macOS path end to end with the real userspace stack — a guest
// netstack on the VM's end of the file-handle socket pair, gvstack on the other, StackNetwork serving it through
// the gateway. The guest resolves through the gateway's synthetic DNS, reaches the listed host by name and is
// refused an unlisted one, a host-side target and the gateway's own address; after Release the actor is unbound and
// its link is dead.
func TestGVisorStackPath(t *testing.T) {
	logrus.SetOutput(io.Discard)
	w := newWorld(t)
	w.resolve("api.listed.example", []string{"198.51.100.60"})
	w.resolve("other.example", []string{"198.51.100.61"})
	g := newGateway(t, w)
	a := egress.Actor{Space: "space-1", Name: "mac-actor", UID: "u-1"}
	n := &egress.StackNetwork{Gateway: g.Gateway, Factory: gvstack.Factory{}}

	// Attached before it has an actor (a warm VM): its flows are refused until Assign.
	file, err := n.Attach("vm-1")
	if err != nil {
		t.Fatal(err)
	}
	vm, err := gvvm.New(file, egress.DefaultStackVM, egress.DefaultStackGateway)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	if c, err := vm.DialTCP(context.Background(), netip.MustParseAddrPort("198.51.100.60:80")); err == nil {
		_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: api.listed.example\r\n\r\n")
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if b, _ := io.ReadAll(c); len(b) != 0 {
			t.Errorf("a VM with no actor was answered: %q", b)
		}
		c.Close()
	}
	n.Assign("vm-1", a)
	bind(t, g, a, compile(t, httpRule(nil, "api.listed.example"), httpsRule(nil, "api.listed.example")))

	r, err := vm.Exchange(netip.AddrPortFrom(egress.DefaultStackGateway, 53), dnsQuery("api.listed.example"), 5*time.Second)
	if err != nil {
		t.Fatalf("DNS: %v", err)
	}
	ip := netip.AddrFrom4([4]byte(r[len(r)-4:]))
	if ip != egress.DefaultStackGateway {
		t.Errorf("DNS answer %v, want the gateway", ip)
	}

	tr := &http.Transport{DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
		_, ps, _ := net.SplitHostPort(addr)
		p, _ := strconv.Atoi(ps)
		return vm.DialTCP(ctx, netip.AddrPortFrom(ip, uint16(p)))
	}, TLSClientConfig: &tls.Config{RootCAs: g.ca.Pool()}}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	for _, tc := range []struct {
		url  string
		want int
	}{
		{"http://api.listed.example/", 200},
		{"https://api.listed.example/", 200},
		{"http://other.example/", 403},
		{"http://169.254.169.254/latest/meta-data/", 403},
		{"http://192.168.127.1:8080/", 403},
	} {
		if code, body := get(t, c, tc.url, nil); code != tc.want {
			t.Errorf("%s: %d %q, want %d", tc.url, code, body, tc.want)
		}
	}
	if _, err := vm.Exchange(netip.MustParseAddrPort("8.8.8.8:53"), dnsQuery("x.exfil.example"), 300*time.Millisecond); err != gvvm.ErrNoAnswer {
		t.Errorf("DNS to 8.8.8.8: %v, want no answer", err)
	}
	if d := n.Dropped("vm-1"); d < 1 {
		t.Errorf("the stack dropped %d packets, want the outside DNS query counted", d)
	}
	tr.CloseIdleConnections()

	n.Release("vm-1")
	for _, b := range g.Actors() {
		if b == a {
			t.Errorf("actor still bound after Release")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if conn, err := vm.DialTCP(ctx, netip.MustParseAddrPort("198.51.100.60:80")); err == nil {
		conn.Close()
		t.Errorf("a released VM's link still carries connections")
	}
}
