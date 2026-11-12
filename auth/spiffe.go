package auth

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/spiffebundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// SPIFFEConfig configures the SPIFFE authenticator (ADR 0001 A-60).
type SPIFFEConfig struct {
	// TrustDomain is the one trust domain herd trusts (workload identity spec E2), e.g. "everdict.example".
	TrustDomain string
	// BundleFile is the trust bundle: PEM X.509 authorities, or a SPIFFE bundle (a JWKS whose keys are used
	// x509-svid and jwt-svid, `spire-server bundle show -format spiffe`). It is read again whenever it changes.
	BundleFile string
	// JWTBundleFile, if set, is a JWKS of JWT-SVID authorities beside a PEM BundleFile, read again when it changes.
	JWTBundleFile string
	// Audience is what a JWT-SVID's aud must name for herd to accept it (herd-api's own audience).
	Audience string
	// PolicyFile maps SPIFFE IDs to spaces and roles (Policy), read again when it changes.
	PolicyFile string
	// Now is the clock X.509-SVIDs are checked at (default time.Now).
	Now func() time.Time
}

// SPIFFE authenticates a caller by its SPIFFE verifiable identity document (workload identity spec W1): an
// X.509-SVID presented as the mTLS client certificate when the call carries no bearer, or a JWT-SVID bearer. The
// SVID must verify against the trust bundle, name a SPIFFE ID in the trust domain whose path ParseID reads, not be
// expired, and (a JWT-SVID) name the audience. The policy then maps the ID to the principal's spaces and roles; an
// ID no rule names is refused. Every refusal is ErrUnauthenticated with its reason.
type SPIFFE struct {
	td       spiffeid.TrustDomain
	audience string
	now      func() time.Time
	bundle   *watched[bundles]
	policy   *watched[*Policy]
}

// bundles is what one read of the bundle files gives.
type bundles struct {
	x509 *x509bundle.Bundle
	jwt  *jwtbundle.Bundle
}

// NewSPIFFE reads the bundle and the policy; either failing to read or parse is an error here, at start.
func NewSPIFFE(c SPIFFEConfig) (*SPIFFE, error) {
	td, err := spiffeid.TrustDomainFromString(c.TrustDomain)
	if err != nil {
		return nil, fmt.Errorf("auth: trust domain %q: %w", c.TrustDomain, err)
	}
	if c.BundleFile == "" || c.PolicyFile == "" {
		return nil, errors.New("auth: SPIFFE needs a bundle file and a policy file")
	}
	if c.Audience == "" {
		return nil, errors.New("auth: SPIFFE needs the audience JWT-SVIDs must name")
	}
	s := &SPIFFE{td: td, audience: c.Audience, now: c.Now}
	if s.now == nil {
		s.now = time.Now
	}
	paths := []string{c.BundleFile}
	if c.JWTBundleFile != "" {
		paths = append(paths, c.JWTBundleFile)
	}
	if s.bundle, err = watch("bundle", paths, func(b [][]byte) (bundles, error) { return parseBundles(td, b) }); err != nil {
		return nil, err
	}
	if s.policy, err = watch("policy", []string{c.PolicyFile}, func(b [][]byte) (*Policy, error) { return ParsePolicy(td, b[0]) }); err != nil {
		return nil, err
	}
	return s, nil
}

func parseBundles(td spiffeid.TrustDomain, files [][]byte) (bundles, error) {
	var out bundles
	if b := bytes.TrimSpace(files[0]); len(b) > 0 && b[0] == '{' {
		sb, err := spiffebundle.Parse(td, b)
		if err != nil {
			return out, err
		}
		out.x509, out.jwt = sb.X509Bundle(), sb.JWTBundle()
	} else {
		xb, err := x509bundle.Parse(td, b)
		if err != nil {
			return out, err
		}
		out.x509, out.jwt = xb, jwtbundle.New(td)
	}
	if len(files) > 1 {
		jb, err := jwtbundle.Parse(td, files[1])
		if err != nil {
			return out, err
		}
		for kid, k := range jb.JWTAuthorities() {
			if err := out.jwt.AddJWTAuthority(kid, k); err != nil {
				return out, err
			}
		}
	}
	if out.x509.Empty() && out.jwt.Empty() {
		return out, errors.New("the bundle holds no X.509 or JWT authority")
	}
	return out, nil
}

