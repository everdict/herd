// Package nfttest is a fake nft.Host: it keeps the TAPs and nftables tables the commands of egress/nft create and
// remove, and answers ip(8) and nft(8) as they do, so the network's lifecycle is tested without root.
package nfttest

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/everdict/herd/egress/nft"
)

// Host is the fake. Its zero value is ready.
type Host struct {
	mu     sync.Mutex
	taps   map[string]bool
	up     map[string]bool
	tables map[string]bool
	// Log is every command run, in order (nft.Command.String, with the loaded table's name after "nft -f -").
	Log []string
	// KeepTAP makes "ip link del" answer success and remove nothing (a TAP left after delete).
	KeepTAP bool
	// KeepTable makes "nft delete table" answer success and remove nothing.
	KeepTable bool
	// Blind makes every "show"/"list" fail without saying whether the object exists (no answer).
	Blind bool
	// FailApply, if set, fails the command whose String has this prefix.
	FailApply string
	// Detached makes every TAP read as one no hypervisor holds open (no LOWER_UP: "NO-CARRIER ... state DOWN").
	Detached bool
	// Ruleset is what "nft list ruleset" answers besides the herd tables.
	Ruleset string
	// KeepHoles makes deleting an inserted accept answer success and remove nothing.
	KeepHoles bool

	holes  []fakeHole
	handle int
	// flows counts conntrack entries by their original source address (flows a VM opened).
	flows map[string]int
	// KeepFlows makes "conntrack -D" report entries deleted and remove none (a kernel that recreates them at once).
	KeepFlows bool
	// Deleted counts the conntrack entries "conntrack -D" reported deleted.
	Deleted int
	// NoConntrack makes conntrack(8) missing from the host.
	NoConntrack bool
}

// AddFlow records a conntrack entry of a flow from addr, as one the kernel tracked before the VM's table existed.
func (h *Host) AddFlow(addr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.flows == nil {
		h.flows = map[string]int{}
	}
	h.flows[addr]++
}

// Flows is how many conntrack entries are of flows from addr.
func (h *Host) Flows(addr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.flows[addr]
}

var _ nft.Host = (*Host)(nil)

var tableRE = regexp.MustCompile(`(?m)^table inet (\S+) \{`)

func (h *Host) init() {
	if h.taps == nil {
		h.taps, h.up, h.tables = map[string]bool{}, map[string]bool{}, map[string]bool{}
	}
}

