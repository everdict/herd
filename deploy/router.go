package deploy

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/router"
	"github.com/everdict/herd/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// Router is herd-router's configuration.
type Router struct {
	Listen        string
	TunnelListen  string
	API           string
	ParkingBudget time.Duration
	DefaultPort   uint
	// Identity is herd-router's own SPIFFE identity (spiffe://<td>/herd/router), the Workload API's by default. Both
	// listeners serve TLS with it, and it dials herd-api as itself. A client is the bearer it carries (a JWT-SVID for
	// herd-api, which the router forwards); a herdlet's tunnel is its node's X.509-SVID.
	Identity Identity
	// Policy grants workloads atespace roles (-spiffe-policy, default DefaultPolicy, which must exist).
	Policy string
	// BreakGlass is the dated HS256 allowance, as herd-api's.
	BreakGlass BreakGlass
	// MetricsListen serves the router's allowance counters at /metrics (default DefaultRouterMetrics in the flags;
	// empty: not served).
	MetricsListen string
}

// Flags registers herd-router's flags on fs.
func (r *Router) Flags(fs *flag.FlagSet) {
	fs.StringVar(&r.Listen, "listen", "127.0.0.1:8443", "address clients connect to (HTTP/1.1 and HTTP/2 over TLS, with herd-router's SVID)")
	fs.StringVar(&r.TunnelListen, "tunnel-listen", "127.0.0.1:7444", "address herdlets open their tunnels to (gRPC over TLS; a tunnel is its node's X.509-SVID)")
	fs.StringVar(&r.API, "api", "127.0.0.1:7443", "herd-api address (dialed over TLS as herd-router, expecting spiffe://<td>/herd/api)")
	fs.DurationVar(&r.ParkingBudget, "parking-budget", 30*time.Second, "how long a request waits for its actor to become reachable")
	fs.UintVar(&r.DefaultPort, "default-port", 8080, "guest port ordinary requests go to (herd-guest's API)")
	r.Identity.Flags(fs)
	r.BreakGlass.Flags(fs)
	fs.StringVar(&r.Policy, "spiffe-policy", "", "JSON policy granting workloads atespace roles, as herd-api's (default "+DefaultPolicy+", which must exist)")
	fs.StringVar(&r.MetricsListen, "metrics-listen", DefaultRouterMetrics, "serve the authentication allowance counters at http://<addr>/metrics (empty: not served)")
}

// Run serves clients on cl and herdlets' tunnels on tl until ctx ends. Its one outbound connection is to herd-api.
func (r *Router) Run(ctx context.Context, cl, tl net.Listener) error {
	if r.DefaultPort == 0 || r.DefaultPort > 65535 {
		return fmt.Errorf("-default-port %d", r.DefaultPort)
	}
	src, closeID, err := r.Identity.Open(ctx, "herd-router")
	if err != nil {
		return err
	}
	defer closeID()
	self, err := requireID("herd-router", src, "router")
	if err != nil {
		return err
	}
	policy, err := policyFile("herd-router", r.Policy)
	if err != nil {
		return err
	}
	meter := auth.NewMeter()
	// The router forwards a client's bearer to herd-api, so the JWT-SVID it checks is one for herd-api.
	apiID := auth.HerdID(self.TrustDomain(), "api")
	authn, _, err := authenticator("herd-router", src, apiID, policy, &r.BreakGlass, meter)
	if err != nil {
		return err
	}
	if r.MetricsListen != "" {
		mln, err := net.Listen("tcp", r.MetricsListen)
		if err != nil {
			return err
		}
		go serveMetrics(ctx, mln, meter, "herd-router")
	}
	cc, err := dial(r.API, src, apiID)
	if err != nil {
		return err
	}
	defer cc.Close()
	tunnels := &tunnel.Server{Authn: authn}
	rt, err := router.New(router.Config{
		Control: router.GRPCControl{CC: cc}, Tunnels: tunnels, Authn: authn, Authz: auth.SpaceRoles{}, Meter: meter,
		DefaultPort: uint32(r.DefaultPort), ParkingBudget: r.ParkingBudget,
	})
	if err != nil {
		return err
	}
	// Herdlets behind NAT keep their tunnel alive with pings; allow them every 10 s even with no stream open.
	g := grpc.NewServer(serverCreds(src), grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}))
	tunnels.Register(g)
	hs := router.NewHTTPServer(rt)
	hs.Protocols.SetHTTP2(true)
	hs.TLSConfig = auth.ServerTLS(src)
	hs.TLSConfig.NextProtos = []string{"h2", "http/1.1"}
	errc := make(chan error, 2)
	go func() { errc <- g.Serve(tl) }()
	go func() { errc <- hs.ServeTLS(cl, "", "") }()
	log.Printf("herd-router: clients on %s, herdlet tunnels on %s, herd-api at %s", cl.Addr(), tl.Addr(), r.API)
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	g.Stop()
	_ = hs.Close()
	rt.Wait()
	return err
}
