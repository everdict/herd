// Command herd-router is resume-on-request ingress for herd actors (spec R3). Clients send requests naming
// "ate-target-actor: <atespace>/<actor>" with a bearer token; herdlets dial in and hold the tunnels requests reach
// their VMs through. The router dials only herd-api, never a node.
//
//	herd-router -listen 0.0.0.0:8443 -tunnel-listen 0.0.0.0:7444 -api 127.0.0.1:7443 -key-file /etc/herd/jwt.key
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/router"
	"github.com/everdict/herd/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatalf("herd-router: %v", err)
	}
}

func readKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("-key-file is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k := []byte(strings.TrimSpace(string(b)))
	if len(k) < 32 {
		return nil, fmt.Errorf("%s: the key must be at least 32 bytes", path)
	}
	return k, nil
}

func run(args []string) error {
	fs := flag.NewFlagSet("herd-router", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8443", "address clients connect to (HTTP/1.1 and h2c)")
	tunnelListen := fs.String("tunnel-listen", "127.0.0.1:7444", "address herdlets open their tunnels to (gRPC)")
	api := fs.String("api", "127.0.0.1:7443", "herd-api address")
	keyFile := fs.String("key-file", "", "HS256 key bearer tokens are signed with (herd-api's)")
	audience := fs.String("audience", "", "required JWT audience, if any")
	budget := fs.Duration("parking-budget", 30*time.Second, "how long a request waits for its actor to become reachable")
	port := fs.Uint("default-port", 8080, "guest port ordinary requests go to (herd-guest's API)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key, err := readKey(*keyFile)
	if err != nil {
		return err
	}
	if *port == 0 || *port > 65535 {
		return fmt.Errorf("-default-port %d", *port)
	}
	authn := auth.StaticKey{Key: key, Audience: *audience}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The one connection the router makes: to herd-api.
	cc, err := grpc.NewClient("passthrough:///"+*api, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer cc.Close()
	tunnels := &tunnel.Server{Authn: authn}
	rt, err := router.New(router.Config{
		Control: router.GRPCControl{CC: cc}, Tunnels: tunnels, Authn: authn, Authz: auth.SpaceRoles{},
		DefaultPort: uint32(*port), ParkingBudget: *budget,
	})
	if err != nil {
		return err
	}
	// Herdlets behind NAT keep their tunnel alive with pings; allow them every 10 s even with no stream open.
	g := grpc.NewServer(grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}))
	tunnels.Register(g)
	tl, err := net.Listen("tcp", *tunnelListen)
	if err != nil {
		return err
	}
	cl, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	hs := router.NewHTTPServer(rt)
	errc := make(chan error, 2)
	go func() { errc <- g.Serve(tl) }()
	go func() { errc <- hs.Serve(cl) }()
	log.Printf("herd-router: clients on %s, herdlet tunnels on %s, herd-api at %s", cl.Addr(), tl.Addr(), *api)
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	g.Stop()
	_ = hs.Close()
	rt.Wait()
	return err
}
