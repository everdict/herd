package deploy

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/everdict/herd/auth"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// Identity is where a herd component gets its SPIFFE identity (ADR 0001 A-60): by default the SPIFFE Workload API
// (-spiffe-socket, else SPIFFE_ENDPOINT_SOCKET, else auth.DefaultWorkloadAPI), whose stream rotates the SVID and the
// bundles. -tls-cert, -tls-key and -spiffe-bundle (with -spiffe-jwt-bundle) override it on a cluster without a
// Workload API. A component that gets no identity does not start.
type Identity struct {
	Socket      string
	Wait        time.Duration
	TLSCert     string
	TLSKey      string
	Bundle      string
	JWTBundle   string
	TrustDomain string
	// Source, if set, is the identity instead (herd all-in-one's local CA, tests).
	Source auth.Source
}

// DefaultSPIFFEWait is how long a component waits for the Workload API's first SVID before it refuses to start.
const DefaultSPIFFEWait = 30 * time.Second

// Flags registers the identity flags.
func (i *Identity) Flags(fs *flag.FlagSet) {
	fs.StringVar(&i.Socket, "spiffe-socket", "", "SPIFFE Workload API address (default: $SPIFFE_ENDPOINT_SOCKET, else "+auth.DefaultWorkloadAPI+"); the process's X.509-SVID, trust bundle and JWT bundles come from it and rotate with it")
	fs.DurationVar(&i.Wait, "spiffe-wait", DefaultSPIFFEWait, "how long to wait for the Workload API's first SVID before refusing to start")
	fs.StringVar(&i.TLSCert, "tls-cert", "", "override for a cluster without a Workload API: this process's X.509-SVID chain (PEM), with -tls-key and -spiffe-bundle; read again when it changes")
	fs.StringVar(&i.TLSKey, "tls-key", "", "override: the private key of -tls-cert (PEM)")
	fs.StringVar(&i.Bundle, "spiffe-bundle", "", "override: the trust bundle, PEM X.509 authorities or a SPIFFE bundle JWKS (spire-server bundle show -format spiffe); read again when it changes")
	fs.StringVar(&i.JWTBundle, "spiffe-jwt-bundle", "", "override: a JWKS of JWT-SVID authorities beside a PEM -spiffe-bundle")
	fs.StringVar(&i.TrustDomain, "spiffe-trust-domain", "", "override check: the trust domain this process's SVID must be of (default: whatever it is)")
}

// Open returns the identity, or why there is none: who refuses to start then.
func (i *Identity) Open(ctx context.Context, who string) (auth.Source, func(), error) {
	var src auth.Source
	closer := func() {}
	switch {
	case i.Source != nil:
		src = i.Source
	case i.TLSCert != "" || i.TLSKey != "" || i.Bundle != "" || i.JWTBundle != "":
		f, err := auth.Files(i.TLSCert, i.TLSKey, i.Bundle, i.JWTBundle)
		if err != nil {
			return nil, nil, fmt.Errorf("%s has no SPIFFE identity, so it does not start: -tls-cert, -tls-key, -spiffe-bundle: %w", who, err)
		}
		src = f
	default:
		addr := i.Socket
		if addr == "" {
			addr = auth.WorkloadAPIAddr()
		}
		wait := i.Wait
		if wait <= 0 {
			wait = DefaultSPIFFEWait
		}
		w, err := auth.WorkloadAPI(ctx, addr, wait)
		if err != nil {
			return nil, nil, fmt.Errorf("%s has no SPIFFE identity, so it does not start: %w. Run a SPIRE agent (or another "+
				"Workload API) there, point -spiffe-socket or SPIFFE_ENDPOINT_SOCKET at one, or, on a cluster without a Workload API, "+
				"give -tls-cert, -tls-key and -spiffe-bundle", who, err)
		}
		src, closer = w, func() { _ = w.Close() }
	}
	self, err := auth.Self(src)
	if err != nil {
		closer()
		return nil, nil, fmt.Errorf("%s has no X.509-SVID, so it does not start: %w", who, err)
	}
	if i.TrustDomain != "" && self.TrustDomain().Name() != i.TrustDomain {
		closer()
		return nil, nil, fmt.Errorf("%s: its SVID %s is not of -spiffe-trust-domain %q", who, self, i.TrustDomain)
	}
	log.Printf("%s: SPIFFE identity %s", who, self)
	return src, closer, nil
}

// IdentityCheck is how often a component checks that its SVID still names the identity it started as.
var IdentityCheck = 5 * time.Second

// ErrIdentityChanged is why a component stops when its Workload API rotates it to another SPIFFE ID.
var ErrIdentityChanged = errors.New("the SPIFFE identity changed")

