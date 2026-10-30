package egress_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/everdict/herd/egress"
)

// TestHostSideRefusedBeforeRules (criterion policy-model): the actor's policy allows every name on every port, over
// http and https, and names the metadata service and the host explicitly — yet every host-side target is refused
// by the guard, before the rules, and the upstream is never reached. Without the guard these requests reach the
// upstream (the fake dialer would connect 169.254.169.254 or the host address like any other).
func TestHostSideRefusedBeforeRules(t *testing.T) {
	w := newWorld(t)
	w.resolve("metadata.cloud.example", []string{"169.254.169.254"})
	w.resolve("api.herd-host.example", []string{"203.0.113.7"}) // the host's own address
	w.resolve("rebind.example", []string{"198.51.100.20", "10.0.0.5"})
	w.resolve("ok.example", []string{"198.51.100.21"})
	g := newGateway(t, w, func(_ *egress.Config, gc *egress.GuardConfig) {
		gc.HostAddrs = []string{"203.0.113.7", "203.0.113.8/32"} // the host and herd-api
		gc.HostNames = []string{"herd-api.example", "herd-host.lan"}
	})
	bind(t, g, actor1, compile(t,
		allPortsHTTP("*", "metadata.cloud.example", "api.herd-host.example", "herd-api.example", "rebind.example", "metadata.google.internal"),
		httpsRule(nil, "*")))
	c := g.client(t, actor1, "198.51.100.99")

	refused := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://metadata.cloud.example/latest/meta-data/",
		"http://api.herd-host.example/",
		"http://herd-api.example:8080/",
		"http://herd-host.lan/",
		"http://localhost:8080/",
		"http://127.0.0.1:22/",
		"http://[fd00:ec2::254]/",
		"http://10.0.0.1/",
		"http://rebind.example/",
		"https://metadata.google.internal/",
		"https://169.254.169.254/",
	}
	for _, u := range refused {
		code, body := get(t, c, u, nil)
		if code == 200 || strings.Contains(body, "reached") {
			t.Errorf("%s: reached the upstream (%d %q); a host-side target must be refused before any rule", u, code, body)
		}
	}
	if code, body := get(t, c, "http://ok.example/", nil); code != 200 {
		t.Fatalf("control request to ok.example: %d %q", code, body)
	}
	if n := w.httpUp.hits.Load() + w.httpsUp.hits.Load(); n != 1 {
		t.Errorf("upstream saw %d requests, want only the control request", n)
	}
	st := g.Stats()
	// Every refusal but https://169.254.169.254 (a TLS client sends no SNI for an address) is the guard's.
	if st.RefusedHostSide < int64(len(refused)-1) {
		t.Errorf("RefusedHostSide = %d, want >= %d (stats %+v)", st.RefusedHostSide, len(refused)-1, st)
	}
	for _, d := range w.dials() {
		ap := netip.MustParseAddrPort(d)
		if ap.Addr().IsPrivate() || ap.Addr().IsLinkLocalUnicast() || ap.Addr() == netip.MustParseAddr("203.0.113.7") {
			t.Errorf("gateway dialed host-side address %s", d)
		}
	}
	t.Logf("refusals measured: %+v", st)
}

