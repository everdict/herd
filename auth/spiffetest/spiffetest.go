// Package spiffetest is a SPIFFE issuer for tests: an X.509 root and a JWT signing key, as a SPIRE server holds them,
// that issues X.509-SVIDs and JWT-SVIDs and writes the trust bundle and a policy herd's SPIFFE authenticator reads.
package spiffetest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/everdict/herd/auth"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/bundle/spiffebundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// CA is a test issuer of one trust domain.
type CA struct {
	TrustDomain string
	Cert        *x509.Certificate
	key         *ecdsa.PrivateKey
	jwt         *ecdsa.PrivateKey
	kid         string
}

// New returns an issuer for trust domain td.
func New(t testing.TB, td string) *CA {
	t.Helper()
	key := genKey(t)
	serial := serial(t)
	tpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: td + " root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		URIs: []*url.URL{{Scheme: "spiffe", Host: td}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &CA{TrustDomain: td, Cert: cert, key: key, jwt: genKey(t), kid: "jwt-" + serial.Text(16)}
}

func genKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func serial(t testing.TB) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TLSCertificate issues an X.509-SVID for id, valid for an hour, as a tls.Certificate (client or server).
func (c *CA) TLSCertificate(t testing.TB, id string) tls.Certificate {
	t.Helper()
	u, err := url.Parse(id)
	if err != nil {
		t.Fatal(err)
	}
	key := genKey(t)
	tpl := &x509.Certificate{
		SerialNumber: serial(t), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.Cert, key.Public(), c.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// WriteKeyPair writes cert's chain and key as PEM files in dir, as spiffe-helper writes an SVID.
func WriteKeyPair(t testing.TB, dir, name string, cert tls.Certificate) (certFile, keyFile string) {
	t.Helper()
	k, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	var chain []byte
	for _, d := range cert.Certificate {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})...)
	}
	certFile, keyFile = filepath.Join(dir, name+".pem"), filepath.Join(dir, name+"-key.pem")
	write(t, certFile, chain)
	write(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k}))
	return certFile, keyFile
}

// JWTSVID issues a JWT-SVID for sub with audience aud, expiring at exp.
func (c *CA) JWTSVID(t testing.TB, sub string, aud []string, exp time.Time) string {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: c.jwt},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", c.kid))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(s).Claims(jwt.Claims{Subject: sub, Audience: aud, Expiry: jwt.NewNumericDate(exp),
		IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute))}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// Bundle is the trust bundle in the SPIFFE format: a JWKS with the root (x509-svid) and the JWT key (jwt-svid).
func (c *CA) Bundle(t testing.TB) []byte {
	t.Helper()
	b := spiffebundle.FromX509Authorities(spiffeid.RequireTrustDomainFromString(c.TrustDomain), []*x509.Certificate{c.Cert})
	if err := b.AddJWTAuthority(c.kid, c.jwt.Public()); err != nil {
		t.Fatal(err)
	}
	out, err := b.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Config writes the bundle and policy into dir and returns the authenticator's configuration for audience.
func (c *CA) Config(t testing.TB, dir, audience, policy string) auth.SPIFFEConfig {
	t.Helper()
	b, p := filepath.Join(dir, "bundle.json"), filepath.Join(dir, "policy.json")
	write(t, b, c.Bundle(t))
	write(t, p, []byte(policy))
	return auth.SPIFFEConfig{TrustDomain: c.TrustDomain, BundleFile: b, Audience: audience, PolicyFile: p}
}

func write(t testing.TB, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
