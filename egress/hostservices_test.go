package egress_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/egress"
)

// The relay a herder serves on its node's private address, and the name actors reach it by (DEFAUL-1599).
const (
	relayName   = "relay.herd.internal"
	relayTarget = "10.1.2.3:8787"
	relayLine   = relayName + ":8787 " + relayTarget
)

func (w *world) lookupsOf(name string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lookups[name]
}

func loadServices(t *testing.T, g *gw, file string) []error {
	t.Helper()
	dropped, err := g.LoadHostServices([]byte(file))
	if err != nil {
		t.Fatalf("LoadHostServices(%q): %v", file, err)
	}
	return dropped
}

// proxyGet sends an explicit-proxy request (absolute-form) for url over one flow to the proxy port.
func proxyGet(t *testing.T, g *gw, url string) (int, string) {
	t.Helper()
	conn := g.flow(t, actor1, netip.MustParseAddrPort("198.51.100.250:3128"))
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	host := strings.TrimPrefix(url, "http://")
	host, _, _ = strings.Cut(host, "/")
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", url, host)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func lastRefusal(g *gw) (egress.Event, bool) {
	var ev egress.Event
	found := false
	for {
		select {
		case e := <-g.events:
			if !e.Allowed {
				ev, found = e, true
			}
		default:
			return ev, found
		}
	}
}

// TestDeclaredHostServiceDialsItsTarget (declared-host-service): a declared name:port is dialed at exactly its
// declared target, a private address, with no DNS lookup and no address check, on the transparent path and on the
// explicit-proxy path. Without the declaration the name is under .internal and the guard refuses it host-side.
func TestDeclaredHostServiceDialsItsTarget(t *testing.T) {
	w := newWorld(t)
	g := newGateway(t, w)
	bind(t, g, actor1, compile(t, httpRule([]int32{8787}, relayName)))
	if d := loadServices(t, g, "# the herder's relay\n\n"+relayLine+"\n"); len(d) != 0 {
		t.Fatalf("dropped %v", d)
	}
	c := g.client(t, actor1, "198.18.0.1")
	if code, body := get(t, c, "http://"+relayName+":8787/r/label", nil); code != 200 || !strings.Contains(body, "reached "+relayName) {
		t.Errorf("transparent GET %s: %d %q, want 200 from the declared target", relayName, code, body)
	}
	if code, body := proxyGet(t, g, "http://"+relayName+":8787/r/label"); code != 200 {
		t.Errorf("explicit-proxy GET %s: %d %q, want 200", relayName, code, body)
	}
	if n := w.lookupsOf(relayName); n != 0 {
		t.Errorf("the gateway looked %s up %d times; a declared name is never resolved", relayName, n)
	}
	for _, d := range w.dials() {
		if d != relayTarget {
			t.Errorf("dialed %s, want only the declared target %s", d, relayTarget)
		}
	}
	if len(w.dials()) == 0 {
		t.Errorf("the declared target was never dialed")
	}
}

// TestDeclaredHostServiceDefaultDialer: with herd-egress's own dialer, whose Control re-checks every address with
// the guard, a declared loopback target the file marks loopback is reached, and an undeclared loopback is not.
func TestDeclaredHostServiceDefaultDialer(t *testing.T) {
	w := newWorld(t)
	g := newGateway(t, w, func(c *egress.Config, _ *egress.GuardConfig) { c.Dialer = nil })
	bind(t, g, actor1, compile(t, allPortsHTTP("pc.herd.internal", "sneaky.example")))
	_, port, _ := net.SplitHostPort(w.httpUp.addr)
	w.resolve("sneaky.example", []string{"127.0.0.1"})
	if d := loadServices(t, g, "pc.herd.internal:80 "+w.httpUp.addr+" loopback\n"); len(d) != 0 {
		t.Fatalf("dropped %v", d)
	}
	c := g.client(t, actor1, "198.18.0.1")
	if code, body := get(t, c, "http://pc.herd.internal/", nil); code != 200 {
		t.Errorf("declared loopback service: %d %q, want 200", code, body)
	}
	if code, body := get(t, c, "http://sneaky.example:"+port+"/", nil); code == 200 {
		t.Errorf("an undeclared name resolving to the same loopback endpoint was reached: %q", body)
	}
}

