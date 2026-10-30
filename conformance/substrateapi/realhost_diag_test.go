package substrateapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance/substrateapi"
	"github.com/everdict/herd/deploy"
	"github.com/everdict/herd/egress/tapnet"
)

// netDiagnosis boots one more actor of the suite's template on the real host and logs, from inside its guest and
// from the host, everything the egress path depends on, so a failed egress case explains itself:
//   - guest (env exec in the container): ip addr, ip route, /etc/resolv.conf, an nslookup of listed.test at the
//     gateway, a wget of the listed URL (herd-egress's 403 for this actor without a policy proves DNS and TCP reach
//     it), a ping of the gateway (counted by the table's refused_input);
//   - host: the lease, the TAP (flags, carrier, addresses, counters), the VM's table with its named counters, the
//     bindings file, a DNS query to herd-egress's DNS on the TAP's host address over IPv4, other tables' input
//     chains that drop by policy, and the tail of the VM's serial console (herd-guest's network log lines).
func netDiagnosis(t *testing.T, c *substrateapi.Control, cfg substrateapi.Config, addrs deploy.Addrs, data, listedURL string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	const name = "net-diagnosis"
	logf := func(format string, args ...any) { t.Logf("net-diagnosis: "+format, args...) }
	a, err := c.CreateActor(ctx, &ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: name},
		ActorTemplate: &ateapi.ObjectRef{Atespace: space, Name: cfg.Template}})
	if err != nil {
		logf("CreateActor: %v", err)
		return
	}
	defer func() { _, _ = c.DeleteActor(context.WithoutCancel(ctx), space, name) }()
	_, _ = c.ResumeActor(ctx, space, name)
	for a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		if ctx.Err() != nil {
			logf("actor never RUNNING: %v", a.GetStatus())
			return
		}
		time.Sleep(200 * time.Millisecond)
		if a, err = c.GetActor(ctx, space, name); err != nil {
			logf("GetActor: %v", err)
			return
		}
	}
	vm := a.GetMetadata().GetUid()
	var leases struct {
		Pool   string         `json:"pool"`
		Leases map[string]int `json:"leases"`
	}
	b, err := os.ReadFile(filepath.Join(data, "herdlet", "net", "leases.json"))
	if err == nil {
		err = json.Unmarshal(b, &leases)
	}
	slot, ok := leases.Leases[vm]
	logf("VM %s: lease slot %d (held %v) of pool %s (%v)", vm, slot, ok, leases.Pool, err)
	l := tapnet.Lease{}
	if p, perr := netip.ParsePrefix(leases.Pool); perr == nil && ok {
		l = tapLease(p, slot)
	}
	gw := l.Host.Addr().String()
	logf("link: tap %s mac %s host %s vm %s", l.TAP, l.MAC, l.Host, l.VM)

	host := func(argv ...string) {
		out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
		logf("host$ %s (err %v)\n%s", strings.Join(argv, " "), err, out)
	}
	host("ip", "-d", "-s", "link", "show", "dev", l.TAP)
	host("ip", "addr", "show", "dev", l.TAP)
	host("nft", "list", "table", "inet", "herd_"+strings.ReplaceAll(vm, "-", "_"))
	if b, err := os.ReadFile(filepath.Join(data, "egress-bindings.json")); err == nil {
		logf("bindings file:\n%s", b)
	}
	if out, err := exec.CommandContext(ctx, "nft", "list", "ruleset").Output(); err == nil {
		logf("input chains of other tables that drop by policy: %q", tapnet.InputDrops(string(out)))
	}
	// iptables-legacy rules are invisible to nft list ruleset, and drop just the same.
	host("iptables-legacy", "-S", "INPUT")
	host("iptables", "-S", "INPUT")
	host("sysctl", "net.ipv6.bindv6only", "net.ipv4.conf.all.rp_filter", "net.ipv4.ip_forward")
	if _, port, err := net.SplitHostPort(addrs.DNS); err == nil && gw != "" {
		logf("host DNS query to herd-egress at %s:%s over IPv4: %s", gw, port, dnsQueryV4(net.JoinHostPort(gw, port)))
	}

	u, _ := url.Parse(listedURL)
	script := strings.Join([]string{
		"echo '== ip addr'; ip addr 2>&1 || cat /proc/net/dev",
		"echo '== ip route'; ip route 2>&1 || cat /proc/net/route",
		"echo '== /etc/resolv.conf'; ls -l /etc/resolv.conf; cat /etc/resolv.conf",
		"echo '== libc name service (a glibc image resolves through NSS; nslookup below does not)'; cat /etc/nsswitch.conf 2>&1; ls /lib 2>&1 | head -20",
		"echo '== nslookup " + u.Hostname() + " " + gw + "'; nslookup " + u.Hostname() + " " + gw + " 2>&1",
		"echo '== wget " + listedURL + " (no policy: herd-egress answers 403)'; wget -q -O - -T 5 " + listedURL + " 2>&1",
		"echo '== ping " + gw + " (dropped and counted by refused_input)'; ping -c 1 -W 1 " + gw + " 2>&1",
		"true",
	}, "; ")
	res, err := substrateapi.EnvExec{}.Exec(ctx, cfg.Router, space+"/"+name, "/bin/sh", "-c", script)
	logf("guest$ (exit %d, err %v)\n%s%s", res.ExitCode, err, res.Stdout, res.Stderr)
	host("nft", "list", "table", "inet", "herd_"+strings.ReplaceAll(vm, "-", "_"))

	for _, f := range []string{"serial.log", "console.log"} {
		matches, _ := filepath.Glob(filepath.Join(data, "herdlet", "vms", "*", vm, f))
		for _, m := range matches {
			b, _ := os.ReadFile(m)
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			var keep []string
			for _, ln := range lines {
				if strings.Contains(ln, "herd-guest") || strings.Contains(ln, "virtio_net") || strings.Contains(ln, "eth0") {
					keep = append(keep, ln)
				}
			}
			if len(lines) > 30 {
				lines = lines[len(lines)-30:]
			}
			logf("%s: herd-guest/eth0 lines:\n%s\nlast lines:\n%s", m, strings.Join(keep, "\n"), strings.Join(lines, "\n"))
		}
	}
}

