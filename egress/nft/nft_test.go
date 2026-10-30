package nft_test

import (
	"context"
	"errors"
	"flag"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/everdict/herd/egress/nft"
)

var update = flag.Bool("update", false, "rewrite golden files")

func vm1() nft.VM {
	return nft.VM{ID: "a1b2c3", TAP: "herd-a1b2c3", Host: netip.MustParsePrefix("10.88.0.1/30"), VM: netip.MustParseAddr("10.88.0.2"),
		ProxyPort: 15001, DNSPort: 15053}
}

// TestRulesetGolden (criterion platform-plumbing): the rendered per-VM table, byte for byte.
func TestRulesetGolden(t *testing.T) {
	got, err := nft.Ruleset(vm1())
	if err != nil {
		t.Fatal(err)
	}
	const golden = "testdata/vm-a1b2c3.nft"
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("ruleset differs from %s (-update to rewrite):\n%s", golden, got)
	}
}

// TestRulesetDefaultDeny checks the properties the golden file must keep, so an edit to it cannot quietly open the
// TAP: every chain that sees the TAP ends in a drop, the only accepts are redirected flows to herd-egress's two ports
// on the host's TAP address, and nothing is forwarded.
func TestRulesetDefaultDeny(t *testing.T) {
	rs, err := nft.Ruleset(vm1())
	if err != nil {
		t.Fatal(err)
	}
	chains := map[string][]string{}
	var cur string
	for _, l := range strings.Split(rs, "\n") {
		l = strings.TrimSpace(l)
		if c, ok := strings.CutPrefix(l, "chain "); ok {
			cur = strings.TrimSuffix(c, " {")
			continue
		}
		if l == "}" {
			cur = ""
		}
		if cur != "" && l != "" {
			chains[cur] = append(chains[cur], l)
		}
	}
	last := func(c string) string { r := chains[c]; return r[len(r)-1] }
	for _, c := range []string{"input", "forward", "output"} {
		if !strings.HasSuffix(last(c), "drop") {
			t.Errorf("chain %s does not end in a drop: %q", c, last(c))
		}
	}
	for c, rules := range chains {
		for _, r := range rules {
			if !strings.HasSuffix(r, "accept") {
				continue
			}
			if strings.HasPrefix(r, "type ") {
				continue
			}
			switch {
			case c == "input" && strings.Contains(r, "ct status dnat") && strings.Contains(r, "established,related"):
			case c == "input" && strings.Contains(r, "ct status dnat ip daddr 10.88.0.1 tcp dport 15001"):
			case c == "input" && strings.Contains(r, "ct status dnat ip daddr 10.88.0.1 udp dport 15053"):
			case c == "output" && strings.Contains(r, `oifname "herd-a1b2c3" ct state established,related`):
			default:
				t.Errorf("unexpected accept in chain %s: %q", c, r)
			}
		}
	}
	for _, r := range chains["forward"] {
		if strings.Contains(r, "accept") && !strings.HasPrefix(r, "type ") {
			t.Errorf("forward accepts: %q", r)
		}
	}
	if !strings.Contains(strings.Join(chains["prerouting"], "\n"), `meta l4proto tcp redirect to :15001`) {
		t.Errorf("TCP is not redirected to herd-egress")
	}
	if !strings.Contains(strings.Join(chains["screen"], "\n"), `ip saddr != 10.88.0.2`) {
		t.Errorf("no anti-spoofing")
	}
}

