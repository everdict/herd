package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/link"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Server is the router's side: it accepts herdlets' tunnels and opens connections through them. It never dials.
type Server struct {
	// Authn authenticates herdlets; only a herdlet credential may open a tunnel, and only for the worker it names.
	Authn auth.Authenticator
	// OpenTimeout bounds how long Dial waits for the herdlet to answer an Open (default 10 s).
	OpenTimeout time.Duration
	// WorkerIdentity, if set, answers the node SVID that registered a worker at herd-api ("" if none did): an HS256
	// tunnel for such a worker is refused.
	WorkerIdentity func(ctx context.Context, worker string) (string, error)

	mu       sync.Mutex
	sessions map[string]*session
	pending  map[string]*pendingOpen
	seq      uint64
}

type session struct {
	worker string
	// p is the credential that opened the tunnel: an HS256 tunnel may not replace an SVID one (R3), and a data stream
	// outlives its own leaf while the worker's current tunnel holds a valid one (R1).
	p      auth.Principal
	sendMu sync.Mutex
	stream grpc.ServerStream
	done   chan struct{}
}

type pendingOpen struct {
	worker string
	sess   *session
	ch     chan openResult
}

type openResult struct {
	conn *Conn
	err  error
}

func (s *Server) init() {
	if s.sessions == nil {
		s.sessions = map[string]*session{}
		s.pending = map[string]*pendingOpen{}
	}
}

// Register adds the tunnel service to g.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	g.RegisterService(&grpc.ServiceDesc{
		ServiceName: ServiceName,
		HandlerType: (*any)(nil),
		Streams: []grpc.StreamDesc{
			{StreamName: "Control", ServerStreams: true, ClientStreams: true, Handler: func(_ any, ss grpc.ServerStream) error { return s.serveControl(ss) }},
			{StreamName: "Data", ServerStreams: true, ClientStreams: true, Handler: func(_ any, ss grpc.ServerStream) error { return s.serveData(ss) }},
		},
	}, s)
}

// herdlet authenticates the stream's caller as a herdlet credential.
func (s *Server) herdlet(ctx context.Context) (string, error) {
	p, err := s.herdletPrincipal(ctx)
	return p.HerdletWorker(), err
}

// herdletPrincipal authenticates the stream's caller as a herdlet credential.
func (s *Server) herdletPrincipal(ctx context.Context) (auth.Principal, error) {
	if s.Authn == nil {
		return auth.Principal{}, status.Errorf(codes.Unauthenticated, "tunnel: no authenticator configured")
	}
	var h string
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("authorization"); len(v) > 0 {
			h = v[0]
		}
	}
	if err := auth.RefuseHeader(h); err != nil {
		return auth.Principal{}, status.Errorf(codes.Unauthenticated, "%v", err)
	}
	tok := auth.BearerToken(h)
	if tok == "" && len(auth.PeerChain(ctx)) == 0 {
		return auth.Principal{}, status.Errorf(codes.Unauthenticated, "no node SVID and no bearer token")
	}
	// No bearer: the tunnel is its node's X.509-SVID over mTLS (ADR 0001 A-60).
	p, err := s.Authn.Authenticate(ctx, tok)
	if err != nil {
		return auth.Principal{}, status.Errorf(codes.Unauthenticated, "%v", err)
	}
	if !p.Herdlet {
		return auth.Principal{}, status.Errorf(codes.PermissionDenied, "%q is not a herdlet credential", p.Subject)
	}
	return p, nil
}

func (s *Server) serveControl(ss grpc.ServerStream) error {
	p, err := s.herdletPrincipal(ss.Context())
	if err != nil {
		return err
	}
	if !p.SVID() && s.WorkerIdentity != nil {
		// A worker a node SVID registered is that node's: an HS256 tunnel cannot claim or replace it (re-review 5).
		id, err := s.WorkerIdentity(ss.Context(), p.HerdletWorker())
		if err != nil {
			return status.Errorf(codes.Unavailable, "tunnel: %v", err)
		}
		if id != "" {
			return status.Errorf(codes.PermissionDenied, "worker %q is node SVID %s's: an HS256 credential cannot open its tunnel", p.HerdletWorker(), id)
		}
	}
	// A tunnel ends with the credential that opened it (review 2).
	return auth.ServeWhile(p.Holds, func() error { return s.control(ss, p) })
}

