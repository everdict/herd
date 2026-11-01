// Package deploy is the one code path every deployment mode runs (spec R-D4, ADR 0001 A-20). Each component — herd-api,
// herd-router, herd-egress and herdlet — is an Options struct whose flags are registered by the component itself and
// whose Run serves it; the binaries (cmd/herd-api, cmd/herd-router, cmd/herd-egress, cmd/herdlet) only parse flags
// into them, and the all-in-one binary (cmd/herd) runs the same four in one process. A mode is configuration:
//
//   - all-in-one: `herd -data DIR` — herd-api on a bbolt store, herd-router, herd-egress and a herdlet that joins
//     that herd-api over loopback with a join token the process mints;
//   - multi-host join: herd-api (+ herd-router) on one host, `herdlet join <herd-api> --token <t>` and herd-egress on
//     each node;
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
	KeyFile   string
	Audience  string
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

	// Key, if set, is used instead of KeyFile (all-in-one reads or creates its own).
	Key []byte
	// OnJoin, if set, sees every join herd-api granted.
	OnJoin func(join.Grant)
}

// Flags registers herd-api's flags on fs.
func (a *API) Flags(fs *flag.FlagSet) {
	fs.StringVar(&a.Listen, "listen", "127.0.0.1:7443", "address to serve gRPC (cleartext HTTP/2) on")
	fs.StringVar(&a.KeyFile, "key-file", "", "HS256 signing key for bearer tokens (at least 32 bytes)")
	fs.StringVar(&a.Audience, "audience", "", "required JWT audience, if any")
	fs.StringVar(&a.Store, "store", "memory", "record store: memory or bolt:<path> (one herd-api per file)")
	fs.StringVar(&a.Snapshots, "snapshots", "", "snapshot store directory (all-in-one, or hosts that share it; see -s3)")
	fs.StringVar(&a.S3, "s3", "", "keep snapshots in this bucket instead, s3://bucket[/prefix]; joins hand herdlets the store (credentials: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY)")
	fs.StringVar(&a.S3Endpoint, "s3-endpoint", "", "S3-compatible endpoint, http(s)://host[:port] (default: AWS for -s3-region)")
	fs.StringVar(&a.S3Region, "s3-region", "us-east-1", "S3 region requests are signed for")
	fs.DurationVar(&a.CallTimeout, "call-timeout", 30*time.Second, "how long to wait for a herdlet's answer")
	fs.Float64Var(&a.RestoreBandwidthMiB, "restore-bandwidth", apiserver.DefaultRestoreBandwidth>>20, "MiB/s a herdlet is expected to read a snapshot object it restores at, at least: a restore's call waits -call-timeout plus the object's size at this rate")
	fs.DurationVar(&a.Reconcile, "reconcile", 10*time.Second, "how often to retry owed workflow steps")
	fs.StringVar(&a.AdvertiseTunnel, "advertise-tunnel", "", "herd-router tunnel address handed to herdlets that join")
	fs.DurationVar(&a.CredentialTTL, "credential-ttl", 365*24*time.Hour, "lifetime of the credentials a join grants")
	fs.DurationVar(&a.GoldenWarmup, "golden-warmup", DefaultGoldenWarmup, goldenWarmupUsage)
}

// DefaultGoldenWarmup is Substrate's wait between starting a template's golden actor and taking its snapshot.
const DefaultGoldenWarmup = 20 * time.Second

const goldenWarmupUsage = "how long a template's golden actor runs before its golden snapshot is taken (0: at once)"

// Run serves herd-api on ln until ctx ends. It never dials a herdlet.
func (a *API) Run(ctx context.Context, ln net.Listener) error {
	key := a.Key
	if key == nil {
		var err error
		if key, err = ReadKey(a.KeyFile); err != nil {
			return err
		}
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
	authn := auth.StaticKey{Key: key, Audience: a.Audience}
	hub := link.NewHub()
	srv, err := apiserver.New(apiserver.Config{
		Store: st, Snapshots: snaps, Hub: hub, Authn: authn, Authz: auth.SpaceRoles{}, CallTimeout: a.CallTimeout,
		RestoreBandwidth: int64(a.RestoreBandwidthMiB * (1 << 20)), GoldenWarmup: a.GoldenWarmup,
	})
	if err != nil {
		return err
	}
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
	g := grpc.NewServer(grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}))
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
	log.Printf("herd-api serving on %s", ln.Addr())
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
	fs.Func("space", "atespace=read|write (repeatable; * for every space)", func(v string) error {
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
		if !ok || (role != string(auth.RoleRead) && role != string(auth.RoleWrite)) {
			return "", fmt.Errorf("-space %q: want atespace=read|write", s)
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
	cfg, err := s3.ParseURL(a.S3)
	if err != nil {
		return nil, nil, fmt.Errorf("-s3: %w", err)
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
		return nil, nil, fmt.Errorf("-s3: %w", err)
	}
	cfg = s.Config()
	log.Printf("herd-api: snapshots in s3://%s/%s at %s", cfg.Bucket, cfg.Prefix, cfg.Endpoint)
	return s, &cfg, nil
}
