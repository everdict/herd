// Package egress is herd-egress: the per-actor egress gateway (spec herd.md R4, ADR 0001 A-18). Every TCP flow an
// actor's VM opens reaches the Gateway — on Ubuntu through the VM's TAP, which nftables redirects to the gateway and
// nowhere else (package egress/nft); on macOS through a userspace network stack behind the VM's file-handle network
// device (the Stack interface). The gateway decides by NAME, never by address: the SNI of a TLS ClientHello, the
// authority of each HTTP request, the target of a CONNECT. It dials only the name it decided on, resolving it afresh,
// so a listed host with rotating DNS stays reachable and an unlisted host that shares an address with a listed one
// stays refused.
//
// Order of checks for every destination: the Guard (host-side names and addresses) first, then the actor's policy
// (Substrate's EgressPolicy, package egress/policy), default-deny; at dial time the Guard again on every resolved
// address. A matching https rule intercepts the TLS connection with a leaf from the gateway CA and decides each
// request inside it; replace_headers effects put the credential in on the way out, so the actor holds only a
// placeholder.
package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/everdict/herd/egress/policy"
)

// Actor names the actor a flow belongs to.
type Actor struct {
	Space string
	Name  string
	// UID is the actor record's uid; it goes into actor JWTs.
	UID string
}

func (a Actor) String() string { return a.Space + "/" + a.Name }

type actorKey struct{ space, name string }

func (a Actor) key() actorKey { return actorKey{a.Space, a.Name} }

// Resolver resolves a destination name; *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Dialer opens the upstream connection to an address the Guard has passed; *net.Dialer satisfies it.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Config configures a Gateway.
type Config struct {
	// Guard refuses host-side targets; required.
	Guard *Guard
	// Resolver defaults to net.DefaultResolver.
	Resolver Resolver
	// Dialer defaults to a net.Dialer whose Control re-checks the address with the Guard.
	Dialer Dialer
	// CA issues leafs for https rules; without one, https rules refuse.
	CA *CA
	// Credentials resolves credential_uri; without it, a header that needs one refuses the request.
	Credentials Credentials
	// JWTs mints actor_jwt credentials; without it, a header that needs one refuses the request.
	JWTs ActorJWTs
	// UpstreamRoots verifies the servers herd-egress re-originates TLS to; nil means the system roots.
	UpstreamRoots *x509.CertPool
	// OnEvent, if set, sees every decision.
	OnEvent func(Event)
	// FirstByteTimeout bounds how long a new flow may stay silent; default 30 s.
	FirstByteTimeout time.Duration
}

// Event is one decision of the gateway.
type Event struct {
	Actor    Actor
	Protocol string
	Name     string
	Port     int
	Allowed  bool
	HostSide bool
	Reason   string
}

// Stats counts decisions.
type Stats struct {
	Allowed, Refused, RefusedHostSide int64
}

// Gateway is herd-egress's decision and forwarding core, shared by every actor on a host.
type Gateway struct {
	cfg Config

	mu     sync.RWMutex
	actors map[actorKey]*binding

	allowed, refused, refusedHostSide atomic.Int64

	h1, h2c, tlsUp *http.Transport
}

type binding struct {
	actor  Actor
	policy atomic.Pointer[policy.Policy]
}

// New makes a Gateway.
func New(cfg Config) (*Gateway, error) {
	if cfg.Guard == nil {
		return nil, errors.New("egress: a Guard is required")
	}
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	if cfg.Dialer == nil {
		guard := cfg.Guard
		cfg.Dialer = &net.Dialer{Timeout: 10 * time.Second, ControlContext: func(_ context.Context, _, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			return guard.CheckAddr(ap.Addr())
		}}
	}
	if cfg.FirstByteTimeout == 0 {
		cfg.FirstByteTimeout = 30 * time.Second
	}
	g := &Gateway{cfg: cfg, actors: map[actorKey]*binding{}}
	newTransport := func() *http.Transport {
		return &http.Transport{
			DialContext:         g.dial,
			TLSClientConfig:     &tls.Config{RootCAs: cfg.UpstreamRoots, MinVersion: tls.VersionTLS12},
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			ForceAttemptHTTP2:   true,
		}
	}
	g.h1, g.tlsUp = newTransport(), newTransport()
	g.h2c = newTransport()
	g.h2c.Protocols = new(http.Protocols)
	g.h2c.Protocols.SetUnencryptedHTTP2(true)
	return g, nil
}

// Bind registers an actor with the deny-all policy; SetPolicy gives it its rules. Binding an actor again keeps its
// current policy.
func (g *Gateway) Bind(a Actor) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if b, ok := g.actors[a.key()]; ok {
		b.actor.UID = a.UID
		return
	}
	b := &binding{actor: a}
	b.policy.Store(policy.Deny)
	g.actors[a.key()] = b
}

