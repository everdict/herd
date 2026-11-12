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
	"time"

	"github.com/everdict/herd/auth"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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
	s, err := auth.NewSPIFFE(auth.SPIFFEConfig{Source: src, Audience: audience.String(), PolicyFile: policy})
	if err != nil {
		return auth.Chain{}, nil, err
	}
	c := auth.Chain{SVID: s, Meter: m}
	key, until, err := bg.open(who)
	if err != nil {
		return auth.Chain{}, nil, err
	}
	if key != nil {
		c.BreakGlass, c.BreakGlassUntil = auth.StaticKey{Key: key, Audience: bg.Audience}, until
	}
	return c, key, nil
}

// serverCreds is a herd gRPC listener's TLS (auth.ServerTLS).
func serverCreds(src auth.Source) grpc.ServerOption {
	return grpc.Creds(credentials.NewTLS(auth.ServerTLS(src)))
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

// DefaultAPIMetrics and DefaultRouterMetrics are where herd-api and herd-router serve /metrics by default.
const (
	DefaultAPIMetrics    = ":7449"
	DefaultRouterMetrics = ":8449"
)