// TestUndeclaredPrivateNameRefused (declared-host-service): only the declared name:port goes to its target. A name in
// public DNS that resolves to the same private address, and the declared name on another port, stay refused
// host-side, and nothing is dialed.
func TestUndeclaredPrivateNameRefused(t *testing.T) {
	w := newWorld(t)
	w.resolve("relay.example", []string{"10.1.2.3"})
	g := newGateway(t, w)
	bind(t, g, actor1, compile(t, allPortsHTTP("relay.example", relayName)))
	loadServices(t, g, relayLine)
	c := g.client(t, actor1, "198.18.0.1")
	for _, u := range []string{"http://relay.example:8787/", "http://" + relayName + ":9000/"} {
		code, body := get(t, c, u, nil)
		if code != http.StatusForbidden {
			t.Errorf("%s: %d %q, want 403", u, code, body)
		}
		if ev, ok := lastRefusal(g); !ok || !ev.HostSide {
			t.Errorf("%s: refusal %+v, want host-side", u, ev)
		}
	}
	if d := w.dials(); len(d) != 0 {
		t.Errorf("dialed %v; nothing may be dialed", d)
	}
}

// TestDeclaredNameNeedsRule (declared-host-service): a declared name with no policy rule is refused by the policy,
// which runs before the declaration is consulted, on the transparent path, the explicit-proxy path, CONNECT and a TLS
// ClientHello. The synthetic DNS knows nothing of declarations: it answers the declared name with the gateway.
func TestDeclaredNameNeedsRule(t *testing.T) {
	w := newWorld(t)
	g := newGateway(t, w)
	bind(t, g, actor1, compile(t, allPortsHTTP("other.example"), passRule(8443, "other.example"), httpsRule(nil, "other.example")))
	loadServices(t, g, relayLine+"\n"+relayName+":443 10.1.2.3:443\n"+relayName+":8443 10.1.2.3:8443\n")
	c := g.client(t, actor1, "198.18.0.1")
	check := func(what string, code int, body string) {
		t.Helper()
		if code == 200 || strings.Contains(body, "reached") {
			t.Errorf("%s: reached (%d %q) with no rule for %s", what, code, body, relayName)
		}
		ev, ok := lastRefusal(g)
		if !ok || ev.HostSide || ev.Name != relayName {
			t.Errorf("%s: refusal %+v, want the policy's (not host-side) for %s", what, ev, relayName)
		}
	}
	code, body := get(t, c, "http://"+relayName+":8787/", nil)
	check("transparent", code, body)
	code, body = proxyGet(t, g, "http://"+relayName+":8787/")
	check("explicit proxy", code, body)
	code, body = connect(t, g, relayName+":443", relayName)
	check("CONNECT", code, body)
	res, msg := passthrough(t, g, "198.18.0.1", 8443, relayName)
	check("TLS ClientHello", map[string]int{"ok": 200}[res], msg)
	if d := w.dials(); len(d) != 0 {
		t.Errorf("dialed %v; nothing may be dialed", d)
	}

	q := dnsQuery(relayName)
	resp, err := egress.SyntheticDNS(netip.MustParseAddr("198.18.0.1"))(q)
	if err != nil {
		t.Fatal(err)
	}
	if got := netip.AddrFrom4([4]byte(resp[len(resp)-4:])); got != netip.MustParseAddr("198.18.0.1") {
		t.Errorf("synthetic DNS answered %s with %s, want the gateway", relayName, got)
	}
}