func (s *Server) control(ss grpc.ServerStream, p auth.Principal) error {
	sub := p.HerdletWorker()
	var hello json.RawMessage
	if err := ss.RecvMsg(&hello); err != nil {
		return err
	}
	var up Up
	if err := json.Unmarshal(hello, &up); err != nil || up.Hello == nil || up.Hello.Worker == "" {
		return status.Errorf(codes.InvalidArgument, "tunnel: first message must be hello")
	}
	if up.Hello.Worker != sub {
		return status.Errorf(codes.PermissionDenied, "herdlet credential %q may not open the tunnel of worker %q", sub, up.Hello.Worker)
	}
	sess := &session{worker: sub, p: p, stream: ss, done: make(chan struct{})}
	s.mu.Lock()
	s.init()
	if old, ok := s.sessions[sub]; ok && old.p.SVID() && !p.SVID() {
		// Checked under the lock with the swap (R3): an HS256 tunnel cannot replace a node SVID's.
		s.mu.Unlock()
		return status.Errorf(codes.PermissionDenied, "worker %q's tunnel is node SVID %s's: an HS256 credential cannot replace it", sub, old.p.Subject)
	}
	// A newer tunnel of the worker takes its Opens from now on (make-before-break, ADR 0001 A-60): the older one goes
	// on serving the Opens it was sent until its herdlet ends it, and the data streams it opened stay up until they end.
	s.sessions[sub] = sess
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.sessions[sub] == sess {
			delete(s.sessions, sub)
		}
		close(sess.done)
		s.mu.Unlock()
	}()
	if err := sess.send(Down{Welcome: &Welcome{Worker: sub}}); err != nil {
		return err
	}
	for {
		var m json.RawMessage
		if err := ss.RecvMsg(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := json.Unmarshal(m, &up); err != nil {
			return status.Errorf(codes.InvalidArgument, "tunnel: decode: %v", err)
		}
		if r := up.Refused; r != nil {
			s.mu.Lock()
			p, ok := s.pending[r.ID]
			if ok && p.worker == sub {
				delete(s.pending, r.ID)
			}
			s.mu.Unlock()
			if ok && p.worker == sub {
				p.ch <- openResult{err: &Error{Reason: r.Reason, Detail: r.Detail}}
			}
		}
	}
}

func (sess *session) send(d Down) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
	return sess.stream.SendMsg(json.RawMessage(b))
}

// Connected reports whether worker's herdlet holds a tunnel to this router.
func (s *Server) Connected(worker string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sessions[worker]
	return ok
}

// Dial asks worker's herdlet for a connection to port of vm and waits for it. ctx bounds only the wait: the
// connection, once open, lives until either side closes it or the herdlet cuts it. Failures are *Error.
func (s *Server) Dial(ctx context.Context, worker, vm string, port uint32) (net.Conn, error) {
	if !validVM(vm) {
		return nil, fmt.Errorf("tunnel: invalid VM id %q", vm)
	}
	s.mu.Lock()
	s.init()
	sess, ok := s.sessions[worker]
	s.seq++
	id := worker + "-" + strconv.FormatUint(s.seq, 10)
	p := &pendingOpen{worker: worker, sess: sess, ch: make(chan openResult, 1)}
	if ok {
		s.pending[id] = p
	}
	s.mu.Unlock()
	if !ok {
		return nil, &Error{Reason: ReasonNodeUnreachable, Detail: fmt.Sprintf("worker %q has no tunnel to this router", worker)}
	}
	drop := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, still := s.pending[id]
		delete(s.pending, id)
		return still
	}
	if err := sess.send(Down{Open: &Open{ID: id, VM: vm, Port: port}}); err != nil {
		drop()
		return nil, &Error{Reason: ReasonNodeUnreachable, Detail: fmt.Sprintf("worker %q: send open: %v", worker, err)}
	}
	timeout := s.OpenTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	var fail error
	select {
	case r := <-p.ch:
		return r.conn, r.err
	case <-sess.done:
		fail = &Error{Reason: ReasonNodeUnreachable, Detail: fmt.Sprintf("worker %q: tunnel closed before it answered", worker)}
	case <-t.C:
		fail = &Error{Reason: ReasonOpenTimeout, Detail: fmt.Sprintf("worker %q did not answer within %v", worker, timeout)}
	case <-ctx.Done():
		fail = ctx.Err()
	}
	if !drop() {
		// The answer raced the failure: take it and close it, so the herdlet's side ends too.
		if r := <-p.ch; r.conn != nil {
			r.conn.Close()
		}
	}
	return nil, fail
}