// Unbind forgets an actor; its later flows are refused.
func (g *Gateway) Unbind(a Actor) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.actors, a.key())
}

// Actors lists the bound actors.
func (g *Gateway) Actors() []Actor {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]Actor, 0, len(g.actors))
	for _, b := range g.actors {
		out = append(out, b.actor)
	}
	return out
}

// SetPolicy replaces a bound actor's policy. It takes effect on the next decision of every flow the actor already
// has open: the next request on a kept-alive or intercepted connection, the next ClientHello; an open passthrough
// connection is not re-decided (Substrate's semantics: the gateway reads nothing after the ClientHello).
func (g *Gateway) SetPolicy(a Actor, p *policy.Policy) error {
	if p == nil {
		p = policy.Deny
	}
	g.mu.RLock()
	b, ok := g.actors[a.key()]
	g.mu.RUnlock()
	if !ok {
		return fmt.Errorf("egress: actor %s is not bound", a)
	}
	b.policy.Store(p)
	return nil
}

// Stats returns the decision counters.
func (g *Gateway) Stats() Stats {
	return Stats{Allowed: g.allowed.Load(), Refused: g.refused.Load(), RefusedHostSide: g.refusedHostSide.Load()}
}

func (g *Gateway) lookup(a Actor) (*binding, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	b, ok := g.actors[a.key()]
	return b, ok
}

func (g *Gateway) record(a Actor, proto, name string, port int, err error) {
	ev := Event{Actor: a, Protocol: proto, Name: name, Port: port, Allowed: err == nil}
	if err != nil {
		ev.Reason = err.Error()
		var re *RefusedError
		if errors.As(err, &re) && re.HostSide {
			ev.HostSide = true
			g.refusedHostSide.Add(1)
		}
		g.refused.Add(1)
	} else {
		g.allowed.Add(1)
	}
	if g.cfg.OnEvent != nil {
		g.cfg.OnEvent(ev)
	}
}

// decide runs the Guard and then the actor's current policy.
func (g *Gateway) decide(b *binding, in policy.Input, name string, port int) (policy.Decision, error) {
	if err := g.cfg.Guard.CheckName(name); err != nil {
		return policy.Decision{}, err
	}
	d := b.policy.Load().Decide(in, name, port)
	if !d.Allowed {
		return d, &RefusedError{Reason: d.Reason}
	}
	return d, nil
}

// ServeConn serves one TCP flow of actor that was headed for dst (the address the VM connected to). dst's address
// is never dialed or matched; only its port is used, as the port rules match. ServeConn closes conn.
func (g *Gateway) ServeConn(ctx context.Context, actor Actor, conn net.Conn, dst netip.AddrPort) {
	defer conn.Close()
	b, ok := g.lookup(actor)
	if !ok {
		g.record(actor, "tcp", "", int(dst.Port()), &RefusedError{Reason: "actor is not bound"})
		return
	}
	g.serveStream(ctx, b, conn, int(dst.Port()), "")
}

// serveStream reads the first bytes of a flow: a TLS ClientHello is decided on its SNI, anything else is served as
// HTTP. connectHost is the target of the CONNECT the flow arrived through, if any.
func (g *Gateway) serveStream(ctx context.Context, b *binding, conn net.Conn, port int, connectHost string) {
	_ = conn.SetReadDeadline(time.Now().Add(g.cfg.FirstByteTimeout))
	br := bufio.NewReader(conn)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	if first[0] != 0x16 {
		_ = conn.SetReadDeadline(time.Time{})
		g.serveHTTP(ctx, b, &replayConn{Conn: conn, r: br}, port, connectHost, "")
		return
	}
	hello, raw, err := readClientHello(br)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		g.record(b.actor, "tls", "", port, &RefusedError{Reason: "no TLS ClientHello: " + err.Error()})
		return
	}
	sni := strings.ToLower(hello)
	if sni == "" {
		g.record(b.actor, "tls", "", port, &RefusedError{Reason: "TLS without SNI"})
		return
	}
	if connectHost != "" && sni != connectHost {
		g.record(b.actor, "tls", sni, port, &RefusedError{Reason: fmt.Sprintf("SNI %s differs from CONNECT target %s", sni, connectHost)})
		return
	}
	d, err := g.decide(b, policy.InputTLSHello, sni, port)
	if err != nil {
		g.record(b.actor, "tls", sni, port, err)
		return
	}
	replay := &replayConn{Conn: conn, r: io.MultiReader(bytes.NewReader(raw), br)}
	switch d.Rule.Protocol {
	case policy.ProtocolTLSPassthrough:
		up, err := g.dial(ctx, "tcp", net.JoinHostPort(sni, strconv.Itoa(port)))
		g.record(b.actor, "tls_passthrough", sni, port, err)
		if err != nil {
			return
		}
		splice(replay, up)
	case policy.ProtocolHTTPS:
		if g.cfg.CA == nil {
			g.record(b.actor, "https", sni, port, &RefusedError{Reason: "https rule but herd-egress has no CA"})
			return
		}
		leaf, err := g.cfg.CA.Leaf(sni)
		if err != nil {
			g.record(b.actor, "https", sni, port, err)
			return
		}
		tc := tls.Server(replay, &tls.Config{Certificates: []tls.Certificate{*leaf}, NextProtos: []string{"h2", "http/1.1"}, MinVersion: tls.VersionTLS12})
		g.serveHTTP(ctx, b, tc, port, connectHost, sni)
	}
}

