// Package deploy is the one code path every deployment mode runs (spec R-D4, ADR 0001 A-20). Each component — herd-api,
// herd-router, herd-egress and herdlet — is an Options struct whose flags are registered by the component itself and
// whose Run serves it; the binaries (cmd/herd-api, cmd/herd-router, cmd/herd-egress, cmd/herdlet) only parse flags
// into them, and the all-in-one binary (cmd/herd) runs the same four in one process. A mode is configuration:
//
//   - all-in-one: `herd -data DIR` — herd-api on a bbolt store, herd-router, herd-egress and a herdlet that joins
//     that herd-api over loopback as the node SVID its local CA issues (ADR 0001 A-60);
//   - multi-host join: herd-api (+ herd-router) on one host, `herdlet join <herd-api>` (as the node's SVID) and
//     herd-egress on each node;
//   - Nomad: the same binaries as a herd-api service job and a herdlet system job (dist/nomad).
package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/everdict/herd/apiserver"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/join"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/snapstore/disk"
	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/store"
	"github.com/everdict/herd/store/boltstore"
	"github.com/everdict/herd/store/memstore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// ReadKey reads an HS256 key file (at least 32 bytes after trimming).
func ReadKey(path string) ([]byte, error) {
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

// EnsureKey reads the key at path, creating a random one (mode 0600) if the file does not exist.
func EnsureKey(path string) ([]byte, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(b[:])+"\n"), 0o600); err != nil {
			return nil, err
		}
	}
	return ReadKey(path)
}

// OpenStore opens a record store: memory or bolt:<path>.
func OpenStore(spec string) (store.Store, error) {
	switch {
	case spec == "memory":
		return memstore.New(), nil
	case strings.HasPrefix(spec, "bolt:"):
		return boltstore.Open(strings.TrimPrefix(spec, "bolt:"), 5*time.Second)
	}
	return nil, fmt.Errorf("-store %q: want memory or bolt:<path>", spec)
}

// API is herd-api's configuration.
type API struct {
	Listen    string
	Store     string
	Snapshots string
	// S3 (s3://bucket[/prefix]), if set, keeps snapshots in that bucket at S3Endpoint instead of the Snapshots
	// directory, and every join hands herdlets the store (join.Grant.Snapshots). S3Credentials default to
	// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN.
	S3            string
	S3Endpoint    string
	S3Region      string
	S3Credentials s3.Credentials
	CallTimeout   time.Duration
	// RestoreBandwidthMiB is apiserver.Config.RestoreBandwidth in MiB/s (-restore-bandwidth).
	RestoreBandwidthMiB float64
	Reconcile           time.Duration
	AdvertiseTunnel     string
	CredentialTTL       time.Duration
	GoldenWarmup        time.Duration
	// SnapshotSweep is how often herd-api removes the snapshot objects no record names (apiserver.SweepSnapshots;
	// -snapshot-sweep, 0: never); the first pass runs SnapshotSweepDelay after start.
	SnapshotSweep      time.Duration
	SnapshotSweepDelay time.Duration
	// Identity is herd-api's own SPIFFE identity (ADR 0001 A-60): the Workload API's by default. Listen serves
	// gRPC over TLS with it; a caller is its client X.509-SVID, or the JWT-SVID it carries as its bearer.
	Identity Identity
	// Policy grants workloads atespace roles (-spiffe-policy, default DefaultPolicy, which must exist).
	Policy string
	// BreakGlass is the dated HS256 allowance (-break-glass-hs256-until, -key-file, -audience): off by default.
	BreakGlass BreakGlass
	// MetricsListen serves the A-60 allowance counters at /metrics (-metrics-listen, default DefaultAPIMetrics in
	// the flags; empty: not served).
	MetricsListen string
	// MetricsListener, if set, is used instead of MetricsListen (tests).
	MetricsListener net.Listener
	// Meter, if set, is the meter herd-api counts on instead of a new one (tests).
	Meter *auth.Meter

	// OnJoin, if set, sees every join herd-api granted.
	OnJoin func(join.Grant)
}

// Flags registers herd-api's flags on fs.
func (a *API) Flags(fs *flag.FlagSet) {
	fs.StringVar(&a.Listen, "listen", "127.0.0.1:7443", "address to serve gRPC over TLS on, with herd-api's SVID: a caller is its client X.509-SVID or its JWT-SVID bearer")
	fs.StringVar(&a.Store, "store", "memory", "record store: memory or bolt:<path> (one herd-api per file)")
	fs.StringVar(&a.Snapshots, "snapshots", "", "snapshot store directory (all-in-one, or hosts that share it; see -s3)")
	fs.StringVar(&a.S3, "s3", "", "keep snapshots in this bucket instead, s3://bucket[/prefix]; joins hand herdlets the store (credentials: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY)")
	fs.StringVar(&a.S3Endpoint, "s3-endpoint", "", "S3-compatible endpoint, http(s)://host[:port] (default: AWS for -s3-region)")
	fs.StringVar(&a.S3Region, "s3-region", "us-east-1", "S3 region requests are signed for")
	fs.DurationVar(&a.CallTimeout, "call-timeout", 30*time.Second, "how long to wait for a herdlet's answer, or the snapshot store's, to work that moves no bytes (a snapshot or restore waits longer, by its size: -restore-bandwidth)")
	fs.Float64Var(&a.RestoreBandwidthMiB, "restore-bandwidth", apiserver.DefaultRestoreBandwidth>>20, "MiB/s a herdlet is expected to move a snapshot object to or from the store at, at least: a restore's call waits -call-timeout plus the object's size at this rate, and a snapshot's call plus the size its herdlet reports")
	fs.DurationVar(&a.Reconcile, "reconcile", 10*time.Second, "how often to retry owed workflow steps")
	fs.StringVar(&a.AdvertiseTunnel, "advertise-tunnel", "", "herd-router tunnel address handed to herdlets that join")
	fs.DurationVar(&a.CredentialTTL, "credential-ttl", 365*24*time.Hour, "lifetime of the credentials a join grants")
	fs.DurationVar(&a.GoldenWarmup, "golden-warmup", DefaultGoldenWarmup, goldenWarmupUsage)
	a.Identity.Flags(fs)
	a.BreakGlass.Flags(fs)
	fs.StringVar(&a.Policy, "spiffe-policy", "", "JSON policy granting workloads (SPIFFE IDs, exact or by path prefix) atespace roles read|write|operator (default "+DefaultPolicy+", which must exist; herd's own nodes, egress and operators are built in); read again when it changes")
	fs.StringVar(&a.MetricsListen, "metrics-listen", DefaultAPIMetrics, "serve the authentication allowance counters at http://<addr>/metrics (empty: not served)")
	fs.DurationVar(&a.SnapshotSweep, "snapshot-sweep", DefaultSnapshotSweep, "how often to remove the snapshot objects no actor, tag or template record names (older than an hour) and abort writes unfinished for a day; the first pass runs 10 minutes after start (0: never)")
}

// DefaultSnapshotSweep is -snapshot-sweep's default.
const DefaultSnapshotSweep = 24 * time.Hour

// DefaultSnapshotSweepDelay is how long after start herd-api's first snapshot sweep runs: after the herdlets have
// linked and Reconcile has settled what a restart left owed.
const DefaultSnapshotSweepDelay = 10 * time.Minute

// DefaultGoldenWarmup is Substrate's wait between starting a template's golden actor and taking its snapshot.
const DefaultGoldenWarmup = 20 * time.Second

const goldenWarmupUsage = "how long a template's golden actor runs before its golden snapshot is taken (0: at once)"

// Run serves herd-api on ln until ctx ends. It never dials a herdlet.
func (a *API) Run(ctx context.Context, ln net.Listener) error {
	src, closeID, err := a.Identity.Open(ctx, "herd-api")
	if err != nil {
		return err
	}
	defer closeID()
	self, err := requireID("herd-api", src, "api")
	if err != nil {
		return err
	}
	policy, err := policyFile("herd-api", a.Policy)
	if err != nil {
		return err
	}
	meter := a.Meter
	if meter == nil {
		meter = auth.NewMeter()
	}
	// A JWT-SVID for herd-api names herd-api's own SPIFFE ID as its audience.
	authn, key, err := authenticator("herd-api", src, self, policy, &a.BreakGlass, meter)
	if err != nil {
		return err
	}
	st, err := OpenStore(a.Store)
	if err != nil {
		return err
	}
	defer st.Close()
	snaps, s3cfg, err := a.snapshots()
	if err != nil {
		return err
	}
	mln := a.MetricsListener
	if mln == nil && a.MetricsListen != "" {
		if mln, err = net.Listen("tcp", a.MetricsListen); err != nil {
			return err
		}
	}
	hub := link.NewHub()
	srv, err := apiserver.New(apiserver.Config{
		Store: st, Snapshots: snaps, Hub: hub, Authn: authn, Authz: auth.SpaceRoles{}, Meter: meter, CallTimeout: a.CallTimeout,
		RestoreBandwidth: int64(a.RestoreBandwidthMiB * (1 << 20)), GoldenWarmup: a.GoldenWarmup,
		SnapshotStore: SnapshotStoreName(s3cfg),
	})
	if err != nil {
		return err
	}
	log.Printf("herd-api: record store %s, store id %s", a.Store, srv.StoreID())
	// A node joins by its SVID; a join token is HS256, so only under the break-glass allowance (key set).
	js := &join.Server{OnGrant: a.OnJoin, Key: key, Authn: authn, Tunnel: a.AdvertiseTunnel, Snapshots: s3cfg, TTL: a.CredentialTTL,
		Connected: func(w string) bool {
			for _, c := range hub.Workers() {
				if c.Worker == w {
					return true
				}
			}
			return false
		}}
	// Herdlets behind NAT keep their link alive with pings (every 15 s); allow them every 10 s, as herd-router does.
	g := grpc.NewServer(serverCreds(src), grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}))
	srv.Register(g)
	js.Register(g)
	every := a.Reconcile
	if every <= 0 {
		every = 10 * time.Second
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if left, err := srv.Reconcile(ctx); err != nil {
					log.Printf("herd-api: reconcile: %v", err)
				} else if left > 0 {
					log.Printf("herd-api: reconcile: %d workflow steps still owed", left)
				}
			}
		}
	}()
	if a.SnapshotSweep > 0 {
		delay := a.SnapshotSweepDelay
		if delay <= 0 {
			delay = DefaultSnapshotSweepDelay
		}
		go sweepSnapshots(ctx, srv, delay, a.SnapshotSweep)
	}
	if mln != nil {
		go serveMetrics(ctx, mln, meter, "herd-api")
	}
	log.Printf("herd-api serving gRPC over TLS as %s on %s", self, ln.Addr())
	go func() {
		<-ctx.Done()
		g.Stop()
	}()
	err = g.Serve(ln)
	srv.Close()
	srv.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// sweepSnapshots runs srv's snapshot sweep after delay and then every interval until ctx ends. herd-api is one process
// per record store (bolt holds its file; A-55), so the pass has no other leader to defer to.
func sweepSnapshots(ctx context.Context, srv *apiserver.Server, delay, every time.Duration) {
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := srv.SweepSnapshots(ctx); err != nil {
			log.Printf("herd-api: %v", err)
		}
		t.Reset(every)
	}
}

// Token is `herd-api token`: it mints a bearer token offline from the key.
type Token struct {
	KeyFile  string
	Subject  string
	Audience string
	TTL      time.Duration
	Herdlet  bool
	Spaces   []string
}

// Flags registers the token command's flags.
func (t *Token) Flags(fs *flag.FlagSet) {
	fs.StringVar(&t.KeyFile, "key-file", "", "HS256 signing key")
	fs.StringVar(&t.Subject, "sub", "", "subject")
	fs.StringVar(&t.Audience, "audience", "", "audience")
	fs.DurationVar(&t.TTL, "ttl", 24*time.Hour, "lifetime")
	fs.BoolVar(&t.Herdlet, "herdlet", false, "a herdlet credential (may open the link, nothing else)")
	fs.Func("space", "atespace=read|write|operator (repeatable; * for every space; operator may also write actors it did not create)", func(v string) error {
		t.Spaces = append(t.Spaces, v)
		return nil
	})
}

// Mint returns the token.
func (t *Token) Mint() (string, error) {
	key, err := ReadKey(t.KeyFile)
	if err != nil {
		return "", err
	}
	c := auth.Claims{Subject: t.Subject, Audience: t.Audience, ExpiresAt: time.Now().Add(t.TTL).Unix(), Herdlet: t.Herdlet, Spaces: map[string]auth.Role{}}
	for _, s := range t.Spaces {
		name, role, ok := strings.Cut(s, "=")
		if !ok || (role != string(auth.RoleRead) && role != string(auth.RoleWrite) && role != string(auth.RoleOperator)) {
			return "", fmt.Errorf("-space %q: want atespace=read|write|operator", s)
		}
		c.Spaces[name] = auth.Role(role)
	}
	return auth.Sign(key, c)
}

// JoinToken is `herd-api join-token`: it mints the one token a host joins with.
type JoinToken struct {
	KeyFile string
	TTL     time.Duration
}

// Flags registers the join-token command's flags.
func (j *JoinToken) Flags(fs *flag.FlagSet) {
	fs.StringVar(&j.KeyFile, "key-file", "", "herd-api's HS256 signing key")
	fs.DurationVar(&j.TTL, "ttl", 24*time.Hour, "how long hosts may join with this token")
}

// Mint returns the join token.
func (j *JoinToken) Mint() (string, error) {
	key, err := ReadKey(j.KeyFile)
	if err != nil {
		return "", err
	}
	return join.MintToken(key, j.TTL)
}

// Keygen is `herd-api keygen`: it writes a random signing key, keeping one that already exists.
type Keygen struct {
	Out string
}

// Flags registers the keygen command's flags.
func (k *Keygen) Flags(fs *flag.FlagSet) {
	fs.StringVar(&k.Out, "out", "/etc/herd/jwt.key", "key file to write (mode 0600); an existing key is kept")
}

// snapshots opens herd-api's snapshot store: the S3 bucket -s3 names, whose configuration joins hand to herdlets,
// or the -snapshots directory.
func (a *API) snapshots() (snapstore.Store, *s3.Config, error) {
	if a.S3 == "" {
		if a.Snapshots == "" {
			return nil, nil, errors.New("-snapshots or -s3 is required")
		}
		s, err := disk.New(a.Snapshots)
		return s, nil, err
	}
	s, err := a.s3Store()
	if err != nil {
		return nil, nil, err
	}
	cfg := s.Config()
	log.Printf("herd-api: snapshots in s3://%s/%s at %s", cfg.Bucket, cfg.Prefix, cfg.Endpoint)
	return s, &cfg, nil
}

// s3Store is the S3 store -s3 names, as a join hands it out (Config).
func (a *API) s3Store() (*s3.Store, error) {
	cfg, err := s3.ParseURL(a.S3)
	if err != nil {
		return nil, fmt.Errorf("-s3: %w", err)
	}
	c := a.S3Credentials
	if c.AccessKeyID == "" && c.SecretAccessKey == "" {
		c = s3.Credentials{AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
			SessionToken: os.Getenv("AWS_SESSION_TOKEN")}
	}
	cfg.Endpoint, cfg.Region = a.S3Endpoint, a.S3Region
	cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken = c.AccessKeyID, c.SecretAccessKey, c.SessionToken
	s, err := s3.New(cfg, nil)
	if err != nil {
		return nil, fmt.Errorf("-s3: %w", err)
	}
	return s, nil
}

// grantedStore is the snapshot store a join hands out: -s3's, nil for a directory.
func (a *API) grantedStore() (*s3.Config, error) {
	if a.S3 == "" {
		return nil, nil
	}
	s, err := a.s3Store()
	if err != nil {
		return nil, err
	}
	cfg := s.Config()
	return &cfg, nil
}
