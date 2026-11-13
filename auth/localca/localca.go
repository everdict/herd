// Package localca is a SPIFFE issuer for one host: an X.509 root and a JWT signing key that issue X.509-SVIDs and
// JWT-SVIDs in a trust domain of its own. herd all-in-one uses it to give its components identities without a SPIRE
// server (ADR 0001 A-60), and tests use it as a stand-in for one. It is not a cluster's issuer: a cluster's identities
// come from its Workload API.
package localca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/everdict/herd/auth"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/spiffebundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

// CA issues SVIDs of one trust domain.
type CA struct {
	TD   spiffeid.TrustDomain
	Root *x509.Certificate
	key  *ecdsa.PrivateKey
	jwt  *ecdsa.PrivateKey
	kid  string
}

// New makes a CA for trust domain td, its root valid for ten years.
func New(td string) (*CA, error) {
	d, err := spiffeid.TrustDomainFromString(td)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	jk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber: sn, Subject: pkix.Name{CommonName: td + " local root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		URIs: []*url.URL{{Scheme: "spiffe", Host: td}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{TD: d, Root: root, key: key, jwt: jk, kid: "jwt-" + sn.Text(16)}, nil
}

func serial() (*big.Int, error) { return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62)) }

// LoadOrCreate reads the CA kept in dir (ca.pem, ca-key.pem, jwt-key.pem), or makes one for td and keeps it there
// (mode 0600), so the SVIDs it issued stay valid across restarts. A kept CA of another trust domain is an error.
func LoadOrCreate(dir, td string) (*CA, error) {
	rootPath, keyPath, jwtPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem"), filepath.Join(dir, "jwt-key.pem")
	if _, err := os.Stat(rootPath); errors.Is(err, os.ErrNotExist) {
		ca, err := New(td)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		for path, blk := range map[string]*pem.Block{rootPath: {Type: "CERTIFICATE", Bytes: ca.Root.Raw}, keyPath: mustKey(ca.key), jwtPath: mustKey(ca.jwt)} {
			if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
				return nil, err
			}
		}
		return ca, nil
	}
	root, err := readPEM(rootPath)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(root)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rootPath, err)
	}
	if len(cert.URIs) != 1 || cert.URIs[0].Host != td {
		return nil, fmt.Errorf("%s is the root of another trust domain (%v), not %q", rootPath, cert.URIs, td)
	}
	key, err := readKey(keyPath)
	if err != nil {
		return nil, err
	}
	jk, err := readKey(jwtPath)
	if err != nil {
		return nil, err
	}
	return &CA{TD: spiffeid.RequireTrustDomainFromString(td), Root: cert, key: key, jwt: jk, kid: "jwt-" + cert.SerialNumber.Text(16)}, nil
}

func mustKey(k *ecdsa.PrivateKey) *pem.Block {
	b, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		panic(err) // a P-256 key always marshals
	}
	return &pem.Block{Type: "PRIVATE KEY", Bytes: b}
}

func readPEM(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	return blk.Bytes, nil
}

func readKey(path string) (*ecdsa.PrivateKey, error) {
	der, err := readPEM(path)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ECDSA key", path)
	}
	return ek, nil
}

// Options are an SVID's extra names (a server reached by clients that check a host name) and lifetime.
type Options struct {
	DNS []string
	IPs []net.IP
	// TTL is the SVID's lifetime (default 24 h).
	TTL time.Duration
	// NotBefore, if set, starts the SVID's validity (default an hour ago, for clock skew).
	NotBefore time.Time
}

// SVID issues an X.509-SVID for id (a raw URI, so tests can issue a malformed one).
func (c *CA) SVID(id string, o Options) (*x509svid.SVID, error) {
	u, err := url.Parse(id)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	if o.TTL <= 0 {
		o.TTL = 24 * time.Hour
	}
	nb := o.NotBefore
	if nb.IsZero() {
		nb = time.Now().Add(-time.Hour)
	}
	tpl := &x509.Certificate{
		SerialNumber: sn, NotBefore: nb, NotAfter: nb.Add(o.TTL),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		URIs:        []*url.URL{u}, DNSNames: o.DNS, IPAddresses: o.IPs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.Root, key.Public(), c.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	sid, err := x509svid.IDFromCert(leaf)
	if err != nil {
		// A malformed id (tests): the SVID carries the certificate, its ID zero.
		sid = spiffeid.ID{}
	}
	return &x509svid.SVID{ID: sid, Certificates: []*x509.Certificate{leaf}, PrivateKey: key}, nil
}

// X509Bundle is the CA's X.509 bundle.
func (c *CA) X509Bundle() *x509bundle.Bundle {
	return x509bundle.FromX509Authorities(c.TD, []*x509.Certificate{c.Root})
}

// JWTBundle is the CA's JWT bundle.
func (c *CA) JWTBundle() *jwtbundle.Bundle {
	return jwtbundle.FromJWTAuthorities(c.TD, map[string]crypto.PublicKey{c.kid: c.jwt.Public()})
}

// Source is an identity for id issued by the CA, with the CA's bundles.
func (c *CA) Source(id string, o Options) (auth.Static, error) {
	sv, err := c.SVID(id, o)
	if err != nil {
		return auth.Static{}, err
	}
	return auth.Static{SVID: sv, X509: c.X509Bundle(), JWT: c.JWTBundle()}, nil
}

// JWTSVID issues a JWT-SVID for sub with audience aud, expiring at exp.
func (c *CA) JWTSVID(sub string, aud []string, exp time.Time) (string, error) {
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: c.jwt},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", c.kid))
	if err != nil {
		return "", err
	}
	return jwt.Signed(s).Claims(jwt.Claims{Subject: sub, Audience: aud, Expiry: jwt.NewNumericDate(exp),
		IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute))}).Serialize()
}

// SPIFFEBundle is the CA's bundle in the SPIFFE format (a JWKS with the root and the JWT key).
func (c *CA) SPIFFEBundle() ([]byte, error) {
	b := spiffebundle.FromX509Authorities(c.TD, []*x509.Certificate{c.Root})
	if err := b.AddJWTAuthority(c.kid, c.jwt.Public()); err != nil {
		return nil, err
	}
	return b.Marshal()
}

// WriteSVID writes sv as PEM files dir/<name>.pem (the chain) and dir/<name>-key.pem (its PKCS#8 key, mode 0600), as
// spiffe-helper writes an SVID, and returns their paths.
func WriteSVID(dir, name string, sv *x509svid.SVID) (certFile, keyFile string, err error) {
	chain, key, err := sv.Marshal()
	if err != nil {
		return "", "", err
	}
	certFile, keyFile = filepath.Join(dir, name+".pem"), filepath.Join(dir, name+"-key.pem")
	if err := os.WriteFile(certFile, chain, 0o644); err != nil {
		return "", "", err
	}
	return certFile, keyFile, os.WriteFile(keyFile, key, 0o600)
}

// WriteBundle writes the CA's root as a PEM bundle at path.
func (c *CA) WriteBundle(path string) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Root.Raw}), 0o644)
}