func TestRulesetInvalid(t *testing.T) {
	for name, mut := range map[string]func(*nft.VM){
		"id":         func(v *nft.VM) { v.ID = "A;drop" },
		"tap":        func(v *nft.VM) { v.TAP = "tap\" accept #" },
		"tap long":   func(v *nft.VM) { v.TAP = "a-very-long-tap-name" },
		"vm outside": func(v *nft.VM) { v.VM = netip.MustParseAddr("10.88.1.2") },
		"vm is host": func(v *nft.VM) { v.VM = netip.MustParseAddr("10.88.0.1") },
		"ipv6":       func(v *nft.VM) { v.Host = netip.MustParsePrefix("fd00::1/64"); v.VM = netip.MustParseAddr("fd00::2") },
		"no port":    func(v *nft.VM) { v.ProxyPort = 0 },
	} {
		v := vm1()
		mut(&v)
		if _, err := nft.Ruleset(v); !errors.Is(err, nft.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

type fakeHost struct {
	ran    []nft.Command
	failAt int
}

func (f *fakeHost) Run(_ context.Context, c nft.Command) ([]byte, error) {
	f.ran = append(f.ran, c)
	if len(f.ran) == f.failAt {
		return nil, errors.New("boom")
	}
	return nil, nil
}

func TestApplyOrder(t *testing.T) {
	h := &fakeHost{}
	if err := nft.Apply(context.Background(), h, vm1(), 1000); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range h.ran {
		got = append(got, c.String())
	}
	want := []string{
		"ip tuntap add dev herd-a1b2c3 mode tap user 1000",
		"ip addr add 10.88.0.1/30 dev herd-a1b2c3",
		"nft -f - <<ruleset",
		"ip link set herd-a1b2c3 up",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("plan:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A failure after the TAP exists removes the TAP and the table again.
	h = &fakeHost{failAt: 3}
	if err := nft.Apply(context.Background(), h, vm1(), -1); err == nil {
		t.Fatal("Apply with a failing nft answered nil")
	}
	tail := h.ran[3:]
	if len(tail) != 2 || tail[0].String() != "ip link del herd-a1b2c3" || tail[1].String() != "nft delete table inet herd_a1b2c3" {
		t.Errorf("cleanup after failure: %v", tail)
	}
}

type cannedHost struct{ out string }

func (c cannedHost) Run(context.Context, nft.Command) ([]byte, error) { return []byte(c.out), nil }

// TestHoleRulesAndHoles: the accepts herd inserts name only the TAP, the host's address and herd-egress's two ports
// (iptables-nft can show them), and are found again by their comment in "nft -a list ruleset" as nft prints it.
func TestHoleRulesAndHoles(t *testing.T) {
	v := vm1()
	got := strings.Join(nft.HoleRules(v), "\n")
	want := `iifname "herd-a1b2c3" ip daddr 10.88.0.1 tcp dport 15001 accept comment "herd-egress:herd_a1b2c3"` + "\n" +
		`iifname "herd-a1b2c3" ip daddr 10.88.0.1 udp dport 15053 accept comment "herd-egress:herd_a1b2c3"`
	if got != want {
		t.Errorf("HoleRules:\n%s\nwant:\n%s", got, want)
	}
	listed := `table ip filter { # handle 3
	chain INPUT { # handle 1
		type filter hook input priority filter; policy drop;
		iifname "herd-a1b2c3" ip daddr 10.88.0.1 udp dport 15053 counter packets 4 bytes 240 accept comment "herd-egress:herd_a1b2c3" # handle 91
		iifname "herd-a1b2c3" ip daddr 10.88.0.1 tcp dport 15001 counter packets 0 bytes 0 accept comment "herd-egress:herd_a1b2c3" # handle 90
		iifname "herd9" ip daddr 10.88.0.37 tcp dport 15001 accept comment "herd-egress:herd_other" # handle 88
		counter packets 10 bytes 600 jump ufw-before-input # handle 7
	}
}
table inet firewalld { # handle 5
	chain filter_INPUT { # handle 2
		type filter hook input priority filter + 10; policy accept;
		iifname "herd-a1b2c3" ip daddr 10.88.0.1 tcp dport 15001 accept comment "herd-egress:herd_a1b2c3" # handle 12
	}
}
`
	holes, err := nft.Holes(context.Background(), cannedHost{listed}, v)
	if err != nil {
		t.Fatal(err)
	}
	var hs []string
	for _, h := range holes {
		hs = append(hs, h.Chain.String()+" #"+h.Handle)
	}
	if g, w := strings.Join(hs, ", "), "ip filter INPUT #91, ip filter INPUT #90, inet firewalld filter_INPUT #12"; g != w {
		t.Errorf("Holes = %s, want %s", g, w)
	}
	if cs := nft.InputDropChains(listed); len(cs) != 1 || cs[0].String() != "ip filter INPUT" {
		t.Errorf("InputDropChains = %v", cs)
	}
}
