package tapnet_test

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/everdict/herd/egress/nft/nfttest"
)

// logOf captures the log while f runs.
func logOf(t *testing.T, f func()) string {
	t.Helper()
	var b bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&b)
	defer func() { log.SetOutput(prev); log.SetFlags(flags) }()
	f()
	return b.String()
}

// TestUpFlushesTheVMsConntrack (DEFAUL-1622, A-59): a conntrack entry for the VM's address that the kernel made
// before the VM's table existed carries the guest's DNS past the table's redirect, and the input chain, which accepts
// only `ct status dnat`, drops it. Up removes every entry naming the VM's address once the table is loaded, before the
// guest can send (Up runs before every Boot and Restore), and checks that none remains. This is a unit test of the
// command sequence on the fake host (nfttest), not a network namespace: conntrack(8) is answered as conntrack-tools
// answers it.
func TestUpFlushesTheVMsConntrack(t *testing.T) {
	h := &nfttest.Host{}
	n := newNet(t, h)
	ctx := context.Background()
	g, _, err := n.Lease(ctx, "vm-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	vm := strings.Split(g.Address, "/")[0]
	h.AddFlow(vm)
	h.AddFlow(vm)
	h.AddFlow("10.88.250.2") // another VM's: not this Up's to touch
	if err := n.Up(ctx, "vm-1"); err != nil {
		t.Fatal(err)
	}
	if left := h.Flows(vm); left != 0 {
		t.Fatalf("%d conntrack entries of %s remain after Up; want none before the guest sends", left, vm)
	}
	if h.Flows("10.88.250.2") != 1 {
		t.Errorf("Up removed another address's conntrack entries")
	}
	loaded, flushed := -1, -1
	for i, c := range h.Commands() {
		switch {
		case strings.HasPrefix(c, "nft -f - <<ruleset herd_"):
			loaded = i
		case strings.HasPrefix(c, "conntrack -D -s "+vm) && flushed < 0:
			flushed = i
		}
	}
	if loaded < 0 || flushed < loaded {
		t.Errorf("commands %v: want the VM's conntrack entries flushed after its table is loaded", h.Commands())
	}
}

// TestUpLogsConntrackItCannotClear (A-59): entries that remain after the flush, or a host without conntrack(8), are
// logged with the VM's address; Up still answers, since the table itself is in force.
func TestUpLogsConntrackItCannotClear(t *testing.T) {
	for _, c := range []struct {
		name string
		host *nfttest.Host
		want string
	}{
		{"entries remain", &nfttest.Host{KeepFlows: true}, "2 conntrack entries"},
		{"no conntrack", &nfttest.Host{NoConntrack: true}, "conntrack"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := newNet(t, c.host)
			ctx := context.Background()
			g, _, err := n.Lease(ctx, "vm-1", nil)
			if err != nil {
				t.Fatal(err)
			}
			vm := strings.Split(g.Address, "/")[0]
			c.host.AddFlow(vm)
			c.host.AddFlow(vm)
			out := logOf(t, func() {
				if err := n.Up(ctx, "vm-1"); err != nil {
					t.Fatalf("Up: %v", err)
				}
			})
			if !strings.Contains(out, c.want) || !strings.Contains(out, vm) || !strings.Contains(out, "not cleared") {
				t.Errorf("log after Up:\n%s\nwant a line saying the conntrack entries of %s are not cleared (%q)", out, vm, c.want)
			}
		})
	}
}
