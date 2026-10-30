// Command herd-egress is the per-host egress gateway (ADR 0001 A-18). On Ubuntu it serves the TCP flows and DNS
// queries nftables redirects from each VM's TAP (render a VM's TAP and table with `herd-egress nft`); it reads each
// bound actor's EgressPolicy from herd-api and applies updates to live actors within -interval.
//
//	herd-egress -api 127.0.0.1:7443 -token-file /etc/herd/egress.token -bindings /run/herd/egress-bindings.json \
//	    -ca-cert /etc/herd/egress-ca.pem -ca-key /etc/herd/egress-ca.key -secrets /etc/herd/secrets \
//	    -host-addr 203.0.113.7 -host-name herd-api.example
//	herd-egress nft -id a1b2c3 -tap herd-a1b2c3 -host 10.88.0.1/30 -vm 10.88.0.2
//
// The bindings file is a JSON list of {"space","name","uid","source"} (source: the VM's TAP address); herdlet writes
// it, and herd-egress re-reads it every -interval. macOS hosts serve the same gateway behind a userspace stack per VM
// (egress.Stack), which lands with R7.
package main

import (
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
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/egress/nft"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "nft" {
		if err := renderNFT(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "herd-egress nft:", err)
			os.Exit(2)
		}
		return
	}
	if err := serve(os.Args[1:]); err != nil {
		log.Fatalf("herd-egress: %v", err)
	}
}

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(s string) error { *l = append(*l, s); return nil }

func renderNFT(args []string) error {
	fs := flag.NewFlagSet("herd-egress nft", flag.ContinueOnError)
	id := fs.String("id", "", "VM id (table herd_<id>)")
	tap := fs.String("tap", "", "TAP device")
	host := fs.String("host", "", "host address on the TAP with prefix, e.g. 10.88.0.1/30")
	vm := fs.String("vm", "", "VM address, e.g. 10.88.0.2")
	proxy := fs.Uint("proxy-port", 15001, "herd-egress transparent TCP port")
	dns := fs.Uint("dns-port", 15053, "herd-egress DNS port")
	if err := fs.Parse(args); err != nil {
		return err
	}
	hp, err := netip.ParsePrefix(*host)
	if err != nil {
		return fmt.Errorf("-host: %w", err)
	}
	va, err := netip.ParseAddr(*vm)
	if err != nil {
		return fmt.Errorf("-vm: %w", err)
	}
	if *proxy > 65535 || *dns > 65535 {
		return errors.New("ports must be at most 65535")
	}
	rs, err := nft.Ruleset(nft.VM{ID: *id, TAP: *tap, Host: hp, VM: va, ProxyPort: uint16(*proxy), DNSPort: uint16(*dns)})
	if err != nil {
		return err
	}
	fmt.Print(rs)
	return nil
}

type bindingEntry struct {
	Space  string `json:"space"`
	Name   string `json:"name"`
	UID    string `json:"uid"`
	Source string `json:"source"`
}

func serve(args []string) error {
	fs := flag.NewFlagSet("herd-egress", flag.ContinueOnError)
	api := fs.String("api", "127.0.0.1:7443", "herd-api address")
	tokenFile := fs.String("token-file", "", "bearer token for herd-api (read access to the actors' spaces)")
	listen := fs.String("listen", ":15001", "transparent TCP listener nftables redirects to")
	dnsListen := fs.String("dns", ":15053", "synthetic DNS listener (UDP) nftables redirects port 53 to")
	dnsAnswer := fs.String("dns-answer", "198.18.0.1", "the address every A query is answered with; any address routed over the TAP works, since all TCP is redirected")
	bindings := fs.String("bindings", "", "JSON bindings file (actor and its VM's TAP address)")
	caCert := fs.String("ca-cert", "", "gateway CA certificate (PEM); with -ca-key")
	caKey := fs.String("ca-key", "", "gateway CA EC key (PEM)")
	caOut := fs.String("ca-out", "", "without -ca-cert: where to write the generated CA certificate for herdlet to install in guests")
	secrets := fs.String("secrets", "", "directory for ate-secret://dir/<name>/<tail> credentials")
	jwtKey := fs.String("jwt-key", "", "EC P-256 key (PEM) for actor_jwt credentials")
	jwtIssuer := fs.String("jwt-issuer", "", "issuer of actor JWTs")
	interval := fs.Duration("interval", time.Second, "how often each actor's policy and the bindings are re-read")
	var hostAddrs, hostNames list
	fs.Var(&hostAddrs, "host-addr", "a host or herd-api address or prefix to refuse (repeatable)")
	fs.Var(&hostNames, "host-name", "a host or herd-api name to refuse (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bindings == "" {
		return errors.New("-bindings is required")
	}
	guard, err := egress.NewGuard(egress.GuardConfig{HostAddrs: hostAddrs, HostNames: hostNames})
	if err != nil {
		return err
	}
	ca, err := loadOrMakeCA(*caCert, *caKey, *caOut)
	if err != nil {
		return err
	}
	cfg := egress.Config{Guard: guard, CA: ca, OnEvent: func(e egress.Event) {
		if !e.Allowed {
			log.Printf("refused %s %s %s:%d host-side=%v: %s", e.Actor, e.Protocol, e.Name, e.Port, e.HostSide, e.Reason)
		}
	}}
	if *secrets != "" {
		cfg.Credentials = egress.Providers{"dir": egress.DirProvider{Root: *secrets}}
	}
	if *jwtKey != "" {
		k, err := loadECKey(*jwtKey)
		if err != nil {
			return fmt.Errorf("-jwt-key: %w", err)
		}
		cfg.JWTs = &egress.ES256Minter{Issuer: *jwtIssuer, Key: k}
	}
	g, err := egress.New(cfg)
	if err != nil {
		return err
	}
	answer, err := netip.ParseAddr(*dnsAnswer)
	if err != nil {
		return fmt.Errorf("-dns-answer: %w", err)
	}

	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if *tokenFile != "" {
		tok, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		opts = append(opts, grpc.WithPerRPCCredentials(auth.Bearer(strings.TrimSpace(string(tok)))))
	}
	conn, err := grpc.NewClient("passthrough:///"+*api, opts...)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var src egress.Sources
	reload := func() {
		if err := loadBindings(*bindings, g, &src); err != nil {
			log.Printf("bindings: %v", err)
		}
	}
	reload()
	syncer := &egress.Syncer{Gateway: g, Source: egress.ControlSource{Conn: conn}, Interval: *interval,
		OnError: func(a egress.Actor, err error) { log.Printf("policy of %s: %v", a, err) }}
	go func() { _ = syncer.Run(ctx) }()
	go func() {
		t := time.NewTicker(*interval)
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
	pc, err := net.ListenPacket("udp", *dnsListen)
	if err != nil {
		return err
	}
	go func() {
		if err := egress.ServeDNS(ctx, pc, egress.SyntheticDNS(answer)); err != nil {
			log.Printf("dns: %v", err)
		}
	}()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	log.Printf("herd-egress: flows on %s, DNS on %s, policies from %s every %v", ln.Addr(), pc.LocalAddr(), *api, *interval)
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
	ca, err := egress.NewCA("herd-egress CA")
	if err != nil {
		return nil, err
	}
	if out != "" {
		if err := os.WriteFile(out, ca.CertPEM(), 0o644); err != nil {
			return nil, err
		}
	}
	return ca, nil
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