// serveHTTP serves the HTTP requests of one connection, deciding each on its authority. sni is set when the
// connection was intercepted under an https rule.
func (g *Gateway) serveHTTP(ctx context.Context, b *binding, conn net.Conn, port int, connectHost, sni string) {
	ln := newOneConnListener(conn)
	h := &handler{g: g, b: b, port: port, connectHost: connectHost, intercepted: sni != "", ctx: ctx, ln: ln}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
		ErrorLog:          discardLog,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ConnState: func(_ net.Conn, s http.ConnState) {
			// A hijacked connection (CONNECT) is ended by its handler, which closes ln when the tunnel ends.
			if s == http.StateClosed {
				ln.Close()
			}
		},
	}
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP1(true)
	if sni != "" {
		srv.Protocols.SetHTTP2(true)
	} else {
		srv.Protocols.SetUnencryptedHTTP2(true)
	}
	stop := context.AfterFunc(ctx, func() { _ = srv.Close() })
	defer stop()
	_ = srv.Serve(ln)
	<-ln.done
}

type handler struct {
	g           *Gateway
	b           *binding
	ctx         context.Context
	port        int
	connectHost string
	intercepted bool
	ln          *oneConnListener
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	proto := "http"
	in := policy.InputHTTPRequest
	if h.intercepted {
		proto, in = "https", policy.InputInterceptedRequest
	}
	port := h.port
	name := strings.ToLower(hostOnly(r.Host))
	if r.Method == http.MethodConnect {
		h.connect(w, r)
		return
	}
	if r.URL.IsAbs() && !h.intercepted && h.connectHost == "" {
		// An explicit-proxy request names its destination port in the URL.
		port = 80
		if p := r.URL.Port(); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil {
				h.refuse(w, proto, name, port, &RefusedError{Reason: "bad port"})
				return
			}
			port = n
		}
		if r.URL.Scheme != "http" {
			h.refuse(w, proto, name, port, &RefusedError{Reason: "only http:// URLs may be proxied in cleartext"})
			return
		}
	}
	if h.connectHost != "" && name != h.connectHost {
		h.refuse(w, proto, name, port, &RefusedError{Reason: fmt.Sprintf("authority %s differs from CONNECT target %s", name, h.connectHost)})
		return
	}
	d, err := h.g.decide(h.b, in, name, port)
	if err != nil {
		h.refuse(w, proto, name, port, err)
		return
	}
	for _, hd := range d.Rule.Headers {
		if _, ok := r.Header[hd.Name]; !ok {
			continue
		}
		v, err := h.g.credential(r.Context(), h.b.actor, hd)
		if err != nil {
			h.refuse(w, proto, name, port, &RefusedError{Reason: fmt.Sprintf("credential for %s: %v", hd.Name, err)})
			return
		}
		r.Header.Del(hd.Name)
		r.Header.Set(hd.Name, hd.Prefix+v)
	}
	scheme, tr := "http", h.g.h1
	switch {
	case h.intercepted:
		scheme, tr = "https", h.g.tlsUp
	case r.ProtoMajor == 2:
		tr = h.g.h2c
	}
	target := net.JoinHostPort(name, strconv.Itoa(port))
	var upErr error
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = scheme
			pr.Out.URL.Host = target
			pr.Out.Host = r.Host
			pr.Out.RequestURI = ""
		},
		Transport: tr,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			upErr = err
			var re *RefusedError
			if errors.As(err, &re) {
				w.Header().Set("X-Herd-Egress", "refused")
				http.Error(w, re.Error(), http.StatusForbidden)
				return
			}
			http.Error(w, "herd-egress: upstream: "+err.Error(), http.StatusBadGateway)
		},
		ErrorLog: discardLog,
	}
	rp.ServeHTTP(w, r)
	var re *RefusedError
	if errors.As(upErr, &re) {
		h.g.record(h.b.actor, proto, name, port, upErr)
		return
	}
	h.g.record(h.b.actor, proto, name, port, nil)
}

