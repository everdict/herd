package auth_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/auth"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/spiffebundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

const (
	td       = "herd.test"
	audience = "spiffe://herd.test/herd/api"
	runnerID = "spiffe://herd.test/everdict/ws/acme/runner/r1"
)

// ca is a test SPIFFE issuer: an X.509 root and a JWT signing key, as SPIRE's server holds them.
type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	jwt  *ecdsa.PrivateKey
	kid  string
}

func newCA(t *testing.T, domain string) *ca {
	t.Helper()
	key := genKey(t)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: domain + " root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		URIs: []*url.URL{{Scheme: "spiffe", Host: domain}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &ca{cert: cert, key: key, jwt: genKey(t), kid: domain + "-jwt-" + cert.SerialNumber.String() + "-" + big.NewInt(time.Now().UnixNano()).String()}
}

func genKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// x509SVID issues a leaf for id (a raw URI, so a malformed one can be issued), valid from notBefore to notAfter.
func (c *ca) x509SVID(t *testing.T, id string, notBefore, notAfter time.Time) []*x509.Certificate {
	t.Helper()
	u, err := url.Parse(id)
	if err != nil {
		t.Fatal(err)
	}
	key := genKey(t)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs: []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.cert, key.Public(), c.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return []*x509.Certificate{leaf}
}

