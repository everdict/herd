package deploy

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/egress/nft"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type list []string

func (l *list) String() string { return strings.Join(*l, ",") }

// Set ignores an empty value, so a job can pass a flag whose variable is unset and mean no value.
func (l *list) Set(s string) error {
	if s != "" {
		*l = append(*l, s)
	}
	return nil
}

// egressAllowUsage is -egress-allow-prefix's usage on herd-egress, herdlet and herd.
const egressAllowUsage = "LAB ONLY: a prefix inside the host-side ranges herd-egress refuses, e.g. 100.113.143.93/32 for a tailnet echo host, that it no longer refuses (repeatable); a policy must still name the host, and nothing broader than written is opened. Logged at start and reported in the worker's capabilities"

// labAllow checks the -egress-allow-prefix values (egress.ParseLabAllow) and logs a set one loudly as who.
func labAllow(who string, prefixes []string) error {
	for _, s := range prefixes {
		if _, err := egress.ParseLabAllow(s); err != nil {
			return fmt.Errorf("-egress-allow-prefix: %w", err)
		}
	}
	if len(prefixes) > 0 {
		log.Printf("%s: WARNING: LAB EGRESS ALLOWANCE -egress-allow-prefix %s: herd-egress does not refuse these host-side addresses, so an actor whose policy lists a name resolving into them reaches it; this host is not a production host", who, strings.Join(prefixes, ","))
	}
	return nil
}

// NFT is `herd-egress nft`: it renders one VM's TAP and nftables table.
type NFT struct {
	ID, TAP, Host, VM  string
	ProxyPort, DNSPort uint
}

// Flags registers the nft command's flags.
func (n *NFT) Flags(fs *flag.FlagSet) {
	fs.StringVar(&n.ID, "id", "", "VM id (table herd_<id>)")
	fs.StringVar(&n.TAP, "tap", "", "TAP device")
	fs.StringVar(&n.Host, "host", "", "host address on the TAP with prefix, e.g. 10.88.0.1/30")
	fs.StringVar(&n.VM, "vm", "", "VM address, e.g. 10.88.0.2")
	fs.UintVar(&n.ProxyPort, "proxy-port", 15001, "herd-egress transparent TCP port")
	fs.UintVar(&n.DNSPort, "dns-port", 15053, "herd-egress DNS port")
}

// Render returns the ruleset.
func (n *NFT) Render() (string, error) {
	hp, err := netip.ParsePrefix(n.Host)
	if err != nil {
		return "", fmt.Errorf("-host: %w", err)
	}
	va, err := netip.ParseAddr(n.VM)
	if err != nil {
		return "", fmt.Errorf("-vm: %w", err)
	}
	if n.ProxyPort > 65535 || n.DNSPort > 65535 {
		return "", errors.New("ports must be at most 65535")
	}
	return nft.Ruleset(nft.VM{ID: n.ID, TAP: n.TAP, Host: hp, VM: va, ProxyPort: uint16(n.ProxyPort), DNSPort: uint16(n.DNSPort)})
}

type bindingEntry struct {
	Space  string `json:"space"`
	Name   string `json:"name"`
	UID    string `json:"uid"`
	Source string `json:"source"`
}

// Egress is herd-egress's configuration.
type Egress struct {
	API       string
	TokenFile string
	Listen    string
	DNSListen string
	DNSAnswer string
	Bindings  string
	CACert    string
	CAKey     string
	CAOut     string
	Secrets   string
	JWTKey    string
	JWTIssuer string
	Interval  time.Duration
	HostAddrs list
	HostNames list
	// AllowPrefixes are the operator's lab allowances (-egress-allow-prefix, egress.GuardConfig.LabAllow).
	AllowPrefixes list
	// HostServicesFile declares host services (-host-services-file, egress.Gateway.LoadHostServices), re-read every
	// Interval.
	HostServicesFile string
	// Routers are herd-router's endpoints (-router), which a declared host service may not name, as it may not name API.
	Routers list

	// Token, if set, is used instead of TokenFile.
	Token string
	// NewGateway, if set, builds the gateway from the configuration the flags gave instead of egress.New (tests: a
	// resolver and dialer of their own).
	NewGateway func(egress.Config) (*egress.Gateway, error)
}

