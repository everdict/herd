package substrateapi_test

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/conformance/substrateapi"
	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/link"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The fake hypervisor's VMs run their processes on this host, so for the egress cases each VM gets a network of its
// own: the test binary's "netshim" starts a VM's processes with http_proxy pointing at that VM's listener and with
// the test binary as wget on PATH, and the listener serves every connection through herd-egress's gateway as the
// VM's actor (vmNet). wget (wgetMain) sends every request to the proxy, as busybox wget does with http_proxy set, so
// nothing a guest asks for leaves without the gateway deciding it. The gateway resolves the test's names
// (listed.test, echo.test) to TEST-NET-3 addresses and its dialer reaches them on loopback, so the Guard stays as on
// a real host: loopback, link-local and metadata are host-side.

// wgetMain is busybox wget's subset the suite uses: -q, -O -, -T <seconds>, --header <h>, one URL.
func wgetMain(args []string) int {
	fs := flag.NewFlagSet("wget", flag.ContinueOnError)
	fs.Bool("q", false, "quiet")
	out := fs.String("O", "", "output")
	timeout := fs.Int("T", 30, "timeout")
	var headers []string
	fs.Func("header", "header", func(s string) error { headers = append(headers, s); return nil })
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *out != "-" {
		fmt.Fprintln(os.Stderr, "wget: usage: wget -q -O - [-T s] [--header h] URL")
		return 2
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	if p := os.Getenv("http_proxy"); p != "" {
		pu, err := url.Parse(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wget:", err)
			return 1
		}
		tr.Proxy = func(*http.Request) (*url.URL, error) { return pu, nil }
	}
	c := &http.Client{Transport: tr, Timeout: time.Duration(*timeout) * time.Second}
	req, err := http.NewRequest(http.MethodGet, fs.Arg(0), nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wget:", err)
		return 1
	}
	for _, h := range headers {
		k, v, _ := strings.Cut(h, ":")
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	resp, err := c.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wget:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		fmt.Fprintf(os.Stderr, "wget: server returned error: %s\n", resp.Status)
		return 1
	}
	_, _ = io.Copy(os.Stdout, resp.Body)
	return 0
}

// netshim prepares a VM's process environment (its proxy, wget on PATH) and runs the shim.
func netshim(proxy, bin string) {
	_ = os.Setenv("http_proxy", proxy)
	_ = os.Setenv("https_proxy", proxy)
	_ = os.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

var (
	testNet  = netip.MustParsePrefix("203.0.113.0/24")
	testName = map[string]netip.Addr{"listed.test": netip.MustParseAddr("203.0.113.10"), "echo.test": netip.MustParseAddr("203.0.113.11")}
)

// resolver resolves the test's names only.
type resolver struct{}

func (resolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := testName[host]; ok {
		return []netip.Addr{a}, nil
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a}, nil
	}
	return nil, fmt.Errorf("no such host %s", host)
}

// dialer reaches a TEST-NET-3 address on loopback at the same port, and anything else as it is.
type dialer struct{}

func (dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	ap, err := netip.ParseAddrPort(addr)
	if err == nil && testNet.Contains(ap.Addr()) {
		addr = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), ap.Port()).String()
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// vmNet gives each fake VM its network: a loopback listener whose connections herd-egress's gateway serves as the
// VM's actor. With bypass, the listener is a plain forward proxy instead: herd-egress is not on the path.
type vmNet struct {
	t      *testing.T
	ctx    context.Context
	gw     *egress.Gateway
	bypass bool
	bin    string
	self   string

	mu     sync.Mutex
	actors map[string]egress.Actor
	lns    map[string]string
}

const secretURI, secret = "ate-secret://dir/conformance/token", "s3cr3t-conformance-credential"

// newVMNet starts the gateway and keeps its actors' policies current from herd-api at api (every 200 ms).
func newVMNet(t *testing.T, bypass bool) *vmNet {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := shortDir(t)
	if err := os.Symlink(self, filepath.Join(bin, "wget")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	n := &vmNet{t: t, ctx: ctx, bypass: bypass, bin: bin, self: self, actors: map[string]egress.Actor{}, lns: map[string]string{}}
	guard, err := egress.NewGuard(egress.GuardConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ca, err := egress.NewCA("conformance herd-egress")
	if err != nil {
		t.Fatal(err)
	}
	n.gw, err = egress.New(egress.Config{Guard: guard, CA: ca, Resolver: resolver{}, Dialer: dialer{},
		Credentials: egress.Providers{"dir": egress.StaticProvider{"conformance/token": secret}}})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// sync reads policies from herd-api at api with tok, as herd-egress does with its egress credential.
func (n *vmNet) sync(api, tok string) {
	cc, err := grpc.NewClient("passthrough:///"+api, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(auth.Bearer(tok)))
	if err != nil {
		n.t.Fatal(err)
	}
	n.t.Cleanup(func() { _ = cc.Close() })
	sy := &egress.Syncer{Gateway: n.gw, Source: egress.ControlSource{Conn: cc}, Interval: 200 * time.Millisecond}
	go func() { _ = sy.Run(n.ctx) }()
}

// OnActor is the herdlet's deploy.Herdlet.OnActor.
func (n *vmNet) OnActor(vm string, a link.ActorRef) {
	ea := egress.Actor{Space: a.Space, Name: a.Name, UID: a.UID}
	n.mu.Lock()
	n.actors[vm] = ea
	n.mu.Unlock()
	n.gw.Bind(ea)
}

// ShimFor is the fake guest's per-VM shim: the netshim with the VM's proxy.
func (n *vmNet) ShimFor(vm string) []string {
	return []string{n.self, "netshim", "http://" + n.listener(vm), n.bin}
}

func (n *vmNet) listener(vm string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if a, ok := n.lns[vm]; ok {
		return a
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		n.t.Errorf("vm network: %v", err)
		return "127.0.0.1:1"
	}
	context.AfterFunc(n.ctx, func() { _ = ln.Close() })
	n.lns[vm] = ln.Addr().String()
	if n.bypass {
		go func() { _ = (&http.Server{Handler: forward{}}).Serve(ln) }()
		return ln.Addr().String()
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.mu.Lock()
			a, ok := n.actors[vm]
			n.mu.Unlock()
			if !ok {
				_ = c.Close()
				continue
			}
			// The destination is the gateway's synthetic address: a proxied request names its own.
			go n.gw.ServeConn(n.ctx, a, c, netip.MustParseAddrPort("198.18.0.1:80"))
		}
	}()
	return ln.Addr().String()
}

// forward is an HTTP forward proxy that decides nothing: herd-egress bypassed.
type forward struct{}

func (forward) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		up, err := dialer{}.DialContext(r.Context(), "tcp", resolve(r.Host))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = up.Close()
			return
		}
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
		_, _ = io.Copy(c, up)
		_ = c.Close()
		return
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) { pr.Out.Host = r.Host },
		Transport: &http.Transport{DialContext: func(ctx context.Context, n, a string) (net.Conn, error) {
			return dialer{}.DialContext(ctx, n, resolve(a))
		}},
	}
	rp.ServeHTTP(w, r)
}

