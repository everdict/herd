package auth

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
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
	// Source is the process's own identity, whose bundles verify callers (the Workload API's by default). Without it,
	// BundleFile is read.
	Source Source
	// TrustDomain is the one trust domain herd trusts (workload identity spec E2); with a Source it defaults to the
	// Source's SVID's, and must equal it.
	TrustDomain string
	// BundleFile is the trust bundle when there is no Source: PEM X.509 authorities, or a SPIFFE bundle (a JWKS whose
	// keys are used x509-svid and jwt-svid, `spire-server bundle show -format spiffe`). It is read again whenever it
	// changes.
	BundleFile string
	// JWTBundleFile, if set, is a JWKS of JWT-SVID authorities beside a PEM BundleFile, read again when it changes.
	JWTBundleFile string
	// Audience is what a JWT-SVID's aud must name for herd to accept it: herd-api's own SPIFFE ID.
	Audience string
	// PolicyFile grants workloads atespace roles (Policy), read again when it changes; empty grants none beyond the
	// built-in conventions (Builtin).
	PolicyFile string
	// Now is the clock X.509-SVIDs are checked at (default time.Now).
	Now func() time.Time
	// Meter carries the policy's staleness (MetricPolicyStale); nil carries none.
	Meter *Meter
}

// SPIFFE authenticates a caller by its SPIFFE verifiable identity document (workload identity spec W1): an
// X.509-SVID presented as the mTLS client certificate when the call carries no bearer, or a JWT-SVID bearer. The
// SVID must verify against the trust bundle, name a SPIFFE ID in the trust domain whose path ParseID reads, not be
// expired, and (a JWT-SVID) name the audience. herd's own identities then hold their built-in grant (Builtin), and a
// workload the grant its policy rule names; an ID with neither is refused. Every refusal is ErrUnauthenticated with
// its reason.
type SPIFFE struct {
	td       spiffeid.TrustDomain
	audience string
	now      func() time.Time
	trust    func() trust
	policy   *watched[*Policy]
}

// trust is what verifies an SVID: the bundles of the one trust domain herd trusts.
type trust interface {
	GetX509BundleForTrustDomain(spiffeid.TrustDomain) (*x509bundle.Bundle, error)
	GetJWTBundleForTrustDomain(spiffeid.TrustDomain) (*jwtbundle.Bundle, error)
}

// bundles is what one read of the bundle files gives.
type bundles struct {
	x509 *x509bundle.Bundle
	jwt  *jwtbundle.Bundle
}

// NewSPIFFE reads the bundle and the policy; either failing to read or parse is an error here, at start.
func NewSPIFFE(c SPIFFEConfig) (*SPIFFE, error) {
	if c.Audience == "" {
		return nil, errors.New("auth: SPIFFE needs the audience JWT-SVIDs must name")
	}
	s := &SPIFFE{audience: c.Audience, now: c.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if c.Source != nil {
		self, err := Self(c.Source)
		if err != nil {
			return nil, fmt.Errorf("auth: the identity source has no X.509-SVID: %w", err)
		}
		s.td = self.TrustDomain()
		if c.TrustDomain != "" && c.TrustDomain != s.td.Name() {
			return nil, fmt.Errorf("auth: trust domain %q is configured, but this process's SVID %s is of %q", c.TrustDomain, self, s.td)
		}
		t := onlyDomain{td: s.td, src: c.Source}
		s.trust = func() trust { return t }
	} else {
		td, err := spiffeid.TrustDomainFromString(c.TrustDomain)
		if err != nil {
			return nil, fmt.Errorf("auth: trust domain %q: %w", c.TrustDomain, err)
		}
		if c.BundleFile == "" {
			return nil, errors.New("auth: SPIFFE needs an identity source or a bundle file")
		}
		s.td = td
		paths := []string{c.BundleFile}
		if c.JWTBundleFile != "" {
			paths = append(paths, c.JWTBundleFile)
		}
		b, err := watch("bundle", paths, func(b [][]byte) (bundles, error) { return parseBundles(td, b) })
		if err != nil {
			return nil, err
		}
		s.trust = func() trust { return b.get() }
	}
	if c.PolicyFile != "" {
		td := s.td
		var err error
		if s.policy, err = watch("policy", []string{c.PolicyFile}, func(b [][]byte) (*Policy, error) { return ParsePolicy(td, b[0]) }); err != nil {
			return nil, err
		}
		s.policy.missing = func() *Policy { return nil }
		m := c.Meter
		s.policy.stale = func(v bool) {
			if v {
				m.SetGauge(MetricPolicyStale, 1)
			} else {
				m.SetGauge(MetricPolicyStale, 0)
			}
		}
		m.SetGauge(MetricPolicyStale, 0)
	}
	return s, nil
}

// TrustDomain is the trust domain s accepts SVIDs of.
func (s *SPIFFE) TrustDomain() spiffeid.TrustDomain { return s.td }

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
	b := s.trust()
	if token != "" {
		return s.jwtSVID(b, token)
	}
	chain := PeerChain(ctx)
	if len(chain) == 0 {
		return Principal{}, refuse("no client certificate and no bearer")
	}
	return s.x509SVID(b, chain)
}

func (s *SPIFFE) x509SVID(b trust, chain []*x509.Certificate) (Principal, error) {
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

func (s *SPIFFE) jwtSVID(b trust, token string) (Principal, error) {
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
	r, ok := Builtin(ident)
	if !ok {
		var pol *Policy
		if s.policy != nil {
			pol = s.policy.get()
		}
		if r, ok = pol.Match(id); !ok {
			return Principal{}, refuse("no policy rule names %s", id)
		}
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

	// missing, if set, is the value while the files are deleted (a policy: no workload grants), taken once and
	// logged once; stale, if set, is told whether the value in force is a last good one kept over a change that did
	// not load.
	missing func() T
	stale   func(bool)

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
	stamp, serr := w.stampNow()
	if serr == nil && stamp == w.stamp {
		return w.val
	}
	if serr != nil {
		// Each state is logged once: the stamp names it until the files change again.
		state := "missing:" + serr.Error()
		if w.stamp == state {
			return w.val
		}
		w.stamp = state
		if w.missing != nil && errors.Is(serr, fs.ErrNotExist) {
			w.val = w.missing()
			w.setStale(false)
			log.Printf("auth: %s %s was deleted: %v; in force now: none of its grants", w.what, strings.Join(w.paths, ", "), serr)
			return w.val
		}
		w.setStale(true)
		log.Printf("auth: %s %s cannot be read (%v); keeping the last good %s", w.what, strings.Join(w.paths, ", "), serr, w.what)
		return w.val
	}
	stamp, v, err := w.read()
	if err != nil {
		// The same bad bytes are not parsed again on every call, nor logged again; the next change is read.
		w.stamp = stamp
		w.setStale(true)
		log.Printf("auth: %s %s changed but %v; keeping the last good %s", w.what, strings.Join(w.paths, ", "), err, w.what)
		return w.val
	}
	w.stamp, w.val = stamp, v
	w.setStale(false)
	log.Printf("auth: %s %s read again", w.what, strings.Join(w.paths, ", "))
	return w.val
}

func (w *watched[T]) setStale(v bool) {
	if w.stale != nil {
		w.stale(v)
	}
}
