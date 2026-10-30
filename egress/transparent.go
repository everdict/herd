package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
)

// Sources maps a VM's address on its TAP to the actor it runs (Ubuntu: one address per VM, anti-spoofed by the
// VM's nftables table).
type Sources struct {
	mu sync.RWMutex
	m  map[netip.Addr]Actor
}

// Set binds src to actor.
func (s *Sources) Set(src netip.Addr, a Actor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[netip.Addr]Actor{}
	}
	s.m[src.Unmap()] = a
}

// Remove forgets src.
func (s *Sources) Remove(src netip.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, src.Unmap())
}

// Actor answers the actor bound to src.
func (s *Sources) Actor(src netip.Addr) (Actor, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.m[src.Unmap()]
	return a, ok
}

// OriginalDst answers where a redirected connection was headed; on Linux it reads SO_ORIGINAL_DST.
type OriginalDst func(net.Conn) (netip.AddrPort, error)

// ServeTransparent accepts the connections nftables redirected from VMs' TAPs to ln and serves each through the
// gateway as a flow of the actor its source address is bound to. A connection from an unbound source is refused.
// origDst nil means SO_ORIGINAL_DST.
func (g *Gateway) ServeTransparent(ctx context.Context, ln net.Listener, src *Sources, origDst OriginalDst) error {
	if origDst == nil {
		origDst = SockOriginalDst
	}
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func() {
			ra, err := netip.ParseAddrPort(c.RemoteAddr().String())
			if err != nil {
				_ = c.Close()
				return
			}
			a, ok := src.Actor(ra.Addr())
			if !ok {
				g.record(Actor{}, "tcp", ra.Addr().String(), 0, &RefusedError{Reason: "connection from an address bound to no actor"})
				_ = c.Close()
				return
			}
			dst, err := origDst(c)
			if err != nil {
				g.record(a, "tcp", "", 0, &RefusedError{Reason: "no original destination: " + err.Error()})
				_ = c.Close()
				return
			}
			g.ServeConn(ctx, a, c, dst)
		}()
	}
}
