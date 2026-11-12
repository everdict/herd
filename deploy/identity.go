package deploy

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/everdict/herd/auth"
)

// Identity is how herd-api and herd-router authenticate callers (ADR 0001 A-60): SPIFFE SVIDs verified against the
// install's trust bundle and mapped to roles by a policy file, with the HS256 shared key kept as counted break-glass.
// Without -spiffe-trust-domain only HS256 bearers are accepted, every call still counted as break-glass.
type Identity struct {
	TrustDomain string
	Bundle      string
	JWTBundle   string
	Audience    string
	Policy      string
}

// Flags registers the SPIFFE flags; audience is the default -spiffe-audience.
func (i *Identity) Flags(fs *flag.FlagSet) {
	fs.StringVar(&i.TrustDomain, "spiffe-trust-domain", "", "SPIFFE trust domain callers' SVIDs must name (empty: HS256 bearers only, as break-glass)")
	fs.StringVar(&i.Bundle, "spiffe-bundle", "", "trust bundle file: PEM X.509 authorities, or a SPIFFE bundle JWKS (spire-server bundle show -format spiffe); read again when it changes")
	fs.StringVar(&i.JWTBundle, "spiffe-jwt-bundle", "", "JWKS of JWT-SVID authorities beside a PEM -spiffe-bundle (a SPIFFE-format bundle carries them itself)")
	fs.StringVar(&i.Audience, "spiffe-audience", "", "audience a JWT-SVID must name, e.g. spiffe://<td>/herd/api")
	fs.StringVar(&i.Policy, "spiffe-policy", "", "JSON policy mapping SPIFFE IDs (exact or by path prefix) to atespace roles read|write|operator, or herdlet for /herd/node/<worker>; read again when it changes")
}

// Enabled reports whether SPIFFE is configured.
func (i *Identity) Enabled() bool { return i.TrustDomain != "" }

// Authenticator is the auth.Chain of i: SPIFFE when configured, and HS256 break-glass with key (aud must be
// hsAudience when set), its calls counted on m.
func (i *Identity) Authenticator(key []byte, hsAudience string, m *auth.Meter) (auth.Authenticator, error) {
	c := auth.Chain{BreakGlass: auth.StaticKey{Key: key, Audience: hsAudience}, Meter: m}
	if !i.Enabled() {
		if i.Bundle != "" || i.JWTBundle != "" || i.Policy != "" || i.Audience != "" {
			return nil, errors.New("-spiffe-bundle, -spiffe-jwt-bundle, -spiffe-policy and -spiffe-audience need -spiffe-trust-domain")
		}
		log.Printf("herd: no SPIFFE trust domain configured: only HS256 bearers are accepted, each call counted as break-glass")
		return c, nil
	}
	s, err := auth.NewSPIFFE(auth.SPIFFEConfig{TrustDomain: i.TrustDomain, BundleFile: i.Bundle, JWTBundleFile: i.JWTBundle,
		Audience: i.Audience, PolicyFile: i.Policy})
	if err != nil {
		return nil, err
	}
	c.SVID = s
	log.Printf("herd: SPIFFE trust domain %s, bundle %s, policy %s, audience %s; HS256 bearers accepted as break-glass", i.TrustDomain, i.Bundle, i.Policy, i.Audience)
	return c, nil
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

// keyPair is a certificate and key read from PEM files, read again when either changes (an SVID rotated in place by
// spire-agent or spiffe-helper), the last good pair kept when a change does not load.
type keyPair struct {
	cert, key string
	mu        sync.Mutex
	stamp     string
	pair      *tls.Certificate
}

func loadKeyPair(cert, key string) (*keyPair, error) {
	kp := &keyPair{cert: cert, key: key}
	if _, err := kp.get(); err != nil {
		return nil, err
	}
	return kp, nil
}

func (kp *keyPair) get() (*tls.Certificate, error) {
	kp.mu.Lock()
	defer kp.mu.Unlock()
	var stamp string
	for _, p := range []string{kp.cert, kp.key} {
		fi, err := os.Stat(p)
		if err != nil {
			if kp.pair != nil {
				return kp.pair, nil
			}
			return nil, err
		}
		stamp += fmt.Sprintf("%s:%d:%d;", p, fi.Size(), fi.ModTime().UnixNano())
	}
	if stamp == kp.stamp && kp.pair != nil {
		return kp.pair, nil
	}
	c, err := tls.LoadX509KeyPair(kp.cert, kp.key)
	if err != nil {
		if kp.pair != nil {
			log.Printf("herd-api: -tls-cert/-tls-key changed but do not load (%v); keeping the last good pair", err)
			kp.stamp = stamp
			return kp.pair, nil
		}
		return nil, fmt.Errorf("-tls-cert %s, -tls-key %s: %w", kp.cert, kp.key, err)
	}
	kp.stamp, kp.pair = stamp, &c
	return kp.pair, nil
}

// mtlsConfig is the TLS of herd-api's mTLS listener: its own certificate from kp, and a client certificate required
// of every caller. The handshake proves the caller holds the certificate's key; whether it is an SVID of the trust
// domain is the authenticator's to verify, against the bundle it re-reads (auth.SPIFFE), so the listener's TLS does
// not verify the chain itself.
func mtlsConfig(kp *keyPair) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		ClientAuth:     tls.RequireAnyClientCert,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return kp.get() },
	}
}