// GetX509BundleForTrustDomain answers only the trust domain herd trusts: an SVID of another is foreign.
func (b bundles) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	if td != b.x509.TrustDomain() {
		return nil, fmt.Errorf("foreign trust domain %q", td)
	}
	return b.x509, nil
}

// GetJWTBundleForTrustDomain is GetX509BundleForTrustDomain for JWT-SVIDs.
func (b bundles) GetJWTBundleForTrustDomain(td spiffeid.TrustDomain) (*jwtbundle.Bundle, error) {
	if td != b.jwt.TrustDomain() {
		return nil, fmt.Errorf("foreign trust domain %q", td)
	}
	return b.jwt, nil
}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnauthenticated, fmt.Sprintf(format, args...))
}

// Authenticate is the SPIFFE authenticator: token is a JWT-SVID; empty, the peer's X.509-SVID.
func (s *SPIFFE) Authenticate(ctx context.Context, token string) (Principal, error) {
	b := s.bundle.get()
	if token != "" {
		return s.jwtSVID(b, token)
	}
	chain := PeerChain(ctx)
	if len(chain) == 0 {
		return Principal{}, refuse("no client certificate and no bearer")
	}
	return s.x509SVID(b, chain)
}

func (s *SPIFFE) x509SVID(b bundles, chain []*x509.Certificate) (Principal, error) {
	leaf := chain[0]
	id, err := x509svid.IDFromCert(leaf)
	if err != nil {
		return Principal{}, refuse("X.509-SVID: the URI SAN is not a SPIFFE ID: %v", err)
	}
	if id.TrustDomain() != s.td {
		return Principal{}, refuse("X.509-SVID %s: foreign trust domain %q (herd trusts %q)", id, id.TrustDomain(), s.td)
	}
	now := s.now()
	if now.After(leaf.NotAfter) {
		return Principal{}, refuse("X.509-SVID %s expired at %s", id, leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	if now.Before(leaf.NotBefore) {
		return Principal{}, refuse("X.509-SVID %s is not valid before %s", id, leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	if _, _, err := x509svid.Verify(chain, b, x509svid.WithTime(now)); err != nil {
		return Principal{}, refuse("X.509-SVID %s does not chain to the trust bundle: %v", id, err)
	}
	return s.principal(id, SchemeX509SVID)
}

func (s *SPIFFE) jwtSVID(b bundles, token string) (Principal, error) {
	// The subject names the bundle that verifies the token; it is read unverified here only to name a refusal.
	if sub, ok := unverifiedSubject(token); ok {
		id, err := spiffeid.FromString(sub)
		if err != nil {
			return Principal{}, refuse("JWT-SVID: the subject %q is not a SPIFFE ID: %v", sub, err)
		}
		if id.TrustDomain() != s.td {
			return Principal{}, refuse("JWT-SVID %s: foreign trust domain %q (herd trusts %q)", id, id.TrustDomain(), s.td)
		}
	}
	sv, err := jwtsvid.ParseAndValidate(token, b, []string{s.audience})
	if err != nil {
		return Principal{}, refuse("JWT-SVID refused: %v", err)
	}
	return s.principal(sv.ID, SchemeJWTSVID)
}

// svidAlgorithms are the JWS algorithms a JWT-SVID may be signed with (the JWT-SVID standard's).
var svidAlgorithms = []jose.SignatureAlgorithm{jose.RS256, jose.RS384, jose.RS512, jose.ES256, jose.ES384, jose.ES512,
	jose.PS256, jose.PS384, jose.PS512}

// unverifiedSubject is a JWT's sub claim, unverified; false if token is no JWS of an SVID algorithm.
func unverifiedSubject(token string) (string, bool) {
	tok, err := jwt.ParseSigned(token, svidAlgorithms)
	if err != nil {
		return "", false
	}
	var c jwt.Claims
	if err := tok.UnsafeClaimsWithoutVerification(&c); err != nil {
		return "", false
	}
	return c.Subject, true
}

// principal maps a verified SPIFFE ID through the policy.
func (s *SPIFFE) principal(id spiffeid.ID, scheme Scheme) (Principal, error) {
	ident, err := ParseID(id)
	if err != nil {
		return Principal{}, refuse("%v", err)
	}
	r, ok := s.policy.get().Match(id)
	if !ok {
		return Principal{}, refuse("no policy rule names %s", id)
	}
	p := Principal{Subject: id.String(), Scheme: scheme, Spaces: r.Spaces, Herdlet: r.Herdlet}
	if r.Herdlet {
		p.Worker = ident.Worker
	}
	return p, nil
}

// peerChainKey carries an explicitly given peer chain (WithPeerChain).
type peerChainKey struct{}

// WithPeerChain returns ctx carrying chain as the caller's client certificate chain (leaf first), whose leaf key the
// TLS handshake proved the caller holds: a TLS server that is not gRPC hands the handshake's PeerCertificates this way.
func WithPeerChain(ctx context.Context, chain []*x509.Certificate) context.Context {
	return context.WithValue(ctx, peerChainKey{}, chain)
}

// PeerChain is the client certificate chain of ctx's caller: one WithPeerChain gave, else the gRPC peer's TLS
// handshake's. Whether the chain is trusted is the authenticator's to verify.
func PeerChain(ctx context.Context) []*x509.Certificate {
	if c, ok := ctx.Value(peerChainKey{}).([]*x509.Certificate); ok {
		return c
	}
	if p, ok := peer.FromContext(ctx); ok {
		if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok {
			return ti.State.PeerCertificates
		}
	}
	return nil
}

// watched is a value parsed from files and parsed again when any of them changes (size or modification time). A
// change that does not read or parse keeps the last good value, logged with the reason, so a rotation caught
// half-written does not lock every caller out; the first read must succeed.
type watched[T any] struct {
	what  string
	paths []string
	parse func([][]byte) (T, error)

	mu    sync.Mutex
	stamp string
	val   T
}

func watch[T any](what string, paths []string, parse func([][]byte) (T, error)) (*watched[T], error) {
	w := &watched[T]{what: what, paths: paths, parse: parse}
	stamp, val, err := w.read()
	if err != nil {
		return nil, fmt.Errorf("auth: %s %s: %w", what, strings.Join(paths, ", "), err)
	}
	w.stamp, w.val = stamp, val
	return w, nil
}

func (w *watched[T]) stampNow() (string, error) {
	var sb strings.Builder
	for _, p := range w.paths {
		fi, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&sb, "%s:%d:%d;", p, fi.Size(), fi.ModTime().UnixNano())
	}
	return sb.String(), nil
}

func (w *watched[T]) read() (string, T, error) {
	var zero T
	stamp, err := w.stampNow()
	if err != nil {
		return "", zero, err
	}
	var files [][]byte
	for _, p := range w.paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", zero, err
		}
		files = append(files, b)
	}
	v, err := w.parse(files)
	return stamp, v, err
}

func (w *watched[T]) get() T {
	w.mu.Lock()
	defer w.mu.Unlock()
	if stamp, err := w.stampNow(); err == nil && stamp == w.stamp {
		return w.val
	}
	stamp, v, err := w.read()
	if err != nil {
		log.Printf("auth: %s %s changed but %v; keeping the last good %s", w.what, strings.Join(w.paths, ", "), err, w.what)
		if stamp != "" {
			// The same bad bytes are not parsed again on every call; the next change is read.
			w.stamp = stamp
		}
		return w.val
	}
	w.stamp, w.val = stamp, v
	log.Printf("auth: %s %s read again", w.what, strings.Join(w.paths, ", "))
	return w.val
}