// Run implements nft.Host.
func (h *Host) Run(_ context.Context, c nft.Command) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	s := c.String()
	if m := tableRE.FindStringSubmatch(c.Stdin); m != nil {
		s += " " + m[1]
	}
	h.Log = append(h.Log, s)
	if h.FailApply != "" && strings.HasPrefix(s, h.FailApply) {
		return []byte("injected failure"), fmt.Errorf("%s: injected failure", s)
	}
	fail := func(msg string) ([]byte, error) { return []byte(msg), fmt.Errorf("%s: exit status 1: %s", s, msg) }
	a := c.Args
	switch {
	case c.Name == "ip" && len(a) >= 4 && a[0] == "tuntap" && a[1] == "add":
		if h.taps[a[3]] {
			return fail("ioctl(TUNSETIFF): Device or resource busy")
		}
		h.taps[a[3]] = true
	case c.Name == "ip" && len(a) == 5 && a[0] == "addr" && a[1] == "add":
		if !h.taps[a[4]] {
			return fail(fmt.Sprintf("Cannot find device %q", a[4]))
		}
	case c.Name == "ip" && len(a) == 4 && a[0] == "link" && a[1] == "set" && a[3] == "up":
		if !h.taps[a[2]] {
			return fail(fmt.Sprintf("Cannot find device %q", a[2]))
		}
		h.up[a[2]] = true
	case c.Name == "ip" && len(a) == 3 && a[0] == "link" && a[1] == "del":
		if !h.taps[a[2]] {
			return fail(fmt.Sprintf("Cannot find device %q", a[2]))
		}
		if !h.KeepTAP {
			delete(h.taps, a[2])
			delete(h.up, a[2])
		}
	case c.Name == "ip" && len(a) == 4 && a[0] == "link" && a[1] == "show":
		if h.Blind {
			return fail("RTNETLINK answers: Operation not permitted")
		}
		if !h.taps[a[3]] {
			return fail(fmt.Sprintf("Device %q does not exist.", a[3]))
		}
		flags := "BROADCAST,MULTICAST"
		switch {
		case h.up[a[3]] && !h.Detached:
			flags += ",UP,LOWER_UP"
		case h.up[a[3]]:
			flags = "NO-CARRIER," + flags + ",UP"
		}
		return []byte(fmt.Sprintf("7: %s: <%s> mtu 1500 qdisc fq_codel state DOWN mode DEFAULT group default qlen 1000\n", a[3], flags)), nil
	case c.Name == "nft" && (len(a) == 2 && a[0] == "list" && a[1] == "ruleset" || len(a) == 3 && a[0] == "-a" && a[1] == "list" && a[2] == "ruleset"):
		if h.Blind {
			return fail("Error: cache initialization failed: Operation not permitted")
		}
		out := h.Ruleset
		for t := range h.tables {
			out += "table inet " + t + " {\n}\n"
		}
		for _, o := range h.holes {
			f := strings.Fields(o.chain)
			out += fmt.Sprintf("table %s %s {\n\tchain %s {\n\t\t%s # handle %d\n\t}\n}\n", f[0], f[1], f[2], o.rule, o.handle)
		}
		return []byte(out), nil
	case c.Name == "nft" && len(a) == 2 && a[0] == "-f" && (strings.HasPrefix(c.Stdin, "insert rule ") || strings.HasPrefix(c.Stdin, "delete rule ")):
		// One transaction: every line applies, or none.
		holes := append([]fakeHole(nil), h.holes...)
		next := h.handle
		for _, line := range strings.Split(strings.TrimSpace(c.Stdin), "\n") {
			f := strings.Fields(line)
			switch {
			case len(f) > 5 && f[0] == "insert" && f[1] == "rule":
				next++
				holes = append(holes, fakeHole{handle: next, chain: strings.Join(f[2:5], " "), rule: strings.Join(f[5:], " ")})
			case len(f) == 7 && f[0] == "delete" && f[1] == "rule" && f[5] == "handle":
				found := false
				for i, o := range holes {
					if fmt.Sprint(o.handle) == f[6] && o.chain == strings.Join(f[2:5], " ") {
						if !h.KeepHoles {
							holes = append(holes[:i], holes[i+1:]...)
						}
						found = true
						break
					}
				}
				if !found {
					return fail("Error: Could not process rule: No such file or directory")
				}
			default:
				return fail("syntax error: " + line)
			}
		}
		h.holes, h.handle = holes, next
	case c.Name == "nft" && len(a) == 2 && a[0] == "-f":
		m := tableRE.FindStringSubmatch(c.Stdin)
		if m == nil {
			return fail("syntax error")
		}
		if h.tables[m[1]] {
			return fail("File exists")
		}
		h.tables[m[1]] = true
	case c.Name == "nft" && len(a) == 4 && a[0] == "delete" && a[1] == "table":
		if !h.tables[a[3]] {
			return fail("Error: Could not process rule: No such file or directory")
		}
		if !h.KeepTable {
			delete(h.tables, a[3])
		}
	case c.Name == "nft" && len(a) == 4 && a[0] == "list" && a[1] == "table":
		if h.Blind {
			return fail("Error: cache initialization failed: Operation not permitted")
		}
		if !h.tables[a[3]] {
			return fail("Error: Could not process rule: No such file or directory")
		}
	case c.Name == "conntrack" && len(a) == 3 && (a[0] == "-D" || a[0] == "-L") && (a[1] == "-s" || a[1] == "-d"):
		// conntrack(8) as conntrack-tools 1.4 answers: the entries on stdout, the count on stderr; a delete that
		// matched nothing exits 1.
		if h.NoConntrack {
			return []byte(""), fmt.Errorf("exec: \"conntrack\": executable file not found in $PATH")
		}
		n := 0
		if a[1] == "-s" {
			n = h.flows[a[2]]
		}
		if a[0] == "-L" {
			out := strings.Repeat(fmt.Sprintf("udp      17 29 src=%s dst=1.1.1.1 sport=40000 dport=53 [UNREPLIED] src=1.1.1.1 dst=%s sport=53 dport=40000 mark=0 use=1\n", a[2], a[2]), n)
			return []byte(out + fmt.Sprintf("conntrack v1.4.6 (conntrack-tools): %d flow entries have been shown.\n", n)), nil
		}
		if n == 0 {
			return fail("conntrack v1.4.6 (conntrack-tools): 0 flow entries have been deleted.")
		}
		if !h.KeepFlows {
			delete(h.flows, a[2])
		}
		h.Deleted += n
		return []byte(fmt.Sprintf("conntrack v1.4.6 (conntrack-tools): %d flow entries have been deleted.\n", n)), nil
	default:
		return fail("nfttest: unknown command")
	}
	return nil, nil
}

// TAPs is the TAPs that exist, and whether each is up.
func (h *Host) TAPs() map[string]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	out := map[string]bool{}
	for t := range h.taps {
		out[t] = h.up[t]
	}
	return out
}

// Tables is the tables that exist.
func (h *Host) Tables() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for t := range h.tables {
		out = append(out, t)
	}
	return out
}

// Commands is a copy of Log.
func (h *Host) Commands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.Log...)
}

type fakeHole struct {
	handle      int
	chain, rule string
}

// Holes is every rule inserted into another table's chain, "<family> <table> <chain>: <rule>".
func (h *Host) Holes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, o := range h.holes {
		out = append(out, o.chain+": "+o.rule)
	}
	return out
}
