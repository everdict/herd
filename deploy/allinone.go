package deploy

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/join"
)

// AllInOne is `herd`: herd-api on a bbolt store, herd-router, herd-egress and one herdlet in one process. Each is the
// component the separate binaries run, configured from the data directory; the herdlet joins this herd-api over its
// gRPC port with a join token the process mints, exactly as `herdlet join` does on another host.
type AllInOne struct {
	Data         string
	Listen       string
	RouterListen string
	TunnelListen string
	EgressListen string
	DNSListen    string
	Egress       bool
	Herdlet      Herdlet
	// S3, S3Endpoint and S3Region keep snapshots in a bucket instead of <data>/snapshots (API.S3), so hosts that
	// join this herd-api move actors without a shared filesystem.
	S3, S3Endpoint, S3Region string
	// Secrets is herd-egress's -secrets, and on a Mac the herdlet's (Herdlet.Secrets): the directory they resolve
	// ate-secret://dir/<name>/<tail> credentials from.
	Secrets string
	// HostServicesFile is herd-egress's -host-services-file.
	HostServicesFile string
	// GoldenWarmup is herd-api's -golden-warmup.
	GoldenWarmup time.Duration

	// NewGateway, if set, is herd-egress's Egress.NewGateway.
	NewGateway func(egress.Config) (*egress.Gateway, error)
	// OnJoin, if set, sees the grant herd-api gave the herdlet.
	OnJoin func(join.Grant)
}

// Addrs are where an all-in-one serves.
type Addrs struct {
	API, Router, Tunnel, Egress, DNS string
}

// Flags registers herd's flags: its own, and the herdlet's except those the data directory decides.
func (a *AllInOne) Flags(fs *flag.FlagSet) {
	fs.StringVar(&a.Data, "data", "/var/lib/herd", "data directory: jwt.key, herd.db (bbolt), snapshots, herdlet state")
	fs.StringVar(&a.Listen, "listen", "127.0.0.1:7443", "herd-api address (gRPC)")
	fs.StringVar(&a.RouterListen, "router-listen", "127.0.0.1:8443", "herd-router client address (HTTP/1.1 and h2c)")
	fs.StringVar(&a.TunnelListen, "tunnel-listen", "127.0.0.1:7444", "herd-router tunnel address")
	fs.StringVar(&a.EgressListen, "egress-listen", ":15001", "herd-egress transparent TCP listener")
	fs.StringVar(&a.DNSListen, "dns-listen", ":15053", "herd-egress synthetic DNS listener (UDP)")
	fs.BoolVar(&a.Egress, "egress", true, "run herd-egress")
	fs.StringVar(&a.Secrets, "secrets", "", "herd-egress (and on a Mac the herdlet's in-process one): directory for ate-secret://dir/<name>/<tail> credentials")
	fs.StringVar(&a.HostServicesFile, "host-services-file", "", "herd-egress: "+hostServicesUsage)
	fs.StringVar(&a.S3, "s3", "", "keep snapshots in this bucket instead of <data>/snapshots, s3://bucket[/prefix] (credentials: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY)")
	fs.StringVar(&a.S3Endpoint, "s3-endpoint", "", "S3-compatible endpoint, http(s)://host[:port] (default: AWS for -s3-region)")
	fs.StringVar(&a.S3Region, "s3-region", "us-east-1", "S3 region requests are signed for")
	fs.DurationVar(&a.GoldenWarmup, "golden-warmup", DefaultGoldenWarmup, goldenWarmupUsage)
	hf := flag.NewFlagSet("herdlet", flag.ContinueOnError)
	a.Herdlet.HostFlags(hf)
	hf.VisitAll(func(f *flag.Flag) {
		switch f.Name {
		case "state", "snapshots", "router", "egress-bindings", "egress-ca", "egress-proxy-port", "egress-dns-port", "secrets":
			return
		}
		fs.Var(f.Value, f.Name, f.Usage)
	})
}

