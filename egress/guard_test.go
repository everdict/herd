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
