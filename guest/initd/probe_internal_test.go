package initd

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/everdict/herd/egress"
)

// TestProbeDNSAgainstHerdEgress: herd-guest's boot probe reads herd-egress's synthetic answer, and says so when
// nothing answers.
func TestProbeDNSAgainstHerdEgress(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = egress.ServeDNS(ctx, pc, egress.SyntheticDNS(netip.MustParseAddr("198.18.0.1"))) }()
	a, err := probeDNS(pc.LocalAddr().String(), "herd-guest.probe", 2*time.Second)
	if err != nil || a != netip.MustParseAddr("198.18.0.1") {
		t.Fatalf("probe: %v %v, want 198.18.0.1", a, err)
	}
	silent, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	if _, err := probeDNS(silent.LocalAddr().String(), "herd-guest.probe", 200*time.Millisecond); err == nil {
		t.Error("a resolver that does not answer probed fine")
	}
}
