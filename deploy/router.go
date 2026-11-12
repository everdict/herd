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
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// Router is herd-router's configuration.
type Router struct {
	Listen        string
	TunnelListen  string
	API           string
	KeyFile       string
	Audience      string
	ParkingBudget time.Duration
	DefaultPort   uint
	// Identity authenticates clients by JWT-SVID bearers beside HS256 break-glass (ADR 0001 A-60). The router
	// forwards the client's bearer to herd-api, so a client is a bearer here: X.509-SVIDs are herd-api's -tls-listen.
	Identity Identity
	// MetricsListen serves the router's allowance counters at /metrics (empty: not served).
	MetricsListen string

	// Key, if set, is used instead of KeyFile.
	Key []byte
}

// Flags registers herd-router's flags on fs.
func (r *Router) Flags(fs *flag.FlagSet) {
	fs.StringVar(&r.Listen, "listen", "127.0.0.1:8443", "address clients connect to (HTTP/1.1 and h2c)")
	fs.StringVar(&r.TunnelListen, "tunnel-listen", "127.0.0.1:7444", "address herdlets open their tunnels to (gRPC)")
	fs.StringVar(&r.API, "api", "127.0.0.1:7443", "herd-api address")
	fs.StringVar(&r.KeyFile, "key-file", "", "HS256 key bearer tokens are signed with (herd-api's)")
	fs.StringVar(&r.Audience, "audience", "", "required JWT audience, if any")
	fs.DurationVar(&r.ParkingBudget, "parking-budget", 30*time.Second, "how long a request waits for its actor to become reachable")
	fs.UintVar(&r.DefaultPort, "default-port", 8080, "guest port ordinary requests go to (herd-guest's API)")
	r.Identity.Flags(fs)
	fs.StringVar(&r.MetricsListen, "metrics-listen", "", "serve the authentication allowance counters at http://<addr>/metrics")
}

// Run serves clients on cl and herdlets' tunnels on tl until ctx ends. Its one outbound connection is to herd-api.
func (r *Router) Run(ctx context.Context, cl, tl net.Listener) error {
	key := r.Key
	if key == nil {
		var err error
		if key, err = ReadKey(r.KeyFile); err != nil {
			return err
		}
	}
	if r.DefaultPort == 0 || r.DefaultPort > 65535 {
		return fmt.Errorf("-default-port %d", r.DefaultPort)
	}
	meter := auth.NewMeter()
	authn, err := r.Identity.Authenticator(key, r.Audience, meter)
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
	cc, err := grpc.NewClient("passthrough:///"+r.API, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer cc.Close()
	// A herdlet's tunnel credential is the HS256 one its join granted (link credentials are W-5's to move to SVIDs).
	tunnels := &tunnel.Server{Authn: auth.StaticKey{Key: key, Audience: r.Audience}}
	rt, err := router.New(router.Config{
		Control: router.GRPCControl{CC: cc}, Tunnels: tunnels, Authn: authn, Authz: auth.SpaceRoles{}, Meter: meter,
		DefaultPort: uint32(r.DefaultPort), ParkingBudget: r.ParkingBudget,
	})
	if err != nil {
		return err
	}
	// Herdlets behind NAT keep their tunnel alive with pings; allow them every 10 s even with no stream open.
	g := grpc.NewServer(grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}))
	tunnels.Register(g)
	hs := router.NewHTTPServer(rt)
	errc := make(chan error, 2)
	go func() { errc <- g.Serve(tl) }()
	go func() { errc <- hs.Serve(cl) }()
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