// Flags registers herd-egress's flags on fs.
func (e *Egress) Flags(fs *flag.FlagSet) {
	fs.StringVar(&e.API, "api", "127.0.0.1:7443", "herd-api address")
	fs.StringVar(&e.TokenFile, "token-file", "", "bearer token for herd-api (read access to the actors' spaces; `herdlet join` writes one)")
	fs.StringVar(&e.Listen, "listen", ":15001", "transparent TCP listener nftables redirects to")
	fs.StringVar(&e.DNSListen, "dns", ":15053", "synthetic DNS listener (UDP) nftables redirects port 53 to")
	fs.StringVar(&e.DNSAnswer, "dns-answer", "198.18.0.1", "the address every A query is answered with; any address routed over the TAP works, since all TCP is redirected")
	fs.StringVar(&e.Bindings, "bindings", "", "JSON bindings file (actor and its VM's TAP address)")
	fs.StringVar(&e.CACert, "ca-cert", "", "gateway CA certificate (PEM); with -ca-key")
	fs.StringVar(&e.CAKey, "ca-key", "", "gateway CA EC key (PEM)")
	fs.StringVar(&e.CAOut, "ca-out", "", "without -ca-cert: where to keep the CA certificate herdlet installs in guests (generated once; its key beside it as <ca-out>.key, so a restart keeps the CA guests trust)")
	fs.StringVar(&e.Secrets, "secrets", "", "directory for ate-secret://dir/<name>/<tail> credentials")
	fs.StringVar(&e.JWTKey, "jwt-key", "", "EC P-256 key (PEM) for actor_jwt credentials")
	fs.StringVar(&e.JWTIssuer, "jwt-issuer", "", "issuer of actor JWTs")
	fs.DurationVar(&e.Interval, "interval", time.Second, "how often each actor's policy and the bindings are re-read")
	fs.Var(&e.HostAddrs, "host-addr", "a host or herd-api address or prefix to refuse (repeatable)")
	fs.Var(&e.HostNames, "host-name", "a host or herd-api name to refuse (repeatable)")
	fs.Var(&e.AllowPrefixes, "egress-allow-prefix", egressAllowUsage)
	fs.StringVar(&e.HostServicesFile, "host-services-file", "", hostServicesUsage)
	fs.Var(&e.Routers, "router", "herd-router endpoint(s), address:port, comma-separated (repeatable), that a declared host service may not name")
}