// tapLease is slot s of pool p as tapnet leases it.
func tapLease(p netip.Prefix, s int) tapnet.Lease {
	b := p.Addr().As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) + uint32(s)*4
	at := func(off uint32) netip.Addr {
		x := v + off
		return netip.AddrFrom4([4]byte{byte(x >> 24), byte(x >> 16), byte(x >> 8), byte(x)})
	}
	return tapnet.Lease{Slot: s, TAP: fmt.Sprintf("herd%d", s), MAC: fmt.Sprintf("02:68:64:%02x:%02x:%02x", byte(s>>16), byte(s>>8), byte(s)),
		Host: netip.PrefixFrom(at(1), 30), VM: at(2)}
}

// dnsQueryV4 asks server (host:port) over IPv4 for listed.test's A record and says what came back.
func dnsQueryV4(server string) string {
	q := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0, 6, 'l', 'i', 's', 't', 'e', 'd', 4, 't', 'e', 's', 't', 0, 0, 1, 0, 1}
	c, err := net.DialTimeout("udp4", server, 2*time.Second)
	if err != nil {
		return err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write(q); err != nil {
		return err.Error()
	}
	r := make([]byte, 512)
	n, err := c.Read(r)
	if err != nil {
		return "no answer: " + err.Error()
	}
	if n < len(q)+16 {
		return fmt.Sprintf("answer without a record (%d bytes)", n)
	}
	return "answered " + netip.AddrFrom4([4]byte(r[n-4:n])).String()
}
