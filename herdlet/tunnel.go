package herdlet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/everdict/herd/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// GuestDialer connects to a port in a VM's guest: hybrid vsock under Cloud Hypervisor, native vsock under
// Virtualization.framework, a unix socket for the fake hypervisor. It is the hardware boundary of the tunnel.
type GuestDialer interface {
	DialGuest(ctx context.Context, vm string, port uint32) (net.Conn, error)
}

// Tunnel is the herdlet's side of its tunnel to herd-router (package tunnel): the herdlet dials the router, and
// every connection the router asks for is a new outbound stream carrying the bytes of one guest connection.
type Tunnel struct {
	Worker string
	Guests GuestDialer
	// DialTimeout bounds connecting to the guest (default 5 s).
	DialTimeout time.Duration

	mu      sync.Mutex
	closing map[string]tunnel.Frame // VM → the cut its streams got; new opens are refused with it
	streams map[string]map[*guestStream]struct{}
	opened  atomic.Int64
}

// Opened counts the guest connections this tunnel carried.
func (t *Tunnel) Opened() int64 { return t.opened.Load() }

// Close cuts every stream to vm with reason and refuses new ones until Reopen.
func (t *Tunnel) Close(vm string, reason tunnel.Reason, detail string) {
	cut := tunnel.Frame{Kind: tunnel.FrameCut, Reason: reason, Detail: detail}
	t.mu.Lock()
	if t.closing == nil {
		t.closing = map[string]tunnel.Frame{}
	}
	t.closing[vm] = cut
	var cutNow []*guestStream
	for g := range t.streams[vm] {
		cutNow = append(cutNow, g)
	}
	t.mu.Unlock()
	for _, g := range cutNow {
		g.end(&cut)
	}
}

// Reopen lets the router open streams to vm again (it booted or was restored).
func (t *Tunnel) Reopen(vm string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.closing, vm)
}

// Run dials nothing itself: it opens the control stream on cc (a connection the herdlet dialed to the router),
// introduces the worker and serves Opens until ctx ends or the tunnel breaks. A guest stream it opened outlives the
// control stream (make-before-break: a newer tunnel takes the Opens while the older one's streams finish, ADR 0001
// A-60) and ends with ctx or its own stream; Run returns once they have.
func (t *Tunnel) Run(ctx context.Context, cc grpc.ClientConnInterface) error {
	outer := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cs, err := cc.NewStream(ctx, &tunnel.ControlStreamDesc, tunnel.ControlMethod, grpc.CallContentSubtype(tunnel.ControlCodec))
	if err != nil {
		return fmt.Errorf("herdlet: tunnel: %w", err)
	}
	var sendMu sync.Mutex
	send := func(u tunnel.Up) error {
		b, err := json.Marshal(u)
		if err != nil {
			return err
		}
		sendMu.Lock()
		defer sendMu.Unlock()
		return cs.SendMsg(json.RawMessage(b))
	}
	if err := send(tunnel.Up{Hello: &tunnel.Hello{Worker: t.Worker}}); err != nil {
		return fmt.Errorf("herdlet: tunnel hello: %w", err)
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	for {
		var m json.RawMessage
		if err := cs.RecvMsg(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("herdlet: tunnel: %w", err)
		}
		var d tunnel.Down
		if err := json.Unmarshal(m, &d); err != nil {
			return fmt.Errorf("herdlet: tunnel decode: %w", err)
		}
		if d.Open == nil {
			continue
		}
		o := *d.Open
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := t.open(outer, cc, o); r != nil {
				_ = send(tunnel.Up{Refused: r})
			}
		}()
	}
}

