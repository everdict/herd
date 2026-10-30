package egress_test

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// TestNameEnforcedNotAddress (criterion name-enforced; Everdict defect PLT-180): rules are decided on the name the
// actor asked for and the gateway dials that name, resolving it afresh. A listed host whose DNS rotates to addresses
// nobody saw when the policy was written stays reachable, and an unlisted host that shares a listed host's address
// is refused even when the actor connects to that very address. An address-based allowlist (resolve the listed
// names, allow traffic to those addresses) fails both halves.
func TestNameEnforcedNotAddress(t *testing.T) {
	w := newWorld(t)
	// api.listed.example rotates through three addresses, one per lookup.
	w.resolve("api.listed.example", []string{"198.51.100.1"}, []string{"198.51.100.2"}, []string{"198.51.100.3"}, []string{"198.51.100.4"})
	w.resolve("pass.listed.example", []string{"198.51.100.30"}, []string{"198.51.100.31"})
	// evil.unlisted.example shares the listed host's (first) address, as hosts behind one CDN do.
	w.resolve("evil.unlisted.example", []string{"198.51.100.1"})
	g := newGateway(t, w)
	bind(t, g, actor1, compile(t,
		httpRule(nil, "api.listed.example"),
		httpsRule(nil, "api.listed.example"),
		passRule(8443, "pass.listed.example")))

	// Listed: the actor connects to whatever address its own lookup gave it; every request reaches the upstream,
	// whichever address the name has rotated to by then.
	for i, ip := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"} {
		c := g.client(t, actor1, ip)
		for _, u := range []string{"http://api.listed.example/v1", "https://api.listed.example/v1"} {
			code, body := get(t, c, u, nil)
			if code != 200 || !strings.Contains(body, "reached api.listed.example") {
				t.Errorf("round %d %s via %s: %d %q, want 200 from the upstream", i, u, ip, code, body)
			}
		}
	}
	if code, body := passthrough(t, g, "198.51.100.30", 8443, "pass.listed.example"); code != "ok" {
		t.Errorf("passthrough to pass.listed.example: %s", body)
	}

	// Unlisted, sharing the listed host's address, and the actor connects to exactly that address.
	before := w.httpUp.hits.Load() + w.httpsUp.hits.Load()
	c := g.client(t, actor1, "198.51.100.1")
	for _, u := range []string{"http://evil.unlisted.example/", "https://evil.unlisted.example/"} {
		code, body := get(t, c, u, nil)
		if code == 200 || strings.Contains(body, "reached") {
			t.Errorf("%s at a listed host's address: %d %q, want refused", u, code, body)
		}
	}
	// The listed name in the URL with the unlisted name in the Host header: decided on the authority.
	req, _ := http.NewRequest(http.MethodGet, "http://api.listed.example/", nil)
	req.Host = "evil.unlisted.example"
	if resp, err := c.Do(req); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("Host: evil.unlisted.example reached the upstream: %q", b)
		}
	}
	if code, _ := passthrough(t, g, "198.51.100.30", 8443, "evil.unlisted.example"); code == "ok" {
		t.Errorf("passthrough with SNI evil.unlisted.example was forwarded")
	}
	if got := w.httpUp.hits.Load() + w.httpsUp.hits.Load(); got != before {
		t.Errorf("unlisted requests reached the upstream %d times", got-before)
	}

	// Explicit proxy: CONNECT names the target; the tunneled TLS must name the same host.
	if code, body := connect(t, g, "api.listed.example:443", "api.listed.example"); code != 200 {
		t.Errorf("CONNECT api.listed.example:443: %d %q", code, body)
	}
	if code, _ := connect(t, g, "evil.unlisted.example:443", "evil.unlisted.example"); code == 200 {
		t.Errorf("CONNECT evil.unlisted.example:443 reached the upstream")
	}
	if code, _ := connect(t, g, "api.listed.example:443", "evil.unlisted.example"); code == 200 {
		t.Errorf("CONNECT to a listed host carrying an unlisted SNI reached the upstream")
	}

	// The gateway dialed the rotating answers, never an address because the actor connected to it.
	seen := map[string]bool{}
	for _, d := range w.dials() {
		seen[netip.MustParseAddrPort(d).Addr().String()] = true
	}
	for _, ip := range []string{"198.51.100.2", "198.51.100.3"} {
		if !seen[ip] {
			t.Errorf("gateway never dialed rotated address %s (dialed %v)", ip, w.dials())
		}
	}
	st := g.Stats()
	if st.Refused < 6 {
		t.Errorf("refusals = %d, want >= 6 (%+v)", st.Refused, st)
	}
	t.Logf("decisions: %+v", st)
}

// passthrough opens a TLS connection with sni through the gateway to dst and reads the raw upstream's answer.
func passthrough(t *testing.T, g *gw, ip string, port uint16, sni string) (string, string) {
	t.Helper()
	conn := g.flow(t, actor1, netip.AddrPortFrom(netip.MustParseAddr(ip), port))
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	tc := tls.Client(conn, &tls.Config{ServerName: sni, RootCAs: g.world.upCA.Pool()})
	if err := tc.Handshake(); err != nil {
		return "refused", err.Error()
	}
	fmt.Fprintf(tc, "hello\n")
	b, _ := io.ReadAll(tc)
	if !strings.HasPrefix(string(b), "passthrough "+sni) {
		return "bad", string(b)
	}
	return "ok", string(b)
}

// connect speaks explicit proxy: CONNECT target over a flow to the proxy port, then HTTPS with sni inside.
func connect(t *testing.T, g *gw, target, sni string) (int, string) {
	t.Helper()
	conn := g.flow(t, actor1, netip.MustParseAddrPort("198.51.100.250:3128"))
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return 0, err.Error()
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	tc := tls.Client(&bufConn{Conn: conn, r: br}, &tls.Config{ServerName: sni, RootCAs: g.ca.Pool(), NextProtos: []string{"http/1.1"}})
	if err := tc.Handshake(); err != nil {
		return 0, err.Error()
	}
	fmt.Fprintf(tc, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", sni)
	r2, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		return 0, err.Error()
	}
	b, _ := io.ReadAll(r2.Body)
	return r2.StatusCode, string(b)
}