func (c *ca) jwtSVID(t *testing.T, sub string, aud []string, exp time.Time) string {
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

// spiffeBundle is c's trust bundle in the SPIFFE bundle format (a JWKS with x509-svid and jwt-svid keys).
func (c *ca) spiffeBundle(t *testing.T, domain string) []byte {
	t.Helper()
	b := spiffebundle.FromX509Authorities(spiffeid.RequireTrustDomainFromString(domain), []*x509.Certificate{c.cert})
	if err := b.AddJWTAuthority(c.kid, c.jwt.Public()); err != nil {
		t.Fatal(err)
	}
	out, err := b.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (c *ca) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
}

func (c *ca) jwks(t *testing.T) []byte {
	t.Helper()
	out, err := jwtbundle.FromJWTAuthorities(spiffeid.RequireTrustDomainFromString(td), map[string]crypto.PublicKey{c.kid: c.jwt.Public()}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

const policy = `{"rules": [
  {"id": "spiffe://herd.test/everdict/ws/acme/runner/r1", "spaces": {"acme": "write"}},
  {"prefix": "spiffe://herd.test/everdict/ws/acme/", "spaces": {"acme": "read"}}
]}`

func write(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newSPIFFE(t *testing.T, bundle []byte) (*auth.SPIFFE, string) {
	t.Helper()
	dir := t.TempDir()
	bp := write(t, dir, "bundle", bundle)
	s, err := auth.NewSPIFFE(auth.SPIFFEConfig{TrustDomain: td, BundleFile: bp, Audience: audience, PolicyFile: write(t, dir, "policy.json", []byte(policy))})
	if err != nil {
		t.Fatalf("NewSPIFFE: %v", err)
	}
	return s, bp
}

func withChain(chain []*x509.Certificate) context.Context {
	return auth.WithPeerChain(context.Background(), chain)
}

func wantRefused(t *testing.T, what string, err error, reason string) {
	t.Helper()
	if !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("%s: %v, want ErrUnauthenticated", what, err)
		return
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("%s: refused with %q, want the reason %q", what, err, reason)
	}
}

// TestSPIFFEAcceptsX509SVID (PLT-489, A-60): an X.509-SVID presented as the mTLS client certificate, chained to the
// bundle, in the trust domain and not expired, is the principal its policy rule maps (an exact id rule over a prefix)
// or, for herd's own identities, its built-in grant with no rule: a node's herdlet credential naming its worker, an
// operator in every atespace, a node's herd-egress reading every atespace.
func TestSPIFFEAcceptsX509SVID(t *testing.T) {
	c := newCA(t, td)
	s, _ := newSPIFFE(t, c.spiffeBundle(t, td))
	now := time.Now()
	for _, tc := range []struct {
		id     string
		check  func(auth.Principal) bool
		reason string
	}{
		{runnerID, func(p auth.Principal) bool { return p.Spaces["acme"] == auth.RoleWrite }, "the exact id rule's write role"},
		{"spiffe://herd.test/everdict/ws/acme/runner/r2", func(p auth.Principal) bool { return p.Spaces["acme"] == auth.RoleRead }, "the prefix rule's read role"},
		{"spiffe://herd.test/herd/node/w1", func(p auth.Principal) bool {
			return p.Herdlet && p.HerdletWorker() == "w1" && p.Spaces[auth.AllSpaces] == auth.RoleEgress
		}, "a herdlet credential for worker w1 that reads every space"},
		{"spiffe://herd.test/herd/operator/alice", func(p auth.Principal) bool { return p.Spaces[auth.AllSpaces] == auth.RoleOperator }, "the operator role"},
		{"spiffe://herd.test/herd/egress/w1", func(p auth.Principal) bool { return p.Spaces[auth.AllSpaces] == auth.RoleEgress && !p.Herdlet }, "the egress role in every space, no herdlet"},
	} {
		p, err := s.Authenticate(withChain(c.x509SVID(t, tc.id, now.Add(-time.Minute), now.Add(time.Hour))), "")
		if err != nil {
			t.Errorf("%s: %v", tc.id, err)
			continue
		}
		if p.Subject != tc.id || p.Scheme != auth.SchemeX509SVID || p.Creator() != tc.id || !tc.check(p) {
			t.Errorf("%s: principal %+v, want subject and creator %s by x509-svid with %s", tc.id, p, tc.id, tc.reason)
		}
	}
}

// TestSPIFFERefusesX509SVID: each refusal names its reason.
func TestSPIFFERefusesX509SVID(t *testing.T) {
	c := newCA(t, td)
	s, _ := newSPIFFE(t, c.spiffeBundle(t, td))
	foreign := newCA(t, "other.test")
	stranger := newCA(t, td) // the right trust domain, a root the bundle does not hold
	now := time.Now()
	valid := func(c *ca, id string) []*x509.Certificate {
		return c.x509SVID(t, id, now.Add(-time.Minute), now.Add(time.Hour))
	}
	for _, tc := range []struct {
		what   string
		chain  []*x509.Certificate
		reason string
	}{
		{"an expired SVID", c.x509SVID(t, runnerID, now.Add(-2*time.Hour), now.Add(-time.Hour)), "expired"},
		{"an SVID of a foreign trust domain", valid(foreign, "spiffe://other.test/everdict/ws/acme/runner/r1"), `foreign trust domain "other.test"`},
		{"an SVID whose chain is not in the bundle", valid(stranger, runnerID), "does not chain to the trust bundle"},
		{"a URI SAN that is no SPIFFE ID", valid(c, "https://herd.test/everdict/ws/acme/runner/r1"), "not a SPIFFE ID"},
		{"a SPIFFE ID whose path names nothing", valid(c, "spiffe://herd.test/somewhere/else"), "names no herd or Everdict identity"},
		{"an identity no policy rule names", valid(c, "spiffe://herd.test/everdict/ws/other/runner/r1"), "no policy rule"},
		{"herd-router, which is no caller of the API", valid(c, "spiffe://herd.test/herd/router"), "no policy rule"},
	} {
		_, err := s.Authenticate(withChain(tc.chain), "")
		wantRefused(t, tc.what, err, tc.reason)
	}
	_, err := s.Authenticate(context.Background(), "")
	wantRefused(t, "no client certificate and no bearer", err, "no client certificate")
}

// TestSPIFFEJWTSVID: a JWT-SVID bearer whose aud names herd-api's audience, signed by a key of the bundle, is
// accepted; a wrong audience, an expiry, a foreign trust domain, an unknown key and an unparseable subject are refused
// with their reason. The keys come from a SPIFFE bundle or from a separate JWKS beside a PEM bundle.
func TestSPIFFEJWTSVID(t *testing.T) {
	c := newCA(t, td)
	s, _ := newSPIFFE(t, c.spiffeBundle(t, td))
	future := time.Now().Add(5 * time.Minute)
	p, err := s.Authenticate(context.Background(), c.jwtSVID(t, runnerID, []string{audience}, future))
	if err != nil {
		t.Fatalf("a valid JWT-SVID: %v", err)
	}
	if p.Subject != runnerID || p.Scheme != auth.SchemeJWTSVID || p.Spaces["acme"] != auth.RoleWrite {
		t.Fatalf("a valid JWT-SVID: principal %+v, want %s by jwt-svid with write in acme", p, runnerID)
	}
	foreign := newCA(t, "other.test")
	other := newCA(t, td)
	for _, tc := range []struct {
		what, token, reason string
	}{
		{"a wrong audience", c.jwtSVID(t, runnerID, []string{"spiffe://herd.test/somebody-else"}, future), "audience"},
		{"an expired JWT-SVID", c.jwtSVID(t, runnerID, []string{audience}, time.Now().Add(-time.Minute)), "expired"},
		{"a foreign trust domain", foreign.jwtSVID(t, "spiffe://other.test/everdict/ws/acme/runner/r1", []string{audience}, future), `foreign trust domain "other.test"`},
		{"a key the bundle does not hold", other.jwtSVID(t, runnerID, []string{audience}, future), "no JWT authority"},
		{"an unparseable subject", c.jwtSVID(t, "not-a-spiffe-id", []string{audience}, future), "not a SPIFFE ID"},
		{"a path that names nothing", c.jwtSVID(t, "spiffe://herd.test/somewhere/else", []string{audience}, future), "names no herd or Everdict identity"},
	} {
		_, err := s.Authenticate(context.Background(), tc.token)
		wantRefused(t, tc.what, err, tc.reason)
	}

	dir := t.TempDir()
	sp, err := auth.NewSPIFFE(auth.SPIFFEConfig{TrustDomain: td, BundleFile: write(t, dir, "bundle.pem", c.pem()),
		JWTBundleFile: write(t, dir, "jwks.json", c.jwks(t)), Audience: audience, PolicyFile: write(t, dir, "policy.json", []byte(policy))})
	if err != nil {
		t.Fatalf("NewSPIFFE with a PEM bundle and a JWKS: %v", err)
	}
	if _, err := sp.Authenticate(context.Background(), c.jwtSVID(t, runnerID, []string{audience}, future)); err != nil {
		t.Errorf("a JWT-SVID verified against the JWKS beside a PEM bundle: %v", err)
	}
	if _, err := sp.Authenticate(withChain(c.x509SVID(t, runnerID, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))), ""); err != nil {
		t.Errorf("an X.509-SVID verified against the PEM bundle: %v", err)
	}
}

// TestSPIFFEBundleReRead: the bundle file is read again when it changes, so a rotated root is trusted without a
// restart and a removed one is not; a bundle that does not parse keeps the last good one and says so.
func TestSPIFFEBundleReRead(t *testing.T) {
	old, next := newCA(t, td), newCA(t, td)
	s, bp := newSPIFFE(t, old.pem())
	now := time.Now()
	svid := func(c *ca) context.Context {
		return withChain(c.x509SVID(t, runnerID, now.Add(-time.Minute), now.Add(time.Hour)))
	}
	if _, err := s.Authenticate(svid(next), ""); err == nil {
		t.Fatal("an SVID of a root not in the bundle yet was accepted")
	}
	// A rotation writes the new root; mtime granularity on some filesystems is coarse, so move it forward.
	if err := os.WriteFile(bp, next.pem(), 0o600); err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Second)
	if err := os.Chtimes(bp, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(svid(next), ""); err != nil {
		t.Errorf("an SVID of the rotated-in root after the bundle changed: %v", err)
	}
	if _, err := s.Authenticate(svid(old), ""); err == nil {
		t.Error("an SVID of the rotated-out root was accepted after the bundle changed")
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	if err := os.WriteFile(bp, []byte("not a bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	later = later.Add(2 * time.Second)
	if err := os.Chtimes(bp, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(svid(next), ""); err != nil {
		t.Errorf("after a bundle that does not parse, the last good bundle: %v", err)
	}
	if !strings.Contains(logs.String(), "keeping the last good bundle") {
		t.Errorf("log %q does not say the bad bundle was not taken", logs.String())
	}
}

// TestSPIFFEPolicyRefusesWhatItCannotMean: a policy rule that names a path no identity parses to, a prefix without
// its trailing slash (which would match /runner/r1 to /runner/r10), another trust domain, an unknown role, or a
// rule under /herd/ (whose grants are built in) is refused when the policy is read.
func TestSPIFFEPolicyRefusesWhatItCannotMean(t *testing.T) {
	c := newCA(t, td)
	for _, tc := range []struct{ policy, reason string }{
		{`{"rules": [{"id": "spiffe://herd.test/nowhere", "spaces": {"a": "write"}}]}`, "names no herd or Everdict identity"},
		{`{"rules": [{"prefix": "spiffe://herd.test/everdict/ws/acme", "spaces": {"a": "write"}}]}`, "must end with /"},
		{`{"rules": [{"id": "spiffe://other.test/herd/api", "spaces": {"a": "read"}}]}`, "trust domain"},
		{`{"rules": [{"id": "spiffe://herd.test/herd/api", "spaces": {"a": "admin"}}]}`, `role "admin"`},
		{`{"rules": [{"prefix": "spiffe://herd.test/herd/node/", "spaces": {"a": "write"}}]}`, "built in"},
		{`{"rules": [{"id": "spiffe://herd.test/herd/operator/bob", "spaces": {"a": "read"}}]}`, "built in"},
		{`{"rules": [{"prefix": "spiffe://herd.test/", "spaces": {"a": "read"}}]}`, "built in"},
		{`{"rules": [{"prefix": "spiffe://herd.test/everdict/", "herdlet": true}]}`, "unknown field"},
		{`{"rules": [{"id": "spiffe://herd.test/herd/api"}], "extra": 1}`, "unknown field"},
	} {
		dir := t.TempDir()
		_, err := auth.NewSPIFFE(auth.SPIFFEConfig{TrustDomain: td, BundleFile: write(t, dir, "b", c.pem()), Audience: audience,
			PolicyFile: write(t, dir, "p.json", []byte(tc.policy))})
		if err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("policy %s: %v, want a refusal naming %q", tc.policy, err, tc.reason)
		}
	}
}

// TestParseID (E3): the one function that reads a SPIFFE ID's path; a path that parses to nothing is refused.
func TestParseID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want auth.Identity
	}{
		{"spiffe://herd.test/everdict/control-plane", auth.Identity{Kind: auth.KindControlPlane}},
		{"spiffe://herd.test/everdict/ws/acme/runner/r1", auth.Identity{Kind: auth.KindRunner, Workspace: "acme", Runner: "r1"}},
		{"spiffe://herd.test/herd/api", auth.Identity{Kind: auth.KindHerdAPI}},
		{"spiffe://herd.test/herd/node/w1", auth.Identity{Kind: auth.KindNode, Worker: "w1"}},
		{"spiffe://herd.test/herd/atespace/a/actor/u1", auth.Identity{Kind: auth.KindActor, Atespace: "a", ActorUID: "u1"}},
		{"spiffe://herd.test/herd/operator/alice", auth.Identity{Kind: auth.KindOperator, Operator: "alice"}},
		{"spiffe://herd.test/herd/router", auth.Identity{Kind: auth.KindRouter}},
		{"spiffe://herd.test/herd/egress/w1", auth.Identity{Kind: auth.KindEgress, Worker: "w1"}},
	} {
		id := spiffeid.RequireFromString(tc.id)
		got, err := auth.ParseID(id)
		tc.want.ID = id
		if err != nil || got != tc.want {
			t.Errorf("ParseID(%s) = %+v, %v; want %+v", tc.id, got, err, tc.want)
		}
	}
	for _, bad := range []string{"spiffe://herd.test", "spiffe://herd.test/everdict/ws/acme", "spiffe://herd.test/everdict/ws/acme/runner/r1/x",
		"spiffe://herd.test/herd/node", "spiffe://herd.test/herd/atespace/a/actor", "spiffe://herd.test/elsewhere"} {
		if got, err := auth.ParseID(spiffeid.RequireFromString(bad)); err == nil {
			t.Errorf("ParseID(%s) = %+v, want a refusal", bad, got)
		}
	}
}

// TestPolicyDeletedOrBroken (review, small items): a policy file deleted at runtime grants no workload anything (the
// built-in grants stay), logged once; one that does not parse keeps the last good policy, logged once, with
// herd_auth_policy_stale at 1 until a good read sets it to 0.
func TestPolicyDeletedOrBroken(t *testing.T) {
	c := newCA(t, td)
	dir := t.TempDir()
	pp := write(t, dir, "policy.json", []byte(policy))
	m := auth.NewMeter()
	s, err := auth.NewSPIFFE(auth.SPIFFEConfig{TrustDomain: td, BundleFile: write(t, dir, "b", c.spiffeBundle(t, td)), Audience: audience, PolicyFile: pp, Meter: m})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	now := time.Now()
	call := func(id string) error {
		_, err := s.Authenticate(withChain(c.x509SVID(t, id, now.Add(-time.Minute), now.Add(time.Hour))), "")
		return err
	}
	touch := func(b []byte) {
		if err := os.WriteFile(pp, b, 0o600); err != nil {
			t.Fatal(err)
		}
		later := time.Now().Add(time.Duration(len(logs.String())+2) * time.Second)
		if err := os.Chtimes(pp, later, later); err != nil {
			t.Fatal(err)
		}
	}

	touch([]byte("{not json"))
	for i := 0; i < 3; i++ {
		if err := call(runnerID); err != nil {
			t.Errorf("a runner while the policy does not parse: %v; want the last good grant", err)
		}
	}
	if n := strings.Count(logs.String(), "keeping the last good policy"); n != 1 {
		t.Errorf("a policy that does not parse logged %d times over three calls, want once:\n%s", n, logs.String())
	}
	if g := m.Gauge(auth.MetricPolicyStale); g != 1 {
		t.Errorf("%s = %v while the policy does not parse, want 1", auth.MetricPolicyStale, g)
	}
	touch([]byte(policy))
	if err := call(runnerID); err != nil {
		t.Fatal(err)
	}
	if g := m.Gauge(auth.MetricPolicyStale); g != 0 {
		t.Errorf("%s = %v after a good read, want 0", auth.MetricPolicyStale, g)
	}

	logs.Reset()
	if err := os.Remove(pp); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		err := call(runnerID)
		wantRefused(t, "a runner after the policy file was deleted", err, "no policy rule")
	}
	if err := call("spiffe://herd.test/herd/operator/alice"); err != nil {
		t.Errorf("an operator after the policy file was deleted: %v; the built-in grants stay", err)
	}
	if n := strings.Count(logs.String(), "deleted"); n != 1 {
		t.Errorf("a deleted policy logged %d times, want once:\n%s", n, logs.String())
	}
}
