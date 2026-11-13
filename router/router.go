// Package router is herd-router (spec R3; Substrate's atenet-router): resume-on-request ingress over the herdlets'
// outbound tunnels.
//
// A client names its actor in the ate-target-actor header as "<atespace>/<actor>", as it does with Substrate, and
// carries a bearer token; the router authenticates it and authorizes it for that atespace before anything else.
// Ordinary requests go to the actor's default port (herd-guest's API, a gRPC service over h2c); an HTTP CONNECT
// reaches any other port named in the authority. The router finds the actor's worker through herd-api, and reaches
// the VM only through that worker's tunnel (package tunnel): it never dials a node.
//
// A request to an actor that is not running is parked: the router asks herd-api to resume it and delivers the
// request once the actor is RUNNING. A request still parked when the parking budget runs out is answered with a
// typed 503: HTTP status 503, the reason in the herd-reason header, and for gRPC clients grpc-status UNAVAILABLE.
//
// Long-lived streams (a terminal, a followed exec) are first-class: nothing in the router times out a request once
// it is delivered. A suspend cuts the actor's streams; the router ends each with the reason in the herd-reason
// trailer (grpc-status UNAVAILABLE for gRPC) and the client re-attaches — the next request resumes the actor, and
// herd-guest follows an exec from the client's byte offsets.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/tunnel"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Header names.
const (
	// TargetActorHeader names the actor as "<atespace>/<actor>" (Substrate's header and format).
	TargetActorHeader = "ate-target-actor"
	// ReasonHeader carries a typed Reason on a refusal (a header) or on a cut stream (a trailer).
	ReasonHeader = "herd-reason"
)

// Reason is why the router refused or ended a request.
type Reason string

const (
	ReasonBadTarget        Reason = "bad-target"
	ReasonUnauthenticated  Reason = "unauthenticated"
	ReasonPermissionDenied Reason = "permission-denied"
	ReasonActorNotFound    Reason = "actor-not-found"
	ReasonActorDeleting    Reason = "actor-deleting"
	// ReasonActorCrashed: the actor is CRASHED (its VM or a container of it ended); the detail is its crash message.
	ReasonActorCrashed Reason = "actor-crashed"
	// ReasonParkingBudget: the actor did not become reachable within the parking budget.
	ReasonParkingBudget Reason = "parking-budget-exceeded"
	// ReasonSuspended: the stream was cut because the actor is being suspended; re-attach after resume.
	ReasonSuspended = Reason(tunnel.ReasonSuspended)
	// ReasonDeleted: the stream was cut because the actor is being deleted.
	ReasonDeleted = Reason(tunnel.ReasonDeleted)
	// ReasonNodeUnreachable: the actor's worker has no tunnel to this router.
	ReasonNodeUnreachable = Reason(tunnel.ReasonNodeUnreachable)
	// ReasonStreamLost: the stream ended without a known cause (the node's tunnel broke, the guest went away).
	ReasonStreamLost Reason = "stream-lost"
)

// Tunnels opens connections to a port of a VM through a worker's tunnel (tunnel.Server).
type Tunnels interface {
	Dial(ctx context.Context, worker, vm string, port uint32) (net.Conn, error)
	// Connected reports whether worker's herdlet holds a tunnel to this router.
	Connected(worker string) bool
}

// placement is where an actor runs.
type placement struct{ worker, vm string }

// Config wires a Router.
type Config struct {
	Control Control
	Tunnels Tunnels
	// DefaultPort is the guest port ordinary requests go to (herd-guest's API, 8080).
	DefaultPort uint32
	// ParkingBudget bounds how long a request waits for its actor to become reachable (default 30 s). It bounds
	// only the wait: a delivered request is never timed out by the router.
	ParkingBudget time.Duration
	// PollInterval is how often a parked request looks at the actor again (default 50 ms).
	PollInterval time.Duration
	// ResumeTimeout bounds one ResumeActor call (default 2 min). The call is not tied to the request that started
	// it, so a client that gives up does not abandon a resume half-way.
	ResumeTimeout time.Duration
}

// Router is herd-router's request handler.
type Router struct {
	cfg      Config
	resuming sync.Map // "<space>/<name>" → struct{}: a resume is in flight
	lastErr  sync.Map // "<space>/<name>" → string: why the last resume did not succeed
	placed   sync.Map // "<space>/<name>" → placement: where herd-api last said the actor runs
	parked   atomic.Int64
	bg       sync.WaitGroup
}

// New returns a router.
func New(cfg Config) (*Router, error) {
	if cfg.Control == nil || cfg.Tunnels == nil {
		return nil, errors.New("router: Control and Tunnels are required")
	}
	if cfg.DefaultPort == 0 {
		cfg.DefaultPort = 8080
	}
	if cfg.ParkingBudget <= 0 {
		cfg.ParkingBudget = 30 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 50 * time.Millisecond
	}
	if cfg.ResumeTimeout <= 0 {
		cfg.ResumeTimeout = 2 * time.Minute
	}
	return &Router{cfg: cfg}, nil
}

// actorWrite is one request's write on its actor, decided by herd-api (Control.CheckActorWrite) with the client's own
// bearer: the router holds no policy (review 7). The decision is for one actor uid: a parked request that finds the
// actor of the name re-created (another uid, perhaps another creator) asks again (review 6).
type actorWrite struct {
	rt                 *Router
	token, space, name string
	uid                string
	expires            time.Time
}

// decide asks herd-api; nil when the write is allowed.
func (w *actorWrite) decide(ctx context.Context) *routeError {
	d, err := w.rt.cfg.Control.CheckActorWrite(ctx, w.token, w.space, w.name)
	if err != nil {
		switch status.Code(err) {
		case codes.Unauthenticated:
			return &routeError{http.StatusUnauthorized, codes.Unauthenticated, ReasonUnauthenticated, status.Convert(err).Message()}
		case codes.PermissionDenied:
			return &routeError{http.StatusForbidden, codes.PermissionDenied, ReasonPermissionDenied, status.Convert(err).Message()}
		case codes.NotFound:
			return &routeError{http.StatusNotFound, codes.NotFound, ReasonActorNotFound, fmt.Sprintf("actor %s/%s not found", w.space, w.name)}
		case codes.InvalidArgument:
			return &routeError{http.StatusBadRequest, codes.InvalidArgument, ReasonBadTarget, status.Convert(err).Message()}
		}
		return &routeError{http.StatusServiceUnavailable, codes.Unavailable, ReasonParkingBudget, "herd-api: " + err.Error()}
	}
	w.uid, w.expires = d.UID, d.Expires
	return nil
}

// allow is the decision for the actor of uid: the one made, or a new one when the uid is another.
func (w *actorWrite) allow(ctx context.Context, uid string) *routeError {
	if uid == w.uid {
		return nil
	}
	if rerr := w.decide(ctx); rerr != nil {
		return rerr
	}
	if w.uid != uid {
		// The actor changed again between the read and the decision; the next poll reads it again.
		return &routeError{http.StatusServiceUnavailable, codes.Unavailable, ReasonParkingBudget, fmt.Sprintf("actor %s/%s changed while it was decided", w.space, w.name)}
	}
	return nil
}

// Parked is how many requests are waiting for their actor right now.
func (rt *Router) Parked() int64 { return rt.parked.Load() }

// Wait blocks until resumes the router started have returned.
func (rt *Router) Wait() { rt.bg.Wait() }

// NewHTTPServer returns the server the router runs in: HTTP/1.1 and prior-knowledge h2c (gRPC clients), with no
// read, write or idle timeout on a request — only the header read is bounded — so long-lived streams are not cut.
func NewHTTPServer(h http.Handler) *http.Server {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return &http.Server{Handler: h, Protocols: p, ReadHeaderTimeout: 30 * time.Second}
}