// pinIdentity holds a component to the SPIFFE ID it started as (review 8): it checks src every IdentityCheck, and when
// the SVID names another ID it logs why and cancels the returned context. changed answers the error the component
// stops with then (nil while the identity holds), so it exits non-zero and its supervisor starts it again as what it
// now is. A rotation of the same ID is the Workload API's normal stream.
func pinIdentity(ctx context.Context, src auth.Source, who string) (context.Context, func() error) {
	start, err := auth.Self(src)
	ctx, cancel := context.WithCancelCause(ctx)
	if err != nil {
		cancel(err)
		return ctx, func() error { return context.Cause(ctx) }
	}
	go func() {
		t := time.NewTicker(IdentityCheck)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			now, err := auth.Self(src)
			if err == nil && now != start {
				err = fmt.Errorf("%w: %s started as %s, and the Workload API now names it %s; it stops so that it starts again as what it is", ErrIdentityChanged, who, start, now)
				log.Printf("%v", err)
				cancel(err)
				return
			}
		}
	}()
	return ctx, func() error {
		if err := context.Cause(ctx); errors.Is(err, ErrIdentityChanged) {
			return err
		}
		return nil
	}
}

// requireID refuses a component whose SVID is not the herd path its peers dial it as.
func requireID(who string, src auth.Source, segs ...string) (spiffeid.ID, error) {
	self, err := auth.Self(src)
	if err != nil {
		return spiffeid.ID{}, err
	}
	want := auth.HerdID(self.TrustDomain(), segs...)
	if self != want {
		return spiffeid.ID{}, fmt.Errorf("%s's SVID is %s; herd's components reach it as %s, so it does not start with another", who, self, want)
	}
	return self, nil
}

// MaxBreakGlass is how far from its start a break-glass allowance may end: at most 30 days (the day named is counted
// whole).
const MaxBreakGlass = 30 * 24 * time.Hour

// BreakGlass is the dated HS256 allowance (ADR 0001 A-60): HS256 bearers signed with -key-file are accepted, and
// counted, only with -break-glass-hs256-until, and only until the end of that day (UTC); a component started on or
// after it refuses to start.
type BreakGlass struct {
	Until    string
	KeyFile  string
	Audience string
	// Key, if set, is used instead of KeyFile.
	Key []byte
	// Now is the clock the date is read against (default time.Now).
	Now func() time.Time
}

// Flags registers the break-glass flags.
func (b *BreakGlass) Flags(fs *flag.FlagSet) {
	fs.StringVar(&b.Until, "break-glass-hs256-until", "", "accept HS256 bearers signed with -key-file, counted, until the end of this day (UTC, YYYY-MM-DD); past it the process refuses to start. Without it HS256 bearers are refused")
	fs.StringVar(&b.KeyFile, "key-file", "", "HS256 key of the break-glass bearers and join tokens (only with -break-glass-hs256-until)")
	fs.StringVar(&b.Audience, "audience", "", "required audience of the break-glass HS256 bearers, if any")
}

// open returns the allowance's key and end, or none (nil key) without the flag.
func (b *BreakGlass) open(who string) ([]byte, time.Time, error) {
	if b.Until == "" {
		if b.KeyFile != "" || b.Key != nil {
			return nil, time.Time{}, fmt.Errorf("%s: -key-file is the HS256 break-glass key; it needs -break-glass-hs256-until <YYYY-MM-DD>", who)
		}
		return nil, time.Time{}, nil
	}
	day, err := time.Parse("2006-01-02", b.Until)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%s: -break-glass-hs256-until %q: want YYYY-MM-DD", who, b.Until)
	}
	until := day.Add(24 * time.Hour)
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	if until.After(now().Add(MaxBreakGlass + 24*time.Hour)) {
		return nil, time.Time{}, fmt.Errorf("%s: -break-glass-hs256-until %s is more than %d days away: an allowance is at most 30 days from its start", who, b.Until, int(MaxBreakGlass/(24*time.Hour)))
	}
	if !now().Before(until) {
		return nil, time.Time{}, fmt.Errorf("%s: -break-glass-hs256-until %s has passed (it ended %s), so it does not start: move the HS256 callers to SVIDs and remove the flag, or name a new end date",
			who, b.Until, until.UTC().Format(time.RFC3339))
	}
	key := b.Key
	if key == nil {
		if key, err = ReadKey(b.KeyFile); err != nil {
			return nil, time.Time{}, fmt.Errorf("%s: -break-glass-hs256-until needs -key-file: %w", who, err)
		}
	}
	log.Printf("%s: HS256 break-glass bearers are accepted, and counted, until %s", who, until.UTC().Format(time.RFC3339))
	return key, until, nil
}

// DefaultPolicy is where herd-api and herd-router read the policy when -spiffe-policy is not given.
const DefaultPolicy = "/etc/herd/spiffe-policy.json"

