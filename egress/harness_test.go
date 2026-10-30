package egress_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/egress/policy"
)

// world is the internet as a test sees it: names resolve through a fake resolver (answers may rotate), and every
// address dials the one upstream server for its scheme on loopback. The guard keeps its full default deny list, so
// loopback is never reached by name.
type world struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr // name -> successive answers (the last one repeats)
	lookups map[string]int

	dialed []string // ip:port the gateway dialed

	httpUp  *upstream
	httpsUp *upstream
	rawUp   net.Listener
	upCA    *egress.CA
}

type upstream struct {
	addr string
	mu   sync.Mutex
	reqs []*http.Request
	hits atomic.Int64
}

func (u *upstream) last() *http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		return nil
	}
	return u.reqs[len(u.reqs)-1]
}

func (u *upstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		u.mu.Lock()
		u.reqs = append(u.reqs, r.Clone(context.Background()))
		u.mu.Unlock()
		fmt.Fprintf(w, "reached %s", r.Host)
	})
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{answers: map[string][][]netip.Addr{}, lookups: map[string]int{}}
	w.httpUp = &upstream{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w.httpUp.addr = ln.Addr().String()
	hs := &http.Server{Handler: w.httpUp.handler()}
	hs.SetKeepAlivesEnabled(false) // every request dials, so the gateway resolves every time
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })

	w.upCA, err = egress.NewCA("upstream test CA")
	if err != nil {
		t.Fatal(err)
	}
	w.httpsUp = &upstream{}
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w.httpsUp.addr = ln2.Addr().String()
	ts := &http.Server{Handler: w.httpsUp.handler(), TLSConfig: &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		return w.upCA.Leaf(h.ServerName)
	}}}
	ts.SetKeepAlivesEnabled(false)
	go func() { _ = ts.ServeTLS(ln2, "", "") }()
	t.Cleanup(func() { _ = ts.Close() })

	// A raw TLS echo server for passthrough: it answers with the SNI it saw.
	w.rawUp, err = tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		return w.upCA.Leaf(h.ServerName)
	}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := w.rawUp.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				tc := c.(*tls.Conn)
				if tc.Handshake() != nil {
					return
				}
				line, _ := bufio.NewReader(tc).ReadString('\n')
				fmt.Fprintf(tc, "passthrough %s %s", tc.ConnectionState().ServerName, line)
			}()
		}
	}()
	t.Cleanup(func() { _ = w.rawUp.Close() })
	return w
}

func (w *world) resolve(name string, answers ...[]string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out [][]netip.Addr
	for _, a := range answers {
		var as []netip.Addr
		for _, s := range a {
			as = append(as, netip.MustParseAddr(s))
		}
		out = append(out, as)
	}
	w.answers[name] = out
}

func (w *world) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	as, ok := w.answers[host]
	if !ok || len(as) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	i := w.lookups[host]
	w.lookups[host] = i + 1
	return as[min(i, len(as)-1)], nil
}

// DialContext dials the upstream for the port's scheme: 443 the https upstream, 8443 the raw TLS one, otherwise
// the http one.
func (w *world) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	w.mu.Lock()
	w.dialed = append(w.dialed, address)
	w.mu.Unlock()
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return nil, err
	}
	target := w.httpUp.addr
	switch ap.Port() {
	case 443:
		target = w.httpsUp.addr
	case 8443:
		target = w.rawUp.Addr().String()
	}
	var d net.Dialer
	return d.DialContext(ctx, network, target)
}

func (w *world) dials() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.dialed...)
}

type gw struct {
	*egress.Gateway
	ca     *egress.CA
	world  *world
	events chan egress.Event
	// flows counts the flows the actor opened.
	flows atomic.Int64
}

type gwOpt func(*egress.Config, *egress.GuardConfig)

func newGateway(t *testing.T, w *world, opts ...gwOpt) *gw {
	t.Helper()
	ca, err := egress.NewCA("herd-egress test CA")
	if err != nil {
		t.Fatal(err)
	}
	g := &gw{ca: ca, world: w, events: make(chan egress.Event, 1024)}
	cfg := egress.Config{Resolver: w, Dialer: w, CA: ca, UpstreamRoots: w.upCA.Pool(), OnEvent: func(e egress.Event) {
		select {
		case g.events <- e:
		default:
		}
	}}
	var gc egress.GuardConfig
	for _, o := range opts {
		o(&cfg, &gc)
	}
	cfg.Guard, err = egress.NewGuard(gc)
	if err != nil {
		t.Fatal(err)
	}
	g.Gateway, err = egress.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// flow opens one TCP flow of actor to dst through the gateway, as a TAP redirect or a userspace stack would.
func (g *gw) flow(t *testing.T, actor egress.Actor, dst netip.AddrPort) net.Conn {
	t.Helper()
	g.flows.Add(1)
	a, b := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go g.ServeConn(ctx, actor, b, dst)
	return a
}

func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			ch <- nil
			return
		}
		ch <- c
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b := <-ch
	if b == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

// client is the actor's HTTP client inside the VM: every name resolves to dstIP (the synthetic DNS answer, or an
// address the actor picked itself), and every connection becomes a flow through the gateway.
func (g *gw) client(t *testing.T, actor egress.Actor, dstIP string) *http.Client {
	tr := &http.Transport{
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			_, ps, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			p, _ := strconv.Atoi(ps)
			return g.flow(t, actor, netip.AddrPortFrom(netip.MustParseAddr(dstIP), uint16(p))), nil
		},
		TLSClientConfig:   &tls.Config{RootCAs: g.ca.Pool()},
		ForceAttemptHTTP2: true,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func get(t *testing.T, c *http.Client, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func compile(t *testing.T, rules ...*ateapi.EgressRule) *policy.Policy {
	t.Helper()
	p, err := policy.CompileRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func httpRule(ports []int32, hosts ...string) *ateapi.EgressRule {
	r := &ateapi.HttpRule{Hostnames: hosts}
	if ports != nil {
		r.Ports = &ateapi.Ports{Numbers: ports}
	}
	return &ateapi.EgressRule{Http: r}
}

func allPortsHTTP(hosts ...string) *ateapi.EgressRule {
	return &ateapi.EgressRule{Http: &ateapi.HttpRule{Hostnames: hosts, Ports: &ateapi.Ports{All: &ateapi.AllPorts{}}}}
}

func httpsRule(effects *ateapi.HttpRuleEffects, hosts ...string) *ateapi.EgressRule {
	return &ateapi.EgressRule{Https: &ateapi.HttpsRule{Hostnames: hosts, Effects: effects}}
}

func passRule(port int32, hosts ...string) *ateapi.EgressRule {
	return &ateapi.EgressRule{TlsPassthrough: &ateapi.TlsPassthroughRule{Hostnames: hosts, Ports: &ateapi.Ports{Numbers: []int32{port}}}}
}

var actor1 = egress.Actor{Space: "space-1", Name: "a1", UID: "uid-a1"}

func bind(t *testing.T, g *gw, a egress.Actor, p *policy.Policy) {
	t.Helper()
	g.Bind(a)
	if err := g.SetPolicy(a, p); err != nil {
		t.Fatal(err)
	}
}

// bufConn reads through a bufio.Reader that may already hold bytes of the connection.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }
