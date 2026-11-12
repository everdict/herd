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

	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/auth/localca"
	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/join"
)

// LocalTrustDomain is herd all-in-one's trust domain.
const LocalTrustDomain = "herd.local"

// LocalOperator is the operator SVID all-in-one writes for the host's own clients (identity/operator.pem).
const LocalOperator = "spiffe://" + LocalTrustDomain + "/herd/operator/local"

// localIDs are all-in-one's components' identities and its policy file.
type localIDs struct {
	api, router, node, egress auth.Source
	policy                    string
}

// IdentityDir is where all-in-one keeps its local CA and the files a client of it uses: bundle.pem (the trust
// bundle), operator.pem and operator-key.pem (an X.509-SVID of LocalOperator), spiffe-policy.json.
func IdentityDir(data string) string { return filepath.Join(data, "identity") }

// identities issues all-in-one's SVIDs from the CA kept in IdentityDir: herd-api's and herd-router's with the loopback
// names a client that checks a host name dials (localhost, 127.0.0.1, ::1), the node's and its herd-egress's for
// worker, and an operator SVID for the host's clients, written with the bundle. A policy file of no rules is written
// if there is none: on one host, the operator SVID is the client.
func (a *AllInOne) identities(worker string) (localIDs, error) {
	dir := IdentityDir(a.Data)
	ca, err := localca.LoadOrCreate(dir, LocalTrustDomain)
	if err != nil {
		return localIDs{}, fmt.Errorf("the local SPIFFE CA in %s: %w", dir, err)
	}
	server := localca.Options{DNS: []string{"localhost"}, IPs: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, TTL: 365 * 24 * time.Hour}
	node := localca.Options{TTL: 365 * 24 * time.Hour}
	var out localIDs
	for _, c := range []struct {
		dst *auth.Source
		id  string
		o   localca.Options
	}{
		{&out.api, "spiffe://" + LocalTrustDomain + "/herd/api", server},
		{&out.router, "spiffe://" + LocalTrustDomain + "/herd/router", server},
		{&out.node, "spiffe://" + LocalTrustDomain + "/herd/node/" + worker, node},
		{&out.egress, "spiffe://" + LocalTrustDomain + "/herd/egress/" + worker, node},
	} {
		s, err := ca.Source(c.id, c.o)
		if err != nil {
			return localIDs{}, err
		}
		*c.dst = s
	}
	op, err := ca.SVID(LocalOperator, node)
	if err != nil {
		return localIDs{}, err
	}
	if _, _, err := localca.WriteSVID(dir, "operator", op); err != nil {
		return localIDs{}, err
	}
	if err := ca.WriteBundle(filepath.Join(dir, "bundle.pem")); err != nil {
		return localIDs{}, err
	}
	out.policy = filepath.Join(dir, "spiffe-policy.json")
	if _, err := os.Stat(out.policy); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(out.policy, []byte(`{"rules": []}`+"\n"), 0o600); err != nil {
			return localIDs{}, err
		}
	}
	return out, nil
}

// AllInOne is `herd`: herd-api on a bbolt store, herd-router, herd-egress and one herdlet in one process. Each is the
// component the separate binaries run, configured from the data directory; the herdlet joins this herd-api over its
// gRPC port by its node SVID, exactly as `herdlet join` does on another host. Its components' SVIDs come from a local
// SPIFFE CA kept in <data>/identity (trust domain LocalTrustDomain; package auth/localca), which also writes an
// operator SVID there for the host's own clients (ADR 0001 A-60).
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
	fs.StringVar(&a.Data, "data", "/var/lib/herd", "data directory: identity/ (the local SPIFFE CA, its bundle and an operator SVID), herd.db (bbolt), snapshots, herdlet state")
	fs.StringVar(&a.Listen, "listen", "127.0.0.1:7443", "herd-api address (gRPC)")
	fs.StringVar(&a.RouterListen, "router-listen", "127.0.0.1:8443", "herd-router client address (HTTP/1.1 and HTTP/2 over TLS)")
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
		case "state", "snapshots", "router", "egress-bindings", "egress-ca", "egress-proxy-port", "egress-dns-port", "secrets",
			"spiffe-socket", "spiffe-wait", "tls-cert", "tls-key", "spiffe-bundle", "spiffe-jwt-bundle", "spiffe-trust-domain":
			// all-in-one's components take their SVIDs from its local CA (identities).
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
	h := a.Herdlet
	if h.Worker == "" {
		h.Worker = defaultWorker()
	}
	ids, err := a.identities(h.Worker)
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

	api := &API{Identity: Identity{Source: ids.api}, Policy: ids.policy, Store: "bolt:" + filepath.Join(a.Data, "herd.db"), Snapshots: snapDir,
		S3: a.S3, S3Endpoint: a.S3Endpoint, S3Region: a.S3Region,
		CallTimeout: 30 * time.Second, Reconcile: 10 * time.Second, AdvertiseTunnel: addrs.Tunnel,
		CredentialTTL: 365 * 24 * time.Hour, OnJoin: a.OnJoin, GoldenWarmup: a.GoldenWarmup, SnapshotSweep: DefaultSnapshotSweep}
	start("herd-api", func() error { return api.Run(ctx, apiLn) })
	rt := &Router{Identity: Identity{Source: ids.router}, Policy: ids.policy, API: addrs.API, ParkingBudget: 30 * time.Second, DefaultPort: 8080}
	start("herd-router", func() error { return rt.Run(ctx, cl, tl) })

	// The herdlet joins like any other host: by its node SVID.
	h.Identity = Identity{Source: ids.node}
	h.API, h.Token, h.TokenFile = addrs.API, "", ""
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
	if _, err := h.Join(ctx); err != nil {
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
		eg := &Egress{API: addrs.API, Identity: Identity{Source: ids.egress}, Bindings: h.EgressBindings,
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