// open connects to the guest and carries the connection on a new data stream; a refusal says why it could not.
func (t *Tunnel) open(ctx context.Context, cc grpc.ClientConnInterface, o tunnel.Open) *tunnel.Refused {
	t.mu.Lock()
	cut, closing := t.closing[o.VM]
	t.mu.Unlock()
	if closing {
		return &tunnel.Refused{ID: o.ID, Reason: cut.Reason, Detail: cut.Detail}
	}
	if t.Guests == nil {
		return &tunnel.Refused{ID: o.ID, Reason: tunnel.ReasonGuestUnreachable, Detail: "herdlet has no guest dialer"}
	}
	timeout := t.DialTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	dctx, dcancel := context.WithTimeout(ctx, timeout)
	guest, err := t.Guests.DialGuest(dctx, o.VM, o.Port)
	dcancel()
	if err != nil {
		return &tunnel.Refused{ID: o.ID, Reason: tunnel.ReasonGuestUnreachable, Detail: fmt.Sprintf("VM %s port %d: %v", o.VM, o.Port, err)}
	}
	sctx, scancel := context.WithCancel(ctx)
	cs, err := cc.NewStream(metadata.AppendToOutgoingContext(sctx, tunnel.IDHeader, o.ID), &tunnel.DataStreamDesc, tunnel.DataMethod,
		grpc.CallContentSubtype(tunnel.FrameCodecName))
	if err != nil {
		scancel()
		guest.Close()
		return &tunnel.Refused{ID: o.ID, Reason: tunnel.ReasonNodeUnreachable, Detail: err.Error()}
	}
	g := &guestStream{cs: cs, guest: guest, cancel: scancel}
	t.mu.Lock()
	if t.streams == nil {
		t.streams = map[string]map[*guestStream]struct{}{}
	}
	if t.streams[o.VM] == nil {
		t.streams[o.VM] = map[*guestStream]struct{}{}
	}
	t.streams[o.VM][g] = struct{}{}
	cut, closing = t.closing[o.VM]
	t.mu.Unlock()
	t.opened.Add(1)
	if closing {
		// Close ran between the check above and the registration.
		g.end(&cut)
	}
	g.pump()
	t.mu.Lock()
	delete(t.streams[o.VM], g)
	if len(t.streams[o.VM]) == 0 {
		delete(t.streams, o.VM)
	}
	t.mu.Unlock()
	return nil
}

// guestStream is one guest connection on its data stream.
type guestStream struct {
	cs     grpc.ClientStream
	guest  net.Conn
	cancel context.CancelFunc

	mu    sync.Mutex // guards sends on cs and ended
	ended bool
}

var errEnded = errors.New("herdlet: tunnel stream ended")

func (g *guestStream) send(f *tunnel.Frame) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ended {
		return errEnded
	}
	return g.cs.SendMsg(f)
}

// end finishes the herdlet's half: the cut frame (if any) goes first, then the half-close, so the router reads the
// reason before the end. The guest connection is closed; the stream is torn down if the router does not end its
// half in time.
func (g *guestStream) end(cut *tunnel.Frame) {
	g.mu.Lock()
	if g.ended {
		g.mu.Unlock()
		return
	}
	g.ended = true
	if cut != nil {
		_ = g.cs.SendMsg(cut)
	}
	_ = g.cs.CloseSend()
	g.mu.Unlock()
	g.guest.Close()
	time.AfterFunc(closeWait, g.cancel)
}

const closeWait = 5 * time.Second

// pump copies guest → router in a goroutine and router → guest here, and returns when the stream is over.
func (g *guestStream) pump() {
	defer g.cancel()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := g.guest.Read(buf)
			if n > 0 {
				if g.send(&tunnel.Frame{Kind: tunnel.FrameData, Data: buf[:n]}) != nil {
					return
				}
			}
			if err != nil {
				g.end(nil)
				return
			}
		}
	}()
	for {
		var f tunnel.Frame
		if err := g.cs.RecvMsg(&f); err != nil {
			g.end(nil)
			return
		}
		switch f.Kind {
		case tunnel.FrameData:
			if _, err := g.guest.Write(f.Data); err != nil {
				g.end(nil)
			}
		case tunnel.FrameClose:
			g.end(nil)
		}
	}
}