// TestHostServiceTargetRefusedAtLoad (declared-host-service): a target that is a metadata endpoint, a host address,
// the Control or router endpoint, an unspecified address, or loopback the file does not mark loopback is refused at
// load: that line alone is dropped, and an actor whose policy names it is not dialed there.
func TestHostServiceTargetRefusedAtLoad(t *testing.T) {
	w := newWorld(t)
	g := newGateway(t, w, func(c *egress.Config, gc *egress.GuardConfig) {
		gc.HostAddrs = []string{"203.0.113.7", "10.9.0.0/16"}
		c.Endpoints = []netip.AddrPort{netip.MustParseAddrPort("10.1.2.3:7443"), netip.MustParseAddrPort("10.1.2.4:7444")}
	})
	bind(t, g, actor1, compile(t, allPortsHTTP("*.herd.internal")))
	bad := map[string]string{
		"meta.herd.internal":    "169.254.169.254:80",
		"alibaba.herd.internal": "100.100.100.200:80",
		"ec2v6.herd.internal":   "[fd00:ec2::254]:80",
		"host.herd.internal":    "203.0.113.7:80",
		"hostnet.herd.internal": "10.9.4.4:80",
		"api.herd.internal":     "10.1.2.3:7443",
		"router.herd.internal":  "10.1.2.4:7444",
		"lo.herd.internal":      "127.0.0.1:80",
		"lo6.herd.internal":     "[::1]:80",
		"any.herd.internal":     "0.0.0.0:80",
	}
	var file strings.Builder
	for n, target := range bad {
		fmt.Fprintf(&file, "%s:80 %s\n", n, target)
	}
	file.WriteString(relayName + ":80 10.1.2.3:80\n")
	dropped := loadServices(t, g, file.String())
	if len(dropped) != len(bad) {
		t.Errorf("dropped %d lines, want %d: %v", len(dropped), len(bad), dropped)
	}
	got := map[string]bool{}
	for _, s := range g.HostServices() {
		got[s.Name] = true
	}
	if !got[relayName] || len(got) != 1 {
		t.Errorf("services after load = %v, want only %s", got, relayName)
	}
	c := g.client(t, actor1, "198.18.0.1")
	for n := range bad {
		if code, body := get(t, c, "http://"+n+"/", nil); code == 200 {
			t.Errorf("%s was reached: %q", n, body)
		}
	}
	for _, d := range w.dials() {
		if d != "10.1.2.3:80" {
			t.Errorf("dialed %s", d)
		}
	}
}

// TestHostServicesReloadAddsAndRemoves (declared-host-service): a load replaces the whole set: a service the file no
// longer names is refused, one it newly names is reached.
func TestHostServicesReloadAddsAndRemoves(t *testing.T) {
	w := newWorld(t)
	g := newGateway(t, w)
	bind(t, g, actor1, compile(t, allPortsHTTP("a.herd.internal", "b.herd.internal")))
	c := g.client(t, actor1, "198.18.0.1")
	loadServices(t, g, "a.herd.internal:80 10.0.0.1:80\n")
	if code, _ := get(t, c, "http://a.herd.internal/", nil); code != 200 {
		t.Errorf("a before reload: %d, want 200", code)
	}
	if code, _ := get(t, c, "http://b.herd.internal/", nil); code == 200 {
		t.Errorf("b before reload was reached")
	}
	loadServices(t, g, "b.herd.internal:80 10.0.0.2:80\n")
	if code, _ := get(t, c, "http://a.herd.internal/", nil); code == 200 {
		t.Errorf("a after it was removed was reached")
	}
	if code, _ := get(t, c, "http://b.herd.internal/", nil); code != 200 {
		t.Errorf("b after it was added: %d, want 200", code)
	}
	want := []string{"10.0.0.1:80", "10.0.0.2:80"}
	if d := w.dials(); fmt.Sprint(d) != fmt.Sprint(want) {
		t.Errorf("dialed %v, want %v", d, want)
	}
}

// TestHostServicesMalformedReloadKeepsLastSet (declared-host-service): a malformed file is refused whole, and the set
// the last good load gave keeps serving.
func TestHostServicesMalformedReloadKeepsLastSet(t *testing.T) {
	w := newWorld(t)
	g := newGateway(t, w)
	bind(t, g, actor1, compile(t, allPortsHTTP(relayName)))
	loadServices(t, g, relayLine)
	for _, bad := range []string{
		"relay.herd.internal 10.1.2.3:8787",
		"relay.herd.internal:8787",
		"relay.herd.internal:8787 relay-host:8787",
		"relay.herd.internal:99999 10.1.2.3:8787",
		"10.0.0.1:80 10.1.2.3:8787",
		"relay.herd.internal:8787 10.1.2.3:8787 sideways",
		relayLine + "\n" + relayName + ":8787 10.1.2.5:8787",
		"",
	} {
		_, err := g.LoadHostServices([]byte(bad))
		if bad == "" {
			if err != nil {
				t.Errorf("an empty file is a valid empty set: %v", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("LoadHostServices(%q) = nil error, want malformed", bad)
		}
		if s := g.HostServices(); len(s) != 1 || s[0].Target != netip.MustParseAddrPort(relayTarget) {
			t.Errorf("after malformed %q the set is %v, want the last good one", bad, s)
		}
	}
	if s := g.HostServices(); len(s) != 0 {
		t.Errorf("an empty file leaves %v", s)
	}
}