// Run serves until ctx ends or a component fails; ready, if set, gets the addresses once everything listens.
func (a *AllInOne) Run(ctx context.Context, ready func(Addrs)) error {
	if a.Data == "" {
		return errors.New("-data is required")
	}
	if err := os.MkdirAll(a.Data, 0o700); err != nil {
		return err
	}
	key, err := EnsureKey(filepath.Join(a.Data, "jwt.key"))
	if err != nil {
		return err
	}
	snapDir := filepath.Join(a.Data, "snapshots")
	var closers []func() error
	defer func() {
		for _, c := range closers {
			_ = c()
		}
	}()
	listen := func(addr string) (net.Listener, error) {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			closers = append(closers, ln.Close)
		}
		return ln, err
	}
	apiLn, err := listen(a.Listen)
	if err != nil {
		return err
	}
	cl, err := listen(a.RouterListen)
	if err != nil {
		return err
	}
	tl, err := listen(a.TunnelListen)
	if err != nil {
		return err
	}
	addrs := Addrs{API: apiLn.Addr().String(), Router: cl.Addr().String(), Tunnel: tl.Addr().String()}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errc := make(chan error, 4)
	start := func(name string, f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil {
				errc <- fmt.Errorf("%s: %w", name, err)
			}
			cancel()
		}()
	}
	defer func() {
		cancel()
		wg.Wait()
	}()

	api := &API{Key: key, Store: "bolt:" + filepath.Join(a.Data, "herd.db"), Snapshots: snapDir,
		S3: a.S3, S3Endpoint: a.S3Endpoint, S3Region: a.S3Region,
		CallTimeout: 30 * time.Second, Reconcile: 10 * time.Second, AdvertiseTunnel: addrs.Tunnel,
		CredentialTTL: 365 * 24 * time.Hour, OnJoin: a.OnJoin, GoldenWarmup: a.GoldenWarmup}
	start("herd-api", func() error { return api.Run(ctx, apiLn) })
	rt := &Router{Key: key, API: addrs.API, ParkingBudget: 30 * time.Second, DefaultPort: 8080}
	start("herd-router", func() error { return rt.Run(ctx, cl, tl) })

	// The herdlet joins like any other host: one join token, exchanged at herd-api for its credentials.
	tok, err := join.MintToken(key, 10*time.Minute)
	if err != nil {
		return err
	}
	h := a.Herdlet
	h.API, h.Token, h.TokenFile = addrs.API, tok, ""
	h.StateDir, h.Snapshots, h.Router, h.CredentialFile, h.JoinOnly = filepath.Join(a.Data, "herdlet"), snapDir, "", "", false
	// On a Mac the VMs' gateway is the herdlet's own herd-egress (ADR A-21): it resolves credentials from -secrets too.
	h.Secrets = a.Secrets
	// The herdlet binds its VMs in this herd-egress's bindings file and gives the guests its CA, which is kept in the
	// data directory so a restart keeps the CA the guests trust.
	caCert, caKey := filepath.Join(a.Data, "egress-ca.pem"), filepath.Join(a.Data, "egress-ca.key")
	h.EgressBindings, h.EgressCA = filepath.Join(a.Data, "egress-bindings.json"), caCert
	if a.S3 != "" {
		// The herdlet uses the store its join hands it, as every other host does.
		h.Snapshots = ""
	}
	// A grant an earlier run left names the store herd-api handed out then; one that names another than now (-s3
	// added, dropped or changed) is joined again (PLT-382).
	granted, err := api.grantedStore()
	if err != nil {
		return err
	}
	h.granted = &granted
	if err := h.preflight(); err != nil {
		return err
	}
	st, err := h.Join(ctx)
	if err != nil {
		return err
	}
	if a.Egress {
		eln, err := listen(a.EgressListen)
		if err != nil {
			return err
		}
		pc, err := net.ListenPacket("udp", a.DNSListen)
		if err != nil {
			return err
		}
		closers = append(closers, pc.Close)
		addrs.Egress, addrs.DNS = eln.Addr().String(), pc.LocalAddr().String()
		h.EgressProxyPort, h.EgressDNSPort = uint(eln.Addr().(*net.TCPAddr).Port), uint(pc.LocalAddr().(*net.UDPAddr).Port)
		if err := ensureCA(caCert, caKey); err != nil {
			return fmt.Errorf("herd-egress CA: %w", err)
		}
		eg := &Egress{API: addrs.API, Token: st.Grant.EgressCredential, Bindings: h.EgressBindings,
			CACert: caCert, CAKey: caKey, DNSAnswer: "198.18.0.1", Interval: time.Second,
			Secrets: a.Secrets, AllowPrefixes: h.EgressAllow, NewGateway: a.NewGateway,
			HostServicesFile: a.HostServicesFile, Routers: list{addrs.Router, addrs.Tunnel}}
		start("herd-egress", func() error { return eg.Run(ctx, eln, pc) })
	}
	start("herdlet", func() error { return h.Run(ctx) })
	if ready != nil {
		ready(addrs)
	}
	<-ctx.Done()
	select {
	case err := <-errc:
		return err
	default:
		return nil
	}
}