func (s *Server) serveData(ss grpc.ServerStream) error {
	p, err := s.herdletPrincipal(ss.Context())
	if err != nil {
		return err
	}
	// A data stream may outlive the leaf it opened with while the worker's current tunnel is authenticated with a
	// valid, trusted leaf: a node that rotates keeps its long streams; one that stops loses them by its leaf's end plus
	// DrainGrace (R1).
	worker := p.HerdletWorker()
	hold := func(t time.Time) error {
		err := p.Holds(t)
		if err == nil || p.Hold == nil || !p.HardEnd.IsZero() {
			return err
		}
		s.mu.Lock()
		cur, ok := s.sessions[worker]
		s.mu.Unlock()
		if ok && cur.p.SVID() && cur.p.Expires.After(t) && cur.p.Holds(t) == nil {
			return nil
		}
		return err
	}
	return auth.ServeWhile(hold, func() error { return s.data(ss, worker) })
}

func (s *Server) data(ss grpc.ServerStream, sub string) error {
	var id string
	if md, ok := metadata.FromIncomingContext(ss.Context()); ok {
		if v := md.Get(IDHeader); len(v) > 0 {
			id = v[0]
		}
	}
	s.mu.Lock()
	p, ok := s.pending[id]
	if ok && p.worker == sub {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if !ok || p.worker != sub {
		return status.Errorf(codes.NotFound, "tunnel: no open %q waiting for worker %q", id, sub)
	}
	local, remote := net.Pipe()
	c := &Conn{Conn: local}
	p.ch <- openResult{conn: c}
	recvDone := make(chan struct{})
	// herdlet → router: frames into the pipe until a cut or the end of the stream.
	go func() {
		defer close(recvDone)
		defer remote.Close()
		for {
			var f Frame
			if err := ss.RecvMsg(&f); err != nil {
				if !errors.Is(err, io.EOF) {
					c.setCut(ReasonNodeUnreachable, err.Error())
				}
				return
			}
			switch f.Kind {
			case FrameCut:
				c.setCut(f.Reason, f.Detail)
				return
			case FrameData:
				if _, err := remote.Write(f.Data); err != nil {
					return
				}
			}
		}
	}()
	// router → herdlet: what the router writes, until it closes its side or the other direction ended.
	buf := make([]byte, 32<<10)
	for {
		n, err := remote.Read(buf)
		if n > 0 {
			if err := ss.SendMsg(&Frame{Kind: FrameData, Data: buf[:n]}); err != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	select {
	case <-recvDone:
		return nil
	default:
	}
	// The router closed its side: tell the herdlet, which closes the guest connection and ends its half.
	_ = ss.SendMsg(&Frame{Kind: FrameClose})
	select {
	case <-recvDone:
	case <-time.After(closeWait):
	}
	return nil
}

// closeWait bounds how long a data stream waits for the herdlet to end its half after the router closed.
const closeWait = 5 * time.Second

// Conn is a connection through a tunnel. When the herdlet cut it, Cut says why.
type Conn struct {
	net.Conn
	mu     sync.Mutex
	reason Reason
	detail string
}

func (c *Conn) setCut(r Reason, d string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reason == "" {
		c.reason, c.detail = r, d
	}
}

// Cut returns why the herdlet cut the connection, if it did.
func (c *Conn) Cut() (Reason, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reason, c.detail, c.reason != ""
}

// ControlStreamDesc is the client stream description of the control stream.
var ControlStreamDesc = grpc.StreamDesc{StreamName: "Control", ServerStreams: true, ClientStreams: true}

// DataStreamDesc is the client stream description of a data stream.
var DataStreamDesc = grpc.StreamDesc{StreamName: "Data", ServerStreams: true, ClientStreams: true}

// ControlCodec is the content-subtype of the control stream.
const ControlCodec = link.CodecName
