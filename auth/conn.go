package auth

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// DrainMargin is how long before a client's X.509-SVID ends herd asks the client to reconnect (ADR 0001 A-60).
var DrainMargin = time.Minute

// An X.509 connection is authenticated at its handshake (review 1, re-review): rotating the client's leaf is no loss
// of authority, so in-flight work is never cut for it. Each call verifies the leaf as of the handshake, while the trust
// domain, the chain to the current bundle and the policy are checked on every call. Before the leaf ends the
// connection drains: it takes no new call (Unavailable: reconnect), the calls and streams it carries finish with no
// bound, and once none is left it closes, so the client reconnects with its current SVID.

// handshake is a server connection's TLS handshake and its drain.
type handshake struct {
	credentials.TLSInfo
	at       time.Time
	notAfter time.Time
	now      func() time.Time

	mu       sync.Mutex
	raw      net.Conn
	inflight int
	draining bool
	closed   bool
}

func (h *handshake) begin() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.notAfter.IsZero() && h.now().After(h.notAfter) {
		// The drain timer runs on the monotonic clock; the leaf ends by the wall clock (R2): no call past it.
		h.draining = true
	}
	if h.draining {
		h.closeIfIdle()
		return false
	}
	h.inflight++
	return true
}

func (h *handshake) end() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inflight--
	h.closeIfIdle()
}

func (h *handshake) drain() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.draining = true
	h.closeIfIdle()
}

// closeIfIdle closes a draining connection with nothing in flight. h.mu is held.
func (h *handshake) closeIfIdle() {
	if h.draining && h.inflight == 0 && !h.closed {
		h.closed = true
		_ = h.raw.Close()
	}
}

// serverCreds records each connection's handshake and schedules its drain.
type serverCreds struct {
	credentials.TransportCredentials
	now func() time.Time
}

// ServerCreds is the transport of a herd gRPC server: TLS of cfg (ServerTLS), each connection drained before its
// client's leaf ends. Serve with ConnInterceptors.
func ServerCreds(cfg *tls.Config) credentials.TransportCredentials {
	return serverCreds{TransportCredentials: credentials.NewTLS(cfg), now: time.Now}
}

func (c serverCreds) ServerHandshake(raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, ai, err := c.TransportCredentials.ServerHandshake(raw)
	if err != nil {
		return conn, ai, err
	}
	ti, ok := ai.(credentials.TLSInfo)
	if !ok {
		return conn, ai, nil
	}
	h := &handshake{TLSInfo: ti, at: c.now(), raw: conn, now: c.now}
	if certs := ti.State.PeerCertificates; len(certs) > 0 {
		h.notAfter = certs[0].NotAfter
		at := h.notAfter.Add(-DrainMargin)
		if half := h.at.Add(h.notAfter.Sub(h.at) / 2); at.Before(half) {
			// A leaf with less than the margin left drains halfway through what it has.
			at = half
		}
		time.AfterFunc(time.Until(at), h.drain)
	}
	return conn, h, nil
}

func (c serverCreds) Clone() credentials.TransportCredentials {
	return serverCreds{TransportCredentials: c.TransportCredentials.Clone(), now: c.now}
}

func handshakeOf(ctx context.Context) *handshake {
	if p, ok := peer.FromContext(ctx); ok {
		if h, ok := p.AuthInfo.(*handshake); ok {
			return h
		}
	}
	return nil
}

type handshakeTimeKey struct{}

// WithHandshakeTime returns ctx as a call on a connection whose handshake was at t (tests, and servers that are not
// gRPC).
func WithHandshakeTime(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, handshakeTimeKey{}, t)
}

// HandshakeTime is when ctx's caller's TLS connection was established (zero if it is not one of ServerCreds').
func HandshakeTime(ctx context.Context) time.Time {
	if t, ok := ctx.Value(handshakeTimeKey{}).(time.Time); ok {
		return t
	}
	if h := handshakeOf(ctx); h != nil {
		return h.at
	}
	return time.Time{}
}

var errDraining = status.Error(codes.Unavailable, "auth: the connection is draining before its client's X.509-SVID ends: reconnect")

// ConnInterceptors count each connection's calls and streams, and refuse new ones on a draining connection.
func ConnInterceptors() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
			h := handshakeOf(ctx)
			if h == nil {
				return next(ctx, req)
			}
			if !h.begin() {
				return nil, errDraining
			}
			defer h.end()
			return next(ctx, req)
		}),
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
			h := handshakeOf(ss.Context())
			if h == nil {
				return next(srv, ss)
			}
			if !h.begin() {
				return errDraining
			}
			defer h.end()
			return next(srv, ss)
		}),
	}
}