// resolve turns a test name in host:port into its address.
func resolve(hostport string) string {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	if a, ok := testName[h]; ok {
		return net.JoinHostPort(a.String(), p)
	}
	return hostport
}

// idleProxy forwards TCP to target and cuts a connection that carried nothing either way for idle: a router path
// with an idle timeout.
func idleProxy(t *testing.T, target string, idle time.Duration) string {
	t.Helper()
	ln := listen(t)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				up, err := net.Dial("tcp", target)
				if err != nil {
					_ = c.Close()
					return
				}
				pipe := func(dst, src net.Conn) {
					b := make([]byte, 32<<10)
					for {
						_ = src.SetReadDeadline(time.Now().Add(idle))
						n, err := src.Read(b)
						if n > 0 {
							_ = dst.SetReadDeadline(time.Now().Add(idle))
							if _, werr := dst.Write(b[:n]); werr != nil {
								break
							}
						}
						// A read that timed out is the idle cut.
						if err != nil {
							break
						}
					}
					_ = c.Close()
					_ = up.Close()
				}
				go pipe(up, c)
				pipe(c, up)
			}()
		}
	}()
	return ln.Addr().String()
}

// echoes starts the suite's echo server and the listed host, both reached by the guest under test names.
func echoes(t *testing.T) (*substrateapi.Echo, string, string) {
	t.Helper()
	e, err := substrateapi.StartEcho("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	listed := listen(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "listed\n") })}
	go func() { _ = srv.Serve(listed) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, ep, _ := net.SplitHostPort(e.Addr())
	_, lp, _ := net.SplitHostPort(listed.Addr().String())
	return e, "http://echo.test:" + ep + "/", "http://listed.test:" + lp + "/"
}

// wantFail runs the suite and wants each named case FAIL.
func wantFail(t *testing.T, cfg substrateapi.Config, cases ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep := substrateapi.Run(ctx, cfg)
	for _, c := range cases {
		r, _ := rep.Get(c)
		if r.Outcome != substrateapi.Fail {
			t.Errorf("%s: %s %s; want FAIL", c, r.Outcome, r.Detail)
			continue
		}
		t.Logf("%s FAIL as wanted: %s", c, r.Detail)
	}
}

// TestEgressCasesRedWhenEgressBypassed: when the VMs' network is a proxy that decides nothing (herd-egress
// bypassed), the egress walk fails (the unlisted echo host answers) and so does the credential header (the echo
// server sees the guest's placeholder).
func TestEgressCasesRedWhenEgressBypassed(t *testing.T) {
	n := newVMNet(t, true)
	addrs, tok := startAllInOneWith(t, n)
	cfg := suiteConfig("herd-egress-bypassed", addrs.API, addrs.Router, tok)
	e, echoURL, listedURL := echoes(t)
	optional(&cfg, e, echoURL, listedURL)
	cfg.EgressBound = 2 * time.Second
	cfg.LongStream, cfg.Template2 = false, ""
	wantFail(t, cfg, "egress-walk", "credential-header")
}

// TestLongStreamRedUnderRouterIdleTimeout: with a router path that cuts a connection idle for less than the
// stream's gaps, the long stream fails before its duration.
func TestLongStreamRedUnderRouterIdleTimeout(t *testing.T) {
	addrs, tok := startAllInOne(t)
	cfg := suiteConfig("herd-router-idle-timeout", addrs.API, idleProxy(t, addrs.Router, 150*time.Millisecond), tok)
	cfg.LongStream, cfg.StreamFor, cfg.StreamGap = true, 1500*time.Millisecond, 500*time.Millisecond
	wantFail(t, cfg, "long-stream")
}