// Run serves the gateway's transparent TCP listener on ln and its synthetic DNS on pc until ctx ends.
func (e *Egress) Run(ctx context.Context, ln net.Listener, pc net.PacketConn) error {
	if e.Interval <= 0 {
		e.Interval = time.Second
	}
	if e.Bindings == "" {
		return errors.New("-bindings is required")
	}
	if err := labAllow("herd-egress", e.AllowPrefixes); err != nil {
		return err
	}
	guard, err := egress.NewGuard(egress.GuardConfig{HostAddrs: e.HostAddrs, HostNames: e.HostNames, LabAllow: e.AllowPrefixes})
	if err != nil {
		return err
	}
	ca, err := loadOrMakeCA(e.CACert, e.CAKey, e.CAOut)
	if err != nil {
		return err
	}
	cfg := egress.Config{Guard: guard, CA: ca, OnEvent: func(ev egress.Event) {
		if !ev.Allowed {
			log.Printf("herd-egress: refused %s %s %s:%d host-side=%v: %s", ev.Actor, ev.Protocol, ev.Name, ev.Port, ev.HostSide, ev.Reason)
		}
	}}
	if cfg.Endpoints, err = e.endpoints(); err != nil {
		return err
	}
	cfg.Credentials = credentialStores(e.Secrets)
	if e.JWTKey != "" {
		k, err := loadECKey(e.JWTKey)
		if err != nil {
			return fmt.Errorf("-jwt-key: %w", err)
		}
		cfg.JWTs = &egress.ES256Minter{Issuer: e.JWTIssuer, Key: k}
	}
	newGateway := e.NewGateway
	if newGateway == nil {
		newGateway = egress.New
	}
	g, err := newGateway(cfg)
	if err != nil {
		return err
	}
	var services *hostServicesFile
	if e.HostServicesFile != "" {
		services = &hostServicesFile{path: e.HostServicesFile, g: g}
		if err := services.load(); err != nil {
			return fmt.Errorf("-host-services-file: %w", err)
		}
		log.Printf("herd-egress: declared host services are not reported in the worker's capabilities: that needs a capabilities field herd does not have yet")
	}
	answer, err := netip.ParseAddr(e.DNSAnswer)
	if err != nil {
		return fmt.Errorf("-dns-answer: %w", err)
	}

	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	tok := e.Token
	if tok == "" && e.TokenFile != "" {
		b, err := os.ReadFile(e.TokenFile)
		if err != nil {
			return err
		}
		tok = strings.TrimSpace(string(b))
	}
	if tok != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(auth.Bearer(tok)))
	}
	conn, err := grpc.NewClient("passthrough:///"+e.API, opts...)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var src egress.Sources
	reload := func() {
		if err := loadBindings(e.Bindings, g, &src); err != nil {
			log.Printf("herd-egress: bindings: %v", err)
		}
		if services != nil {
			services.reload()
		}
	}
	reload()
	syncer := &egress.Syncer{Gateway: g, Source: egress.ControlSource{Conn: conn}, Interval: e.Interval,
		OnError: func(a egress.Actor, err error) { log.Printf("herd-egress: policy of %s: %v", a, err) }}
	go func() { _ = syncer.Run(ctx) }()
	go func() {
		t := time.NewTicker(e.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				reload()
			}
		}
	}()
	go func() {
		if err := egress.ServeDNS(ctx, pc, egress.SyntheticDNS(answer)); err != nil {
			log.Printf("herd-egress: dns: %v", err)
		}
	}()
	log.Printf("herd-egress: flows on %s, DNS on %s, policies from %s every %v", ln.Addr(), pc.LocalAddr(), e.API, e.Interval)
	return g.ServeTransparent(ctx, ln, &src, nil)
}

// loadBindings makes the gateway's bound actors and the source map equal the file.
func loadBindings(path string, g *egress.Gateway, src *egress.Sources) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		b = []byte("[]")
	} else if err != nil {
		return err
	}
	var entries []bindingEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return err
	}
	want := map[egress.Actor]netip.Addr{}
	for _, e := range entries {
		a, err := netip.ParseAddr(e.Source)
		if err != nil {
			return fmt.Errorf("binding %s/%s: source: %w", e.Space, e.Name, err)
		}
		want[egress.Actor{Space: e.Space, Name: e.Name, UID: e.UID}] = a
	}
	for _, a := range g.Actors() {
		if _, ok := want[a]; !ok {
			g.Unbind(a)
		}
	}
	for a, addr := range want {
		g.Bind(a)
		src.Set(addr, a)
	}
	src.Retain(func(addr netip.Addr, a egress.Actor) bool { return want[a] == addr })
	return nil
}

func loadOrMakeCA(certPath, keyPath, out string) (*egress.CA, error) {
	if certPath != "" || keyPath != "" {
		c, err := os.ReadFile(certPath)
		if err != nil {
			return nil, err
		}
		k, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, err
		}
		return egress.LoadCA(c, k)
	}
	if out != "" {
		if err := ensureCA(out, out+".key"); err != nil {
			return nil, err
		}
		return loadOrMakeCA(out, out+".key", "")
	}
	return egress.NewCA("herd-egress CA")
}

func loadECKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("not PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
		if ek, ok := k.(*ecdsa.PrivateKey); ok {
			return ek, nil
		}
		return nil, errors.New("not an EC key")
	}
	return x509.ParseECPrivateKey(blk.Bytes)
}

// ensureCA keeps a gateway CA at certPath and keyPath: the one there, or a new one written there (the key 0600), so
// herd-egress restarts with the CA its guests were given.
func ensureCA(certPath, keyPath string) error {
	_, cerr := os.Stat(certPath)
	_, kerr := os.Stat(keyPath)
	if cerr == nil && kerr == nil {
		_, err := loadOrMakeCA(certPath, keyPath, "")
		return err
	}
	ca, err := egress.NewCA("herd-egress CA")
	if err != nil {
		return err
	}
	key, err := ca.KeyPEM()
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, ca.CertPEM(), 0o644)
}

// credentialStores are a gateway's credential stores for -secrets dir, herd-egress's and a Mac herdlet's alike:
// ate-secret://dir/<name>/<tail> from the directory, and a credential naming another store refused as absent.
// Without a directory there is none, and every credential_uri is refused ("no credential store").
func credentialStores(dir string) egress.Credentials {
	if dir == "" {
		return nil
	}
	return egress.Providers{"dir": egress.DirProvider{Root: dir}}
}

// hostServicesUsage is -host-services-file's usage on herd-egress and herd.
const hostServicesUsage = "file of declared host services, one '<name>:<port> <address>:<port> [loopback]' per line (# comments): an actor whose policy allows <name>:<port> reaches exactly that address, private or not, with no lookup. Re-read every -interval; a bad read keeps the last good set, and a file unreadable at start refuses the start. A line whose target is a -host-addr, the -api or -router endpoint, a metadata endpoint, or loopback without 'loopback' is dropped with a log"

// endpoints are the Control (-api) and router (-router) endpoints a declared host service may not name.
func (e *Egress) endpoints() ([]netip.AddrPort, error) {
	var out []netip.AddrPort
	if ap, err := netip.ParseAddrPort(e.API); err == nil {
		out = append(out, ap)
	} else if e.HostServicesFile != "" {
		log.Printf("herd-egress: -api %s is not an address:port, so a host service naming herd-api is refused only as a -host-addr", e.API)
	}
	for _, r := range e.Routers {
		for _, s := range strings.Split(r, ",") {
			if s = strings.TrimSpace(s); s == "" {
				continue
			}
			ap, err := netip.ParseAddrPort(s)
			if err != nil {
				return nil, fmt.Errorf("-router %q: want address:port: %w", s, err)
			}
			out = append(out, ap)
		}
	}
	return out, nil
}

// hostServicesFile is -host-services-file: the gateway's declared host services follow it.
type hostServicesFile struct {
	path    string
	g       *egress.Gateway
	last    []byte
	loaded  bool
	lastErr string
}

// load reads the file and gives the gateway its services; a file that cannot be read or parsed changes nothing.
func (f *hostServicesFile) load() error {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return err
	}
	if f.loaded && bytes.Equal(b, f.last) {
		return nil
	}
	dropped, err := f.g.LoadHostServices(b)
	if err != nil {
		return err
	}
	f.last, f.loaded = b, true
	for _, d := range dropped {
		log.Printf("herd-egress: %s: %v; that line is dropped", f.path, d)
	}
	var names []string
	for _, s := range f.g.HostServices() {
		names = append(names, s.String())
	}
	log.Printf("herd-egress: declared host services from %s: [%s]", f.path, strings.Join(names, ", "))
	return nil
}

// reload is load on the -interval: a failure is logged once until the file changes, and keeps the last good set.
func (f *hostServicesFile) reload() {
	msg := ""
	if err := f.load(); err != nil {
		msg = err.Error()
		if msg != f.lastErr {
			log.Printf("herd-egress: -host-services-file: %v; keeping the last good set", err)
		}
	}
	f.lastErr = msg
}
