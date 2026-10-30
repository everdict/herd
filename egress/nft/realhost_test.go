package nft_test

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/egress/nft"
	"github.com/everdict/herd/egress/policy"
)

// TestRealHostEgress (criterion platform-plumbing, real host; HERD_EGRESS_REAL=1, root, ip(8), nft(8)) applies a
// rendered VM table in a host network namespace whose "TAP" is one end of a veth pair; the other end sits in a VM
// namespace. herd-egress runs in the host namespace (this binary re-executed), the probes in the VM namespace (this
// binary again). It measures: a listed host reached by name through the redirect, an unlisted one refused, the
// host's own service (a herd-api stand-in) unreachable, UDP to the host and the internet dropped, DNS answered only
// by the synthetic resolver, spoofed sources dropped; and the nft counters. It also creates and removes a real TAP
// with nft.Apply / nft.Remove.
func TestRealHostEgress(t *testing.T) {
	switch os.Getenv("HERD_EGRESS_REAL_ROLE") {
	case "gateway":
		realGateway(t)
		return
	case "probe":
		realProbe(t)
		return
	}
	if os.Getenv("HERD_EGRESS_REAL") != "1" {
		t.Skip("set HERD_EGRESS_REAL=1 (root, ip, nft) to apply the ruleset in network namespaces")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sfx := fmt.Sprint(os.Getpid())
	hostNS, vmNS := "herdeg-h-"+sfx, "herdeg-v-"+sfx
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	inH := func(args ...string) string { return run(append([]string{"ip", "netns", "exec", hostNS}, args...)...) }
	inV := func(args ...string) string { return run(append([]string{"ip", "netns", "exec", vmNS}, args...)...) }
	run("ip", "netns", "add", hostNS)
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", hostNS).Run() })
	run("ip", "netns", "add", vmNS)
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", vmNS).Run() })

	v := nft.VM{ID: "rt", TAP: "herd-rt0", Host: netip.MustParsePrefix("10.88.0.1/29"), VM: netip.MustParseAddr("10.88.0.2"), ProxyPort: 15001, DNSPort: 15053}
	inH("ip", "link", "set", "lo", "up")
	inH("ip", "link", "add", v.TAP, "type", "veth", "peer", "name", "eth0", "netns", vmNS)
	inH("ip", "addr", "add", v.Host.String(), "dev", v.TAP)
	rs, err := nft.Ruleset(v)
	if err != nil {
		t.Fatal(err)
	}
	host := nft.ExecHost{Prefix: []string{"ip", "netns", "exec", hostNS}}
	if _, err := host.Run(ctx, nft.Command{Name: "nft", Args: []string{"-f", "-"}, Stdin: rs}); err != nil {
		t.Fatal(err)
	}
	inH("ip", "link", "set", v.TAP, "up")
	inV("ip", "link", "set", "lo", "up")
	inV("ip", "addr", "add", "10.88.0.2/29", "dev", "eth0")
	inV("ip", "addr", "add", "10.88.0.3/29", "dev", "eth0") // the spoofing source
	inV("ip", "link", "set", "eth0", "up")
	inV("ip", "route", "add", "default", "via", "10.88.0.1")

	// herd-egress in the host namespace.
	gw := exec.CommandContext(ctx, "ip", "netns", "exec", hostNS, os.Args[0], "-test.run=^TestRealHostEgress$", "-test.v")
	gw.Env = append(os.Environ(), "HERD_EGRESS_REAL_ROLE=gateway")
	gwIn, _ := gw.StdinPipe()
	gwOut, _ := gw.StdoutPipe()
	gw.Stderr = os.Stderr
	if err := gw.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(gwOut)
	waitLine := func(prefix string) string {
		for lines.Scan() {
			if s, ok := strings.CutPrefix(lines.Text(), prefix); ok {
				return s
			}
		}
		t.Fatalf("gateway ended before %q", prefix)
		return ""
	}
	waitLine("READY")

	probe := exec.CommandContext(ctx, "ip", "netns", "exec", vmNS, os.Args[0], "-test.run=^TestRealHostEgress$", "-test.v")
	probe.Env = append(os.Environ(), "HERD_EGRESS_REAL_ROLE=probe")
	out, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, out)
	}
	var res probeResults
	for _, l := range strings.Split(string(out), "\n") {
		if s, ok := strings.CutPrefix(l, "RESULTS "); ok {
			if err := json.Unmarshal([]byte(s), &res); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = gwIn.Close()
	var gs gatewayStats
	if err := json.Unmarshal([]byte(waitLine("STATS ")), &gs); err != nil {
		t.Fatal(err)
	}
	_ = gw.Wait()
	counters := inH("nft", "list", "table", "inet", v.Table())
	t.Logf("probe results: %+v", res)
	t.Logf("gateway: %+v", gs)
	t.Logf("nft table after the probes:\n%s", counters)

	if res.ListedStatus != 200 {
		t.Errorf("listed host: status %d (%s), want 200", res.ListedStatus, res.ListedBody)
	}
	if res.UnlistedStatus != 403 {
		t.Errorf("unlisted host: status %d, want 403", res.UnlistedStatus)
	}
	if res.HerdAPIStatus == 200 || gs.HerdAPIConns != 0 {
		t.Errorf("herd-api stand-in reached: status %d, %d connections", res.HerdAPIStatus, gs.HerdAPIConns)
	}
	if gs.UDPSinkPackets != 0 {
		t.Errorf("UDP to the host reached a host socket (%d packets)", gs.UDPSinkPackets)
	}
	if res.DNSAnswer != "10.88.0.1" || res.DNSExternalAnswer != "10.88.0.1" {
		t.Errorf("DNS answers %q / %q, want the synthetic 10.88.0.1 for both", res.DNSAnswer, res.DNSExternalAnswer)
	}
	if res.SpoofedConnected {
		t.Errorf("a connection from a spoofed source was answered")
	}
	if gs.Refused < 2 {
		t.Errorf("gateway refusals %d, want >= 2", gs.Refused)
	}
	for _, c := range []string{"refused_input", "refused_spoofed"} {
		if counterZero(counters, c) {
			t.Errorf("nft counter %s is zero", c)
		}
	}

	// The real TAP path: Apply creates TAP + table, Remove deletes both.
	tv := nft.VM{ID: "rttap", TAP: "herd-rttap", Host: netip.MustParsePrefix("10.88.1.1/30"), VM: netip.MustParseAddr("10.88.1.2"), ProxyPort: 15001, DNSPort: 15053}
	if err := nft.Apply(ctx, host, tv, -1); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(inH("ip", "-d", "link", "show", tv.TAP), "tun type tap") {
		t.Errorf("Apply did not create a TAP")
	}
	inH("nft", "list", "table", "inet", tv.Table())
	if err := nft.Remove(ctx, host, tv); err != nil {
		t.Errorf("Remove: %v", err)
	}
}

func counterZero(table, name string) bool {
	i := strings.Index(table, "counter "+name)
	if i < 0 {
		return true
	}
	return strings.Contains(table[i:min(len(table), i+120)], "packets 0 ")
}

type probeResults struct {
	ListedStatus      int
	ListedBody        string
	UnlistedStatus    int
	HerdAPIStatus     int
	DNSAnswer         string
	DNSExternalAnswer string
	SpoofedConnected  bool
}

type gatewayStats struct {
	Allowed, Refused, RefusedHostSide int64
	HerdAPIConns                      int64
	UDPSinkPackets                    int64
}

func realGateway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The internet: a loopback upstream that every resolved address dials.
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = http.Serve(up, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "reached %s", r.Host) }))
	}()
	// The host's own services, which the VM must not reach: herd-api and a UDP socket.
	var apiConns, udpPkts atomic.Int64
	api, err := net.Listen("tcp", "10.88.0.1:7443")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := api.Accept()
			if err != nil {
				return
			}
			apiConns.Add(1)
			_ = c.Close()
		}
	}()
	sink, err := net.ListenPacket("udp", "10.88.0.1:9999")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		b := make([]byte, 1500)
		for {
			if _, _, err := sink.ReadFrom(b); err != nil {
				return
			}
			udpPkts.Add(1)
		}
	}()

	guard, err := egress.NewGuard(egress.GuardConfig{HostAddrs: []string{"10.88.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	g, err := egress.New(egress.Config{Guard: guard, Resolver: staticResolver{"listed.example": "198.51.100.7", "unlisted.example": "198.51.100.7"},
		Dialer: upstreamDialer(up.Addr().String())})
	if err != nil {
		t.Fatal(err)
	}
	a := egress.Actor{Space: "s", Name: "rt"}
	g.Bind(a)
	p, err := policy.CompileRules([]*ateapi.EgressRule{{Http: &ateapi.HttpRule{Hostnames: []string{"listed.example"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.SetPolicy(a, p); err != nil {
		t.Fatal(err)
	}
	var src egress.Sources
	src.Set(netip.MustParseAddr("10.88.0.2"), a)
	ln, err := net.Listen("tcp", ":15001")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.ServeTransparent(ctx, ln, &src, nil) }()
	pc, err := net.ListenPacket("udp", ":15053")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = egress.ServeDNS(ctx, pc, egress.SyntheticDNS(netip.MustParseAddr("10.88.0.1"))) }()
	fmt.Println("READY")
	_, _ = io.Copy(io.Discard, os.Stdin)
	st := g.Stats()
	b, _ := json.Marshal(gatewayStats{Allowed: st.Allowed, Refused: st.Refused, RefusedHostSide: st.RefusedHostSide,
		HerdAPIConns: apiConns.Load(), UDPSinkPackets: udpPkts.Load()})
	fmt.Println("STATS " + string(b))
}

type staticResolver map[string]string

func (s staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	a, ok := s[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return []netip.Addr{netip.MustParseAddr(a)}, nil
}

type upstreamDialer string

func (u upstreamDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, string(u))
}

func realProbe(t *testing.T) {
	var r probeResults
	get := func(ip, host string) (int, string) {
		c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, n, ip)
		}}}
		resp, err := c.Get("http://" + host + "/")
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	r.ListedStatus, r.ListedBody = get("198.51.100.7:80", "listed.example")
	r.UnlistedStatus, _ = get("198.51.100.7:80", "unlisted.example")
	r.HerdAPIStatus, _ = get("10.88.0.1:7443", "10.88.0.1:7443")
	for _, dst := range []string{"10.88.0.1:9999", "198.51.100.7:9999"} {
		if c, err := net.Dial("udp", dst); err == nil {
			_, _ = c.Write([]byte("leak"))
			_ = c.Close()
		}
	}
	r.DNSAnswer = dnsA("10.88.0.1:53", "listed.example")
	r.DNSExternalAnswer = dnsA("8.8.8.8:53", "exfil.example")
	d := net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP("10.88.0.3")}}
	if c, err := d.Dial("tcp", "198.51.100.7:80"); err == nil {
		r.SpoofedConnected = true
		_ = c.Close()
	}
	time.Sleep(200 * time.Millisecond) // let the UDP probes land if they were going to
	b, _ := json.Marshal(r)
	fmt.Println("RESULTS " + string(b))
}

func dnsA(server, name string) string {
	c, err := net.Dial("udp", server)
	if err != nil {
		return err.Error()
	}
	defer c.Close()
	q := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range strings.Split(name, ".") {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	q = append(q, 0, 0, 1, 0, 1)
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write(q); err != nil {
		return err.Error()
	}
	b := make([]byte, 512)
	n, err := c.Read(b)
	if err != nil {
		return err.Error()
	}
	if n < len(q)+16 || binary.BigEndian.Uint16(b[6:8]) != 1 {
		return "no answer"
	}
	return netip.AddrFrom4([4]byte(b[n-4 : n])).String()
}
