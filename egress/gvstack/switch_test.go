package gvstack_test

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/egress/gvstack"
	"github.com/everdict/herd/egress/gvstack/gvvm"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// TestFrameAfterAFailedWrite (criterion no-panic): the VM's link fails a write (the VM's end went away) while
// the switch's reader is still handling a frame it had already read. On the base, gvisor-tap-vsock's switch drops
// the connection on the failed write, the reader puts the VM's MAC back in the switch's address table, and the
// next frame to the VM dereferences a nil connection in tap.(*Switch).txBuf: the test binary panics. Herd's link
// must instead close and tear the stack down, with nothing written after the failure.
func TestFrameAfterAFailedWrite(t *testing.T) {
	link := newFakeLink()
	s, err := gvstack.New(link, egress.StackConfig{Gateway: gwAddr, VM: vmAddr, DNS: egress.SyntheticDNS(gwAddr)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A frame from the VM to itself: the switch learns the VM's MAC and writes the frame back to it.
	link.frames <- hairpin()
	select {
	case <-link.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the switch never wrote to the VM")
	}

	// The VM's end goes away: the next write fails. Two frames are already read off the link: the first one's
	// write fails, the second arrives at the switch after that failure.
	link.fail.Store(true)
	link.frames <- hairpin()
	link.frames <- hairpin()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Accept(ctx); err != net.ErrClosed {
		t.Errorf("Accept after the link failed: %v, want net.ErrClosed", err)
	}
	if n := link.writesAfterFailure.Load(); n > 2 {
		t.Errorf("%d writes reached the failed link, want at most the two frames it was handed", n)
	}
}

// TestCloseWhileFlowsSend (criterion no-panic): the VM's end of a real socket pair closes while herd-egress's end
// of many connections is sending to it, over and over; no frame may reach the switch without a connection.
func TestCloseWhileFlowsSend(t *testing.T) {
	for i := 0; i < 30; i++ {
		s, vm := newPair(t)
		var wg sync.WaitGroup
		ctx, cancel := context.WithCancel(context.Background())
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				f, err := s.Accept(ctx)
				if err != nil {
					return
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					buf := make([]byte, 16<<10)
					for j := 0; j < 64; j++ {
						if _, err := f.Conn.Write(buf); err != nil {
							break
						}
					}
					_ = f.Conn.Close()
				}()
			}
		}()
		for j := 0; j < 8; j++ {
			dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
			c, err := vm.DialTCP(dctx, netip443(j))
			dcancel()
			if err != nil {
				t.Fatalf("round %d: guest dial: %v", i, err)
			}
			go func() {
				b := make([]byte, 4096)
				for {
					if _, err := c.Read(b); err != nil {
						return
					}
				}
			}()
		}
		time.Sleep(time.Duration(i%5) * time.Millisecond)
		vm.Close()
		time.Sleep(20 * time.Millisecond)
		_ = s.Close()
		cancel()
		wg.Wait()
	}
}

// fakeLink is the stack's end of the VM's link. Frames handed to it are read off in order; a frame already handed
// over is still read after Close, as one a reader had taken off the socket before it closed would be.
type fakeLink struct {
	net.Conn
	frames             chan []byte
	sent               chan []byte
	fail               atomic.Bool
	writesAfterFailure atomic.Int64
	closed             chan struct{}
	once               sync.Once
}

func newFakeLink() *fakeLink {
	return &fakeLink{frames: make(chan []byte, 16), sent: make(chan []byte, 16), closed: make(chan struct{})}
}

func (l *fakeLink) Read(b []byte) (int, error) {
	select {
	case f := <-l.frames:
		return copy(b, f), nil
	default:
	}
	select {
	case f := <-l.frames:
		return copy(b, f), nil
	case <-l.closed:
		return 0, net.ErrClosed
	}
}

func (l *fakeLink) Write(b []byte) (int, error) {
	if l.fail.Load() {
		l.writesAfterFailure.Add(1)
		return 0, syscall.ECONNREFUSED
	}
	select {
	case <-l.closed:
		return 0, net.ErrClosed
	default:
	}
	select {
	case l.sent <- append([]byte(nil), b...):
	default:
	}
	return len(b), nil
}

func (l *fakeLink) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeLink) LocalAddr() net.Addr  { return &net.UnixAddr{Name: "stack", Net: "unixgram"} }
func (l *fakeLink) RemoteAddr() net.Addr { return &net.UnixAddr{Name: "vm", Net: "unixgram"} }

// hairpin is an Ethernet frame from the VM's MAC to itself.
func hairpin() []byte {
	b := make([]byte, header.EthernetMinimumSize+46)
	header.Ethernet(b).Encode(&header.EthernetFields{
		SrcAddr: tcpip.LinkAddress(mustMAC(gvvm.MAC)), DstAddr: tcpip.LinkAddress(mustMAC(gvvm.MAC)), Type: header.IPv4ProtocolNumber,
	})
	return b
}

func netip443(j int) netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{203, 0, 113, byte(10 + j)}), 443)
}