func TestGuardDefaults(t *testing.T) {
	g, err := egress.NewGuard(egress.GuardConfig{HostAddrs: []string{"203.0.113.7"}, HostNames: []string{"myhost"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.100.100.200",
		"::1", "fe80::1", "fd00:ec2::254", "::ffff:10.0.0.1", "::ffff:127.0.0.1", "203.0.113.7", "0.0.0.0", "224.0.0.1"} {
		if g.CheckAddr(netip.MustParseAddr(a)) == nil {
			t.Errorf("CheckAddr(%s) passed", a)
		}
	}
	for _, a := range []string{"1.1.1.1", "198.51.100.1", "2606:4700::1111"} {
		if err := g.CheckAddr(netip.MustParseAddr(a)); err != nil {
			t.Errorf("CheckAddr(%s): %v", a, err)
		}
	}
	for _, n := range []string{"localhost", "a.localhost", "metadata", "metadata.google.internal", "myhost", "x.myhost", "MyHost.", "1.1.1.1", "[::1]", "printer.local"} {
		if g.CheckName(n) == nil {
			t.Errorf("CheckName(%s) passed", n)
		}
	}
	for _, n := range []string{"api.openai.com", "github.com", "notmyhost"} {
		if err := g.CheckName(n); err != nil {
			t.Errorf("CheckName(%s): %v", n, err)
		}
	}
}

// TestLabAllowOpensExactlyItsPrefix (criterion allow-flag): a tailnet echo host at 100.113.143.93 is refused as
// host-side even when the policy lists its name; with the lab allowance 100.113.143.93/32 it is reached, and every
// other address of the shared space it sits in, its neighbour included, stays refused, as does its IP literal.
func TestLabAllowOpensExactlyItsPrefix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow []string
		want  int
	}{{"without", nil, 403}, {"with", []string{"100.113.143.93/32"}, 200}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.resolve("echo.lab.example", []string{"100.113.143.93"})
			w.resolve("neighbour.lab.example", []string{"100.113.143.94"})
			w.resolve("other.lab.example", []string{"100.64.0.1"})
			g := newGateway(t, w, func(_ *egress.Config, gc *egress.GuardConfig) { gc.LabAllow = tc.allow })
			bind(t, g, actor1, compile(t, allPortsHTTP("echo.lab.example", "neighbour.lab.example", "other.lab.example")))
			c := g.client(t, actor1, "198.51.100.99")
			if code, body := get(t, c, "http://echo.lab.example/", nil); code != tc.want {
				t.Errorf("listed echo.lab.example (100.113.143.93): %d %q, want %d", code, body, tc.want)
			}
			for _, u := range []string{"http://neighbour.lab.example/", "http://other.lab.example/", "http://100.113.143.93/"} {
				if code, body := get(t, c, u, nil); code == 200 || strings.Contains(body, "reached") {
					t.Errorf("%s: reached the upstream (%d %q); only the allowed address may be", u, code, body)
				}
			}
		})
	}
}

// TestLabAllowRefusesBroaderThanWritten (criterion allow-flag): an allowance that would open more than one lab
// host's prefix inside a default denied prefix is refused when the guard is built, and a host address stays
// refused under an allowance that contains it.
func TestLabAllowRefusesBroaderThanWritten(t *testing.T) {
	for _, s := range []string{
		"100.64.0.0/10",     // the whole shared space
		"100.0.0.0/8",       // broader still
		"0.0.0.0/0", "::/0", // everything
		"10.0.0.0/8",                // a whole private prefix
		"100.113.143.93/10",         // host bits: means 100.64.0.0/10
		"100.113.143.93",            // no prefix length
		"8.8.8.8/32",                // nothing to open
		"169.254.169.0/24",          // the metadata endpoint
		"100.100.0.0/16",            // Alibaba's metadata endpoint
		"::ffff:100.113.143.93/128", // IPv4 written as mapped IPv6
	} {
		if _, err := egress.NewGuard(egress.GuardConfig{LabAllow: []string{s}}); err == nil {
			t.Errorf("LabAllow %s accepted", s)
		} else {
			t.Logf("LabAllow %s: %v", s, err)
		}
	}
	g, err := egress.NewGuard(egress.GuardConfig{LabAllow: []string{"100.113.143.93/32", "10.20.30.0/24", "fd7a:115c:a1e0::1/128"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"100.113.143.93", "10.20.30.7", "fd7a:115c:a1e0::1"} {
		if err := g.CheckAddr(netip.MustParseAddr(a)); err != nil {
			t.Errorf("CheckAddr(%s): %v", a, err)
		}
	}
	for _, a := range []string{"100.113.143.92", "100.113.143.94", "100.100.100.200", "10.20.31.1", "10.0.0.1", "fd7a:115c:a1e0::2", "127.0.0.1", "169.254.169.254"} {
		if g.CheckAddr(netip.MustParseAddr(a)) == nil {
			t.Errorf("CheckAddr(%s) passed", a)
		}
	}
	if g.CheckName("100.113.143.93") == nil {
		t.Error("CheckName(100.113.143.93) passed: a destination must be a name, allowed or not")
	}
	h, err := egress.NewGuard(egress.GuardConfig{HostAddrs: []string{"100.113.143.93"}, LabAllow: []string{"100.113.143.0/24"}})
	if err != nil {
		t.Fatal(err)
	}
	if h.CheckAddr(netip.MustParseAddr("100.113.143.93")) == nil {
		t.Error("a host address inside a lab allowance passed")
	}
}