// policyFile is the policy path: path, or DefaultPolicy. A missing file refuses the start: without it no workload
// (Everdict's runners and control plane) gets an atespace grant, which an operator says on purpose with a file of no
// rules, never by omission.
func policyFile(who, path string) (string, error) {
	if path == "" {
		path = DefaultPolicy
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%s: the SPIFFE policy %s: %w. Without it no workload gets an atespace grant, so it does not start: "+
			`write the workloads' grants there (-spiffe-policy names another file), or {"rules": []} to admit only herd's own nodes, egress and operators`, who, path, err)
	}
	return path, nil
}

// authenticator is the auth.Chain of a component on src: SVIDs whose JWT audience is audience, granted by the policy
// at policy, and the dated HS256 allowance if bg grants one; its calls counted on m.
func authenticator(who string, src auth.Source, audience spiffeid.ID, policy string, bg *BreakGlass, m *auth.Meter) (auth.Chain, []byte, error) {
	s, err := auth.NewSPIFFE(auth.SPIFFEConfig{Source: src, Audience: audience.String(), PolicyFile: policy, Meter: m})
	if err != nil {
		return auth.Chain{}, nil, err
	}
	c := auth.Chain{SVID: s, Meter: m, Component: who}
	key, until, err := bg.open(who)
	if err != nil {
		return auth.Chain{}, nil, err
	}
	if key != nil {
		c.BreakGlass, c.BreakGlassUntil = auth.StaticKey{Key: key, Audience: bg.Audience}, until
	}
	return c, key, nil
}

// serverCreds are a herd gRPC listener's TLS (auth.ServerTLS) and its per-connection drain (auth.ServerCreds): a
// connection is authenticated at its handshake, and drains before its client's X.509-SVID ends, its calls and streams
// finishing first (ADR 0001 A-60).
func serverCreds(src auth.Source) grpc.ServerOption {
	return grpc.Creds(auth.ServerCreds(auth.ServerTLS(src)))
}

// serverConn is serverCreds with the interceptors that count each connection's work (auth.ConnInterceptors).
func serverConn(src auth.Source) []grpc.ServerOption {
	return append([]grpc.ServerOption{serverCreds(src)}, auth.ConnInterceptors()...)
}

// dial connects to a herd component at addr as src, accepting only the server SVID id.
func dial(addr string, src auth.Source, id spiffeid.ID, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	cfg, err := auth.ClientTLS(src, id)
	if err != nil {
		return nil, err
	}
	return grpc.NewClient("passthrough:///"+addr, append([]grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(cfg))}, opts...)...)
}

// serveMetrics serves m's counters at /metrics on ln until ctx ends.
func serveMetrics(ctx context.Context, ln net.Listener, m *auth.Meter, who string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = hs.Close()
	}()
	log.Printf("%s: metrics on http://%s/metrics", who, ln.Addr())
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("%s: metrics: %v", who, err)
	}
}

// DefaultAPIMetrics and DefaultRouterMetrics are where herd-api and herd-router serve /metrics by default: the loopback
// address, since its labels carry identities; an explicit -metrics-listen exposes it.
const (
	DefaultAPIMetrics    = "127.0.0.1:7449"
	DefaultRouterMetrics = "127.0.0.1:8449"
)

// redialConn is a client connection to a herd component that, when a call is answered Unavailable (the server drains
// the connection before its client's SVID ends) or Unauthenticated (a connection authenticated with an SVID that has
// since been replaced), dials a new connection, which presents the current SVID, and tries the call once more on it
// (re-review 2). Streams go to the current connection as they are.
type redialConn struct {
	dialFn func() (*grpc.ClientConn, error)
	mu     sync.Mutex
	cc     *grpc.ClientConn
}

// redialing dials addr as src, expecting id, with redialConn's retry.
func redialing(addr string, src auth.Source, id spiffeid.ID, opts ...grpc.DialOption) (*redialConn, error) {
	r := &redialConn{dialFn: func() (*grpc.ClientConn, error) { return dial(addr, src, id, opts...) }}
	cc, err := r.dialFn()
	if err != nil {
		return nil, err
	}
	r.cc = cc
	return r, nil
}

func (r *redialConn) current() *grpc.ClientConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cc
}

func (r *redialConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	cc := r.current()
	err := cc.Invoke(ctx, method, args, reply, opts...)
	if c := status.Code(err); c != codes.Unavailable && c != codes.Unauthenticated {
		return err
	}
	r.mu.Lock()
	if r.cc == cc {
		if n, derr := r.dialFn(); derr == nil {
			_ = r.cc.Close()
			r.cc = n
		}
	}
	cc = r.cc
	r.mu.Unlock()
	return cc.Invoke(ctx, method, args, reply, opts...)
}

func (r *redialConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return r.current().NewStream(ctx, desc, method, opts...)
}

// Close closes the current connection.
func (r *redialConn) Close() error { return r.current().Close() }