// routeError is a typed refusal.
type routeError struct {
	http   int
	code   codes.Code
	reason Reason
	detail string
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ParseTarget parses an ate-target-actor value, "<atespace>/<actor>".
func ParseTarget(v string) (space, name string, err error) {
	space, name, ok := strings.Cut(v, "/")
	if !ok || !nameRE.MatchString(space) || !nameRE.MatchString(name) {
		return "", "", fmt.Errorf("invalid actor reference %q: want <atespace>/<actor>", v)
	}
	return space, name, nil
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	space, name, err := ParseTarget(r.Header.Get(TargetActorHeader))
	if err != nil {
		fail(w, r, &routeError{http.StatusBadRequest, codes.InvalidArgument, ReasonBadTarget, err.Error()})
		return
	}
	token := auth.BearerToken(r.Header.Get("Authorization"))
	if token == "" {
		fail(w, r, &routeError{http.StatusUnauthorized, codes.Unauthenticated, ReasonUnauthenticated, "no bearer token"})
		return
	}
	// Authority before effect: herd-api decides, with the client's bearer, before anything is resumed or reached.
	wr := &actorWrite{rt: rt, token: token, space: space, name: name}
	if rerr := wr.decide(r.Context()); rerr != nil {
		fail(w, r, rerr)
		return
	}
	port := rt.cfg.DefaultPort
	if r.Method == http.MethodConnect {
		_, ps, err := net.SplitHostPort(r.Host)
		n, perr := strconv.ParseUint(ps, 10, 16)
		if err != nil || perr != nil || n == 0 {
			fail(w, r, &routeError{http.StatusBadRequest, codes.InvalidArgument, ReasonBadTarget, fmt.Sprintf("CONNECT authority %q names no port", r.Host)})
			return
		}
		port = uint32(n)
	}
	// The parking budget bounds only the wait for a connection, never the request once delivered.
	pctx, cancel := context.WithTimeout(r.Context(), rt.cfg.ParkingBudget)
	conn, rerr := rt.connect(pctx, wr, token, space, name, port)
	cancel()
	if rerr != nil {
		fail(w, r, rerr)
		return
	}
	// A stream lasts no longer than the credential that opened it (review 2): at its end the connection is cut.
	if !wr.expires.IsZero() {
		cut := time.AfterFunc(time.Until(wr.expires), func() { _ = conn.Close() })
		defer cut.Stop()
	}
	if r.Method == http.MethodConnect {
		rt.splice(w, r, conn, token, space, name)
		return
	}
	rt.proxy(w, r, conn, token, space, name)
}

// connect resolves the actor (parking while it is not running) and opens a connection to port through its worker's
// tunnel, retrying until ctx (the parking budget) ends.
func (rt *Router) connect(ctx context.Context, w *actorWrite, token, space, name string, port uint32) (net.Conn, *routeError) {
	parked := false
	defer func() {
		if parked {
			rt.parked.Add(-1)
		}
	}()
	var last, lastAPI string
	for {
		worker, vm, why, rerr := rt.resolve(ctx, w, token, space, name)
		if rerr != nil {
			return nil, rerr
		}
		if worker != "" {
			c, err := rt.cfg.Tunnels.Dial(ctx, worker, vm, port)
			if err == nil {
				return c, nil
			}
			why = err.Error()
		}
		if strings.HasPrefix(why, "herd-api: ") {
			lastAPI = why
		}
		if ctx.Err() == nil {
			// A lookup cut short by the budget itself says nothing about the actor.
			last = why
		}
		if !parked {
			parked = true
			rt.parked.Add(1)
		}
		select {
		case <-ctx.Done():
			return nil, rt.budgetError(space, name, last, lastAPI)
		case <-time.After(rt.cfg.PollInterval):
		}
	}
}

// budgetError is the typed 503 of a request whose budget ended. The reason is derived from state, not from whichever
// lookup happened to finish last: if the actor's last known worker holds no tunnel to this router, it is
// node-unreachable whether or not herd-api answered in time (herd-api's error, if any, is added as detail);
// otherwise it is parking-budget-exceeded.
func (rt *Router) budgetError(space, name, last, lastAPI string) *routeError {
	if v, ok := rt.placed.Load(space + "/" + name); ok {
		if p := v.(placement); !rt.cfg.Tunnels.Connected(p.worker) {
			d := fmt.Sprintf("actor %s/%s runs on worker %q, which has no tunnel to this router (parking budget %v)", space, name, p.worker, rt.cfg.ParkingBudget)
			if lastAPI != "" {
				d += "; " + lastAPI
			}
			return &routeError{http.StatusServiceUnavailable, codes.Unavailable, ReasonNodeUnreachable, d}
		}
	}
	if last == "" {
		last = lastAPI
	}
	d := fmt.Sprintf("actor %s/%s did not become reachable within the parking budget (%v): %s", space, name, rt.cfg.ParkingBudget, last)
	if e, ok := rt.lastErr.Load(space + "/" + name); ok {
		d += "; last resume: " + e.(string)
	}
	return &routeError{http.StatusServiceUnavailable, codes.Unavailable, ReasonParkingBudget, d}
}

// resolve looks the actor up once. It answers its worker and VM when the actor runs; otherwise why it cannot be
// reached yet (and it starts a resume of a suspended actor), or a terminal refusal.
func (rt *Router) resolve(ctx context.Context, wr *actorWrite, token, space, name string) (worker, vm, why string, _ *routeError) {
	a, err := rt.cfg.Control.GetActor(ctx, token, space, name)
	if err != nil {
		switch status.Code(err) {
		case codes.NotFound:
			return "", "", "", &routeError{http.StatusNotFound, codes.NotFound, ReasonActorNotFound, fmt.Sprintf("actor %s/%s not found", space, name)}
		case codes.PermissionDenied, codes.Unauthenticated:
			return "", "", "", &routeError{http.StatusForbidden, codes.PermissionDenied, ReasonPermissionDenied, status.Convert(err).Message()}
		}
		return "", "", "herd-api: " + err.Error(), nil
	}
	// A request to an actor (an exec, a stream, a resume on request) is a write on it, decided for this actor uid
	// (ADR 0001 A-60).
	if rerr := wr.allow(ctx, a.GetMetadata().GetUid()); rerr != nil {
		return "", "", "", rerr
	}
	st := a.GetStatus().GetState()
	k := space + "/" + name
	if node := a.GetStatus().GetAssignedNode(); st == ateapi.ActorState_ACTOR_STATE_RUNNING && node != "" {
		rt.placed.Store(k, placement{worker: node, vm: a.GetMetadata().GetUid()})
	} else {
		rt.placed.Delete(k)
	}
	switch st {
	case ateapi.ActorState_ACTOR_STATE_RUNNING:
		if node := a.GetStatus().GetAssignedNode(); node != "" {
			return node, a.GetMetadata().GetUid(), "", nil
		}
	case ateapi.ActorState_ACTOR_STATE_SUSPENDED:
		rt.resume(token, space, name)
	case ateapi.ActorState_ACTOR_STATE_DELETING:
		return "", "", "", &routeError{http.StatusNotFound, codes.NotFound, ReasonActorDeleting, fmt.Sprintf("actor %s/%s is being deleted", space, name)}
	case ateapi.ActorState_ACTOR_STATE_CRASHED:
		// Terminal until a delete: waiting out the parking budget would only hide why.
		return "", "", "", &routeError{http.StatusBadRequest, codes.FailedPrecondition, ReasonActorCrashed,
			fmt.Sprintf("actor %s/%s is CRASHED: %s", space, name, a.GetStatus().GetCrash().GetMessage())}
	}
	return "", "", "actor is " + st.String(), nil
}

// resume asks herd-api to resume the actor unless a resume of it is already in flight. Parked requests share it.
func (rt *Router) resume(token, space, name string) {
	k := space + "/" + name
	if _, busy := rt.resuming.LoadOrStore(k, struct{}{}); busy {
		return
	}
	rt.bg.Add(1)
	go func() {
		defer rt.bg.Done()
		defer rt.resuming.Delete(k)
		ctx, cancel := context.WithTimeout(context.Background(), rt.cfg.ResumeTimeout)
		defer cancel()
		if err := rt.cfg.Control.ResumeActor(ctx, token, space, name); err != nil {
			rt.lastErr.Store(k, err.Error())
		} else {
			rt.lastErr.Delete(k)
		}
	}()
}

func isGRPC(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}

// fail answers a request that was not delivered: a typed refusal. gRPC clients get it trailers-only
// (grpc-status, grpc-message, herd-reason) with the HTTP status; others get the HTTP status and a text body.
func fail(w http.ResponseWriter, r *http.Request, e *routeError) {
	h := w.Header()
	h.Set(ReasonHeader, string(e.reason))
	if isGRPC(r) {
		h.Set("Content-Type", "application/grpc")
		h.Set("Grpc-Status", strconv.Itoa(int(e.code)))
		h.Set("Grpc-Message", grpcMessage(e.detail))
		w.WriteHeader(e.http)
		return
	}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(e.http)
	fmt.Fprintf(w, "%s: %s\n", e.reason, e.detail)
}

// trail ends a delivered response that was cut: the reason goes in the trailers.
func trail(w http.ResponseWriter, r *http.Request, e *routeError) {
	h := w.Header()
	h.Set(http.TrailerPrefix+ReasonHeader, string(e.reason))
	if isGRPC(r) {
		h.Set(http.TrailerPrefix+"Grpc-Status", strconv.Itoa(int(e.code)))
		h.Set(http.TrailerPrefix+"Grpc-Message", grpcMessage(e.detail))
	}
}

// grpcMessage percent-encodes a grpc-message value as gRPC requires.
func grpcMessage(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c <= 0x7e && c != '%' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// lost says why a delivered request's connection ended early: the cut reason the herdlet sent, or else what
// herd-api says about the actor now.
func (rt *Router) lost(conn net.Conn, token, space, name string, err error) *routeError {
	if c, ok := conn.(interface {
		Cut() (tunnel.Reason, string, bool)
	}); ok {
		if r, d, ok := c.Cut(); ok {
			return cutError(Reason(r), d)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, gerr := rt.cfg.Control.GetActor(ctx, token, space, name)
	switch {
	case status.Code(gerr) == codes.NotFound:
		return cutError(ReasonDeleted, "the actor is gone")
	case gerr == nil:
		switch a.GetStatus().GetState() {
		case ateapi.ActorState_ACTOR_STATE_SUSPENDING, ateapi.ActorState_ACTOR_STATE_SUSPENDED:
			return cutError(ReasonSuspended, "the actor is "+a.GetStatus().GetState().String())
		case ateapi.ActorState_ACTOR_STATE_DELETING:
			return cutError(ReasonDeleted, "the actor is being deleted")
		}
	}
	return &routeError{http.StatusBadGateway, codes.Unavailable, ReasonStreamLost, fmt.Sprintf("connection to actor %s/%s lost: %v", space, name, err)}
}

func cutError(r Reason, detail string) *routeError {
	msg := fmt.Sprintf("herd: stream cut (%s): %s", r, detail)
	if r == ReasonSuspended {
		msg += "; send the request again to resume the actor, and re-attach by exec id"
	}
	return &routeError{http.StatusServiceUnavailable, codes.Unavailable, r, msg}
}

var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization", "Transfer-Encoding", "Upgrade"}

// proxy forwards one request over conn as h2c and streams the response back, flushing as bytes arrive. It runs on
// the client's own request context: no deadline of the router's applies.
func (rt *Router) proxy(w http.ResponseWriter, r *http.Request, conn net.Conn, token, space, name string) {
	defer conn.Close()
	var once sync.Once
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: p, DialContext: func(context.Context, string, string) (net.Conn, error) {
		var c net.Conn
		once.Do(func() { c = conn })
		if c == nil {
			return nil, errors.New("router: the tunnel connection was already used")
		}
		return c, nil
	}}
	defer tr.CloseIdleConnections()

	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL = &url.URL{Scheme: "http", Host: "herd-guest", Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	// The client's credential stays at the router: no credential in the actor.
	out.Header.Del("Authorization")
	if r.ContentLength == 0 {
		out.Body = nil
	}
	resp, err := tr.RoundTrip(out)
	if err != nil {
		if r.Context().Err() == nil {
			fail(w, r, rt.lost(conn, token, space, name, err))
		}
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	for _, h := range hopHeaders {
		w.Header().Del(h)
	}
	w.WriteHeader(resp.StatusCode)
	// A gRPC status in the headers is a trailers-only answer (the guest refused before any message): it must stay
	// one HEADERS frame that ends the stream. Flushed now, it would open a stream that then ends with no
	// grpc-status, which a gRPC client reads as a broken stream instead of the guest's typed refusal.
	if resp.Header.Get("Grpc-Status") != "" {
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if errors.Is(err, io.EOF) {
			for k, vs := range resp.Trailer {
				for _, v := range vs {
					w.Header().Add(http.TrailerPrefix+k, v)
				}
			}
			return
		}
		if err != nil {
			if r.Context().Err() == nil {
				trail(w, r, rt.lost(conn, token, space, name, err))
			}
			return
		}
	}
}

// splice carries a CONNECT: HTTP/1.1 by taking over the client connection, HTTP/2 over the request and response
// bodies. Over HTTP/2 a cut ends the response with the reason in the trailers; over HTTP/1.1 the connection is
// closed, since a raw byte stream has nowhere to put one.
func (rt *Router) splice(w http.ResponseWriter, r *http.Request, conn net.Conn, token, space, name string) {
	defer conn.Close()
	if r.ProtoMajor == 1 {
		cc, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			fail(w, r, &routeError{http.StatusInternalServerError, codes.Internal, ReasonStreamLost, "cannot take over the connection: " + err.Error()})
			return
		}
		defer cc.Close()
		if _, err := brw.WriteString("HTTP/1.1 200 Connection established\r\n\r\n"); err != nil || brw.Flush() != nil {
			return
		}
		go func() {
			// Bytes the server already buffered belong to the tunnel.
			if n := brw.Reader.Buffered(); n > 0 {
				b, _ := brw.Reader.Peek(n)
				if _, err := conn.Write(b); err != nil {
					cc.Close()
					return
				}
			}
			_, _ = io.Copy(conn, cc)
			conn.Close()
		}()
		_, _ = io.Copy(cc, conn)
		return
	}
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	go func() { _, _ = io.Copy(conn, r.Body) }()
	buf := make([]byte, 32<<10)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			if c, ok := conn.(interface {
				Cut() (tunnel.Reason, string, bool)
			}); ok {
				if _, _, cut := c.Cut(); cut && r.Context().Err() == nil {
					trail(w, r, rt.lost(conn, token, space, name, err))
				}
			}
			return
		}
	}
}
