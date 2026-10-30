// Package stacktest is an in-memory egress.Stack: what a VM on a macOS host sends through its file-handle network
// device, as gvisor-tap-vsock wired for herd-egress would deliver it. It keeps the Stack contract: every TCP flow
// goes to Accept, DNS to the gateway address is answered by the configured DNS function, and every other packet
// (UDP elsewhere, ICMP, IPv6) is dropped and counted.
package stacktest

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"

	"github.com/everdict/herd/egress"
)

// Stack is the fake. The VM side is DialTCP, SendUDP and SendOther.
type Stack struct {
	cfg     egress.StackConfig
	flows   chan egress.Flow
	done    chan struct{}
	once    sync.Once
	dropped atomic.Int64
}

// Factory makes fake stacks; it implements egress.StackFactory.
type Factory struct {
	mu     sync.Mutex
	Stacks map[egress.Actor]*Stack
}

// NewStack implements egress.StackFactory. The fake has no device file: it returns a nil *os.File.
func (f *Factory) NewStack(a egress.Actor, cfg egress.StackConfig) (egress.Stack, *os.File, error) {
	if cfg.DNS == nil || !cfg.Gateway.IsValid() {
		return nil, nil, errors.New("stacktest: StackConfig needs Gateway and DNS")
	}
	s := New(cfg)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Stacks == nil {
		f.Stacks = map[egress.Actor]*Stack{}
	}
	f.Stacks[a] = s
	return s, nil, nil
}

// New makes one fake stack.
func New(cfg egress.StackConfig) *Stack {
	return &Stack{cfg: cfg, flows: make(chan egress.Flow), done: make(chan struct{})}
}

// Accept implements egress.Stack.
func (s *Stack) Accept(ctx context.Context) (egress.Flow, error) {
	select {
	case f := <-s.flows:
		return f, nil
	case <-s.done:
		return egress.Flow{}, net.ErrClosed
	case <-ctx.Done():
		return egress.Flow{}, ctx.Err()
	}
}

// Dropped implements egress.Stack.
func (s *Stack) Dropped() int64 { return s.dropped.Load() }

// Close implements egress.Stack.
func (s *Stack) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

// DialTCP is the VM opening a TCP connection to dst: the stack terminates it and hands herd-egress the other end.
func (s *Stack) DialTCP(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
	vm, gw := net.Pipe()
	select {
	case s.flows <- egress.Flow{Conn: gw, Dst: dst}:
		return vm, nil
	case <-s.done:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SendUDP is the VM sending one datagram. A DNS query to the gateway's port 53 is answered; anything else is
// dropped (ok false).
func (s *Stack) SendUDP(dst netip.AddrPort, payload []byte) (reply []byte, ok bool) {
	if dst.Addr() == s.cfg.Gateway && dst.Port() == 53 {
		r, err := s.cfg.DNS(payload)
		if err == nil {
			return r, true
		}
	}
	s.dropped.Add(1)
	return nil, false
}

// SendOther is the VM sending a packet that is neither TCP nor UDP (ICMP, or anything over IPv6): dropped.
func (s *Stack) SendOther() {
	s.dropped.Add(1)
}
