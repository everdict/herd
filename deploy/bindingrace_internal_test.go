package deploy

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/egress/tapnet"
)

// egressUnderTest is a running herd-egress whose -interval is an hour, so nothing it does in a test is the ticker's.
type egressUnderTest struct {
	bindings string
	addr     string
	g        *egress.Gateway

	mu     sync.Mutex
	events []egress.Event
}

func startEgress(t *testing.T) *egressUnderTest {
	t.Helper()
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &egressUnderTest{bindings: filepath.Join(dir, "egress-bindings.json"), addr: ln.Addr().String()}
	gw := make(chan *egress.Gateway, 1)
	eg := &Egress{API: "127.0.0.1:1", Token: "t", Identity: testEgressIdentity(t), Worker: "node-1", Bindings: e.bindings, CAOut: filepath.Join(dir, "ca.pem"),
		DNSAnswer: "198.18.0.1", Interval: time.Hour,
		NewGateway: func(c egress.Config) (*egress.Gateway, error) {
			logged := c.OnEvent
			c.OnEvent = func(ev egress.Event) {
				e.mu.Lock()
				e.events = append(e.events, ev)
				e.mu.Unlock()
				logged(ev)
			}
			g, err := egress.New(c)
			gw <- g
			return g, err
		}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eg.Run(ctx, ln, pc) }()
	t.Cleanup(func() { cancel(); <-done })
	e.g = <-gw
	// Run's first read of the (absent) bindings file is done once its listener accepts; the probe comes from an
	// address no test binds.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.3")}}
		c, err := d.Dial("tcp", e.addr)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("herd-egress never listened: %v", err)
		}
	}
	e.waitEvents(t, 1)
	e.reset()
	return e
}

func (e *egressUnderTest) reset() {
	e.mu.Lock()
	e.events = nil
	e.mu.Unlock()
}

func (e *egressUnderTest) waitEvents(t *testing.T, n int) []egress.Event {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		e.mu.Lock()
		got := append([]egress.Event(nil), e.events...)
		e.mu.Unlock()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("herd-egress decided %d flows, want %d: %+v", len(got), n, got)
		}
	}
}

// flow opens one connection to herd-egress's transparent listener from local address from and waits for its decision.
func (e *egressUnderTest) flow(t *testing.T, from string) egress.Event {
	t.Helper()
	e.reset()
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(from)}, Timeout: 2 * time.Second}
	c, err := d.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return e.waitEvents(t, 1)[0]
}

const unboundReason = "bound to no actor"

var raceActor = tapnet.Binding{Space: "s", Name: "box", UID: "u1", Source: "127.0.0.1"}

// TestBindingWrittenJustBeforeFirstFlowIsServed (bound-before-first-flow, DEFAUL-1622): herdlet writes a restored VM's
// binding and the guest connects at once, long before herd-egress's next -interval re-read. The flow is the actor's:
// herd-egress re-reads the bindings before it refuses an address it does not know. Its watch of the file is an hour
// here too, so only that re-read can serve the flow.
func TestBindingWrittenJustBeforeFirstFlowIsServed(t *testing.T) {
	poll := bindingsPoll
	t.Cleanup(func() { bindingsPoll = poll }) // after herd-egress stopped
	bindingsPoll = time.Hour
	e := startEgress(t)
	if err := os.WriteFile(e.bindings, []byte(`[{"space":"s","name":"box","uid":"u1","source":"127.0.0.1"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := e.flow(t, "127.0.0.1")
	if strings.Contains(ev.Reason, unboundReason) || ev.Actor != (egress.Actor{Space: "s", Name: "box", UID: "u1"}) {
		t.Fatalf("first flow after the binding was written: actor %+v, reason %q; want it decided as s/box's", ev.Actor, ev.Reason)
	}
}

// TestHerdletBindsOnlyOnceHerdEgressHasIt (bound-before-first-flow, DEFAUL-1622): herdlet's binding (tapnet.Bindings.Set,
// which a VM's network Up runs before its restore or boot) answers only once herd-egress has loaded it, so the guest
// cannot send before herd-egress knows its address.
func TestHerdletBindsOnlyOnceHerdEgressHasIt(t *testing.T) {
	e := startEgress(t)
	b := &tapnet.Bindings{Path: e.bindings}
	if err := b.Set(raceActor); err != nil {
		t.Fatal(err)
	}
	if got := e.g.Actors(); len(got) != 1 || got[0] != (egress.Actor{Space: "s", Name: "box", UID: "u1"}) {
		t.Fatalf("herd-egress holds %+v when the binding answered; want s/box", got)
	}
	ev := e.flow(t, "127.0.0.1")
	if strings.Contains(ev.Reason, unboundReason) {
		t.Fatalf("first flow after the binding answered: %q", ev.Reason)
	}
}

// TestUnboundAddressStillRefused (bound-before-first-flow, DEFAUL-1622): an address the bindings file does not hold is
// refused, every time, after the re-read.
func TestUnboundAddressStillRefused(t *testing.T) {
	e := startEgress(t)
	if err := os.WriteFile(e.bindings, []byte(`[{"space":"s","name":"box","uid":"u1","source":"127.0.0.1"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if ev := e.flow(t, "127.0.0.2"); !strings.Contains(ev.Reason, unboundReason) || ev.Allowed {
			t.Fatalf("flow %d from unbound 127.0.0.2: allowed %v, actor %+v, reason %q; want refused as bound to no actor", i, ev.Allowed, ev.Actor, ev.Reason)
		}
	}
}

// TestUnknownAddressReadsOnlyAChangedFile (bound-before-first-flow, DEFAUL-1622): the re-read before a refusal is
// bounded: flows from an address the file does not hold read it again only if it changed since the last load, so an
// unbound address costs a stat per flow, not a read.
func TestUnknownAddressReadsOnlyAChangedFile(t *testing.T) {
	guard, err := egress.NewGuard(egress.GuardConfig{})
	if err != nil {
		t.Fatal(err)
	}
	g, err := egress.New(egress.Config{Guard: guard})
	if err != nil {
		t.Fatal(err)
	}
	var src egress.Sources
	path := filepath.Join(t.TempDir(), "egress-bindings.json")
	f := &bindingsFile{path: path, g: g, src: &src}
	f.reload(true)
	stranger := netip.MustParseAddr("10.88.0.6")
	for i := 0; i < 100; i++ {
		f.unknown(stranger)
	}
	if f.reads != 1 {
		t.Fatalf("100 flows from an unbound address with the file unchanged read it %d times, want only the first load", f.reads)
	}
	if err := os.WriteFile(path, []byte(`[{"space":"s","name":"box","uid":"u1","source":"10.88.0.2"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	f.unknown(stranger)
	if a, ok := src.Actor(netip.MustParseAddr("10.88.0.2")); !ok || a.Name != "box" || f.reads != 2 {
		t.Fatalf("after the file changed: 10.88.0.2 is %+v (%v), %d reads; want s/box after one more read", a, ok, f.reads)
	}
	if _, ok := src.Actor(stranger); ok {
		t.Fatalf("%v is bound after the re-read", stranger)
	}
	for i := 0; i < 100; i++ {
		f.unknown(stranger)
	}
	if f.reads != 2 {
		t.Fatalf("flows from an unbound address read the unchanged file again: %d reads", f.reads)
	}
}
