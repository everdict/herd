package deploy_test

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/auth/localca"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/deploy"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// The identities of these tests: one local CA stands in for the cluster's SPIRE (ADR 0001 A-60).
const (
	testTD     = "herd.test"
	operatorID = "spiffe://herd.test/herd/operator/tests"
)

var (
	caOnce sync.Once
	ca     *localca.CA
	caErr  error
)

func testCA(t testing.TB) *localca.CA {
	t.Helper()
	caOnce.Do(func() { ca, caErr = localca.New(testTD) })
	if caErr != nil {
		t.Fatal(caErr)
	}
	return ca
}

func source(t testing.TB, id string) auth.Source {
	t.Helper()
	s, err := testCA(t).Source(id, localca.Options{DNS: []string{"localhost"}, IPs: []net.IP{net.IPv4(127, 0, 0, 1)}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func identity(t testing.TB, path string) deploy.Identity {
	return deploy.Identity{Source: source(t, "spiffe://"+testTD+path)}
}

func apiIdentity(t testing.TB) deploy.Identity    { return identity(t, "/herd/api") }
func routerIdentity(t testing.TB) deploy.Identity { return identity(t, "/herd/router") }
func nodeIdentity(t testing.TB, worker string) deploy.Identity {
	return identity(t, "/herd/node/"+worker)
}

// noRules is a policy of no rules: the tests' clients are operators, whose grant is built in.
func noRules(t testing.TB) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spiffe-policy.json")
	if err := os.WriteFile(p, []byte(`{"rules": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// operatorTLS dials herd-api as the tests' operator, accepting only herd-api's SVID.
func operatorTLS(t testing.TB) grpc.DialOption {
	t.Helper()
	cfg, err := auth.ClientTLS(source(t, operatorID), auth.HerdID(spiffeid.RequireTrustDomainFromString(testTD), "api"))
	if err != nil {
		t.Fatal(err)
	}
	return grpc.WithTransportCredentials(credentials.NewTLS(cfg))
}

// transports are the dial options of herd-apis whose identity is not the tests' CA's (an all-in-one's own local CA),
// by address; every other address is dialed with operatorTLS.
var transports sync.Map

func transportFor(t testing.TB, addr string) grpc.DialOption {
	if o, ok := transports.Load(addr); ok {
		return o.(grpc.DialOption)
	}
	return operatorTLS(t)
}

// client is a Control client of herd-api at addr as an operator (its X.509-SVID), with tok as its bearer if set.
func client(t testing.TB, addr, tok string) *conformance.Client {
	return conformance.NewClientOpts(addr, tok, transportFor(t, addr))
}

// localOperator registers the all-in-one at addr, whose identities come from the local CA in data: it is dialed as
// the operator SVID that CA issues.
func localOperator(t testing.TB, data, addr string) {
	t.Helper()
	lca, err := localca.LoadOrCreate(deploy.IdentityDir(data), deploy.LocalTrustDomain)
	if err != nil {
		t.Fatal(err)
	}
	op, err := lca.Source(deploy.LocalOperator, localca.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := auth.ClientTLS(op, auth.HerdID(lca.TD, "api"))
	if err != nil {
		t.Fatal(err)
	}
	transports.Store(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
}

// jwtFor is a JWT-SVID of sub for herd-api.
func jwtFor(t testing.TB, sub string) string {
	t.Helper()
	tok, err := testCA(t).JWTSVID(sub, []string{"spiffe://" + testTD + "/herd/api"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// rotating is an identity source whose SVID for id lives ttl and is issued again every ttl/4 (sooner than SPIRE's
// half-life rotation), as a Workload API's stream rotates a short-lived SVID.
type rotating struct {
	mu   sync.Mutex
	cur  auth.Static
	stop chan struct{}
}

func newRotating(t testing.TB, id string, ttl time.Duration) *rotating {
	t.Helper()
	r := &rotating{stop: make(chan struct{})}
	issue := func() {
		s, err := testCA(t).Source(id, localca.Options{DNS: []string{"localhost"}, IPs: []net.IP{net.IPv4(127, 0, 0, 1)}, TTL: ttl, NotBefore: time.Now()})
		if err != nil {
			t.Error(err)
			return
		}
		r.mu.Lock()
		r.cur = s
		r.mu.Unlock()
	}
	issue()
	go func() {
		tk := time.NewTicker(ttl / 4)
		defer tk.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-tk.C:
				issue()
			}
		}
	}()
	t.Cleanup(func() { close(r.stop) })
	return r
}

func (r *rotating) get() auth.Static {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur
}

func (r *rotating) GetX509SVID() (*x509svid.SVID, error) { return r.get().GetX509SVID() }
func (r *rotating) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return r.get().GetX509BundleForTrustDomain(td)
}
func (r *rotating) GetJWTBundleForTrustDomain(td spiffeid.TrustDomain) (*jwtbundle.Bundle, error) {
	return r.get().GetJWTBundleForTrustDomain(td)
}