func (h *handler) refuse(w http.ResponseWriter, proto, name string, port int, err error) {
	h.g.record(h.b.actor, proto, name, port, err)
	w.Header().Set("X-Herd-Egress", "refused")
	http.Error(w, err.Error(), http.StatusForbidden)
}

// connect serves an explicit-proxy CONNECT: the guard checks its target, then the tunneled stream is decided like a
// transparent flow to the target's port, and must name the same host.
func (h *handler) connect(w http.ResponseWriter, r *http.Request) {
	host, ps, err := net.SplitHostPort(r.Host)
	port, perr := strconv.Atoi(ps)
	if err != nil || perr != nil || port < 1 || port > 65535 || h.intercepted || h.connectHost != "" || r.ProtoMajor != 1 {
		h.refuse(w, "connect", r.Host, 0, &RefusedError{Reason: "malformed or nested CONNECT"})
		return
	}
	host = strings.ToLower(host)
	if err := h.g.cfg.Guard.CheckName(host); err != nil {
		h.refuse(w, "connect", host, port, err)
		return
	}
	if _, ok := policy.NormalizeName(host); !ok {
		h.refuse(w, "connect", host, port, &RefusedError{Reason: fmt.Sprintf("CONNECT target %q is not a DNS name", host)})
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		h.refuse(w, "connect", host, port, &RefusedError{Reason: "connection cannot be hijacked"})
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer h.ln.Close()
	defer conn.Close()
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	h.g.serveStream(h.ctx, h.b, &replayConn{Conn: conn, r: rw.Reader}, port, host)
}

func (g *Gateway) credential(ctx context.Context, a Actor, h policy.Header) (string, error) {
	switch {
	case h.CredentialURI != "":
		if g.cfg.Credentials == nil {
			return "", errors.New("no credential store")
		}
		return g.cfg.Credentials.Resolve(ctx, a, h.CredentialURI)
	case h.JWT != nil:
		if g.cfg.JWTs == nil {
			return "", errors.New("no actor JWT minter")
		}
		return g.cfg.JWTs.ActorJWT(ctx, a, h.JWT.Audiences, time.Duration(h.JWT.ExpirationSeconds)*time.Second)
	}
	return "", errors.New("header has no credential source")
}

// dial resolves name afresh and dials the first address the Guard passes. A name with any host-side address is
// refused outright, so a name cannot be made to point at the host by adding an address to its answer.
func (g *Gateway) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return nil, err
	}
	if err := g.cfg.Guard.CheckName(host); err != nil {
		return nil, err
	}
	addrs, err := g.cfg.Resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("egress: resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("egress: %s has no address", host)
	}
	for _, a := range addrs {
		if err := g.cfg.Guard.CheckAddr(a); err != nil {
			return nil, &RefusedError{HostSide: true, Reason: fmt.Sprintf("%s resolves to a host-side address: %v", host, err)}
		}
	}
	var last error
	for _, a := range addrs {
		c, err := g.cfg.Dialer.DialContext(ctx, network, netip.AddrPortFrom(a.Unmap(), uint16(port)).String())
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

var discardLog = log.New(io.Discard, "", 0)

func hostOnly(authority string) string {
	if h, _, err := net.SplitHostPort(authority); err == nil {
		return h
	}
	return strings.Trim(authority, "[]")
}

// splice copies both ways until both directions end.
func splice(a, b net.Conn) {
	defer b.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		closeWrite(b)
	}()
	_, _ = io.Copy(a, b)
	closeWrite(a)
	wg.Wait()
}

func closeWrite(c net.Conn) {
	type cw interface{ CloseWrite() error }
	if x, ok := c.(cw); ok {
		_ = x.CloseWrite()
		return
	}
	if r, ok := c.(*replayConn); ok {
		closeWrite(r.Conn)
		return
	}
	_ = c.Close()
}

// replayConn reads from r (bytes already peeked, then the conn) and writes to the conn.
type replayConn struct {
	net.Conn
	r io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// oneConnListener hands out one connection, then blocks until it is closed.
type oneConnListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
	took atomic.Bool
}

func newOneConnListener(c net.Conn) *oneConnListener {
	return &oneConnListener{conn: c, done: make(chan struct{})}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.took.CompareAndSwap(false, true) {
		return l.conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

func (g *Gateway) policyOf(a Actor) *policy.Policy {
	b, ok := g.lookup(a)
	if !ok {
		return nil
	}
	return b.policy.Load()
}
