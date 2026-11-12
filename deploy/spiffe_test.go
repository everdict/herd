package deploy_test

import (
	"context"
	"crypto/tls"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/auth/localca"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/deploy"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const runnerID1 = "spiffe://herd.test/everdict/ws/acme/runner/r1"

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// runAPI runs a and answers the error Run ends with.
func runAPI(t *testing.T, a *deploy.API, ln net.Listener) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()
	t.Cleanup(cancel)
	return done
}

// asSVID dials herd-api at addr as src (its X.509-SVID), with tok as bearer if set.
func asSVID(t *testing.T, addr string, src auth.Source, tok string) *conformance.Client {
	t.Helper()
	cfg, err := auth.ClientTLS(src, auth.HerdID(spiffeid.RequireTrustDomainFromString(testTD), "api"))
	if err != nil {
		t.Fatal(err)
	}
	c := conformance.NewClientOpts(addr, tok, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	t.Cleanup(c.Close)
	return c
}

// noCert dials herd-api at addr over TLS with no client certificate (a JWT-SVID bearer's caller), trusting the tests'
// CA for herd-api's SVID.
func noCert(t *testing.T, addr, tok string) *conformance.Client {
	t.Helper()
	cfg, err := auth.ClientTLS(source(t, operatorID), auth.HerdID(spiffeid.RequireTrustDomainFromString(testTD), "api"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &tls.Certificate{}, nil }
	c := conformance.NewClientOpts(addr, tok, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	t.Cleanup(c.Close)
	return c
}

func tpl(name string) *ateapi.CreateActorTemplateRequest {
	return &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: name}, Containers: []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
		SnapshotConfig: &ateapi.SnapshotConfig{GoldenSnapshotConfig: coldBoot}}}
}

// TestAPIServesSPIFFE (PLT-489, ADR 0001 A-60): herd-api's one listener serves gRPC over TLS with its SVID. A caller
// is its client X.509-SVID (a runner the policy grants, an operator by the built-in convention) or, with no client
// certificate, the JWT-SVID it carries for herd-api's own ID; a chain of another issuer is refused with its reason, and
// so is an HS256 bearer without the break-glass. The allowance counters are at /metrics.
func TestAPIServesSPIFFE(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"rules": [{"id": "`+runnerID1+`", "spaces": {"team-a": "write"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, mln := listen(t), listen(t)
	a := &deploy.API{Identity: apiIdentity(t), Policy: policy, Store: "bolt:" + filepath.Join(dir, "herd.db"),
		Snapshots: filepath.Join(dir, "snapshots"), CallTimeout: 5 * time.Second, Reconcile: time.Second, MetricsListener: mln}
	runAPI(t, a, ln)
	ctx := ctxT(t)
	addr := ln.Addr().String()

	runner := asSVID(t, addr, source(t, runnerID1), "")
	var err error
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		_, err = runner.CreateActorTemplate(ctx, tpl("by-x509"))
		if err == nil || status.Code(err) != codes.Unavailable || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("CreateActorTemplate as %s's X.509-SVID: %v", runnerID1, err)
	}
	_, err = runner.ListActors(ctx, &ateapi.ListActorsRequest{Atespace: "team-b"})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), runnerID1) {
		t.Errorf("ListActors in a space the policy does not grant: %v; want PermissionDenied naming %s", err, runnerID1)
	}
	if _, err := asSVID(t, addr, source(t, operatorID), "").ListActors(ctx, &ateapi.ListActorsRequest{Atespace: "team-b"}); err != nil {
		t.Errorf("ListActors as an operator (built in, no rule): %v", err)
	}

	// No client certificate: the JWT-SVID bearer for herd-api's ID is the caller; one for another audience is not.
	if _, err := noCert(t, addr, jwtFor(t, runnerID1)).CreateActorTemplate(ctx, tpl("by-jwt")); err != nil {
		t.Fatalf("CreateActorTemplate with a JWT-SVID bearer and no client certificate: %v", err)
	}
	other, err := testCA(t).JWTSVID(runnerID1, []string{"spiffe://herd.test/herd/router"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, err = noCert(t, addr, other).ListActors(ctx, &ateapi.ListActorsRequest{Atespace: space})
	if status.Code(err) != codes.Unauthenticated || !strings.Contains(err.Error(), "audience") {
		t.Errorf("a JWT-SVID for another audience: %v; want Unauthenticated naming the audience", err)
	}

	// A client certificate of another issuer.
	stranger, err := localca.New(testTD)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := stranger.Source(runnerID1, localca.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ss.X509 = testCA(t).X509Bundle() // it trusts herd-api; herd-api does not trust it
	_, err = asSVID(t, addr, ss, "").ListActors(ctx, &ateapi.ListActorsRequest{Atespace: space})
	if status.Code(err) != codes.Unauthenticated || !strings.Contains(err.Error(), "does not chain to the trust bundle") {
		t.Errorf("a client certificate outside the bundle: %v; want Unauthenticated naming the bundle", err)
	}

	// HS256 without the break-glass.
	hs, err := auth.Sign(make([]byte, 32), auth.Claims{Subject: "everdict", ExpiresAt: time.Now().Add(time.Hour).Unix(), Spaces: map[string]auth.Role{space: auth.RoleWrite}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = noCert(t, addr, hs).ListActors(ctx, &ateapi.ListActorsRequest{Atespace: space})
	if status.Code(err) != codes.Unauthenticated || !strings.Contains(err.Error(), "no break-glass allowance") {
		t.Errorf("an HS256 bearer without -break-glass-hs256-until: %v; want Unauthenticated naming the allowance", err)
	}

	resp, err := http.Get("http://" + mln.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "# TYPE "+auth.MetricBreakGlassCalls+" counter") {
		t.Errorf("/metrics does not list %s:\n%s", auth.MetricBreakGlassCalls, body)
	}
}

// waitErr is the error Run ended with within d.
func waitErr(t *testing.T, done <-chan error, d time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("still running after %s; want it to refuse to start", d)
		return nil
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestAPIFailsClosed (A-60): herd-api with no identity does not start, and never serves in cleartext or on HS256
// instead: no Workload API at the socket, a file override missing its key, an SVID that is not herd-api's, no policy
// file. Each refusal says why.
func TestAPIFailsClosed(t *testing.T) {
	dir := t.TempDir()
	cert, _, err := localca.WriteSVID(dir, "svid", must(testCA(t).SVID("spiffe://herd.test/herd/api", localca.Options{})))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		a      deploy.API
		reason string
	}{
		{"no Workload API", deploy.API{Identity: deploy.Identity{Socket: "unix:///tmp/herd-no-agent/api.sock", Wait: 300 * time.Millisecond}, Policy: noRules(t)},
			"no SPIFFE identity"},
		{"a file override without its key", deploy.API{Identity: deploy.Identity{TLSCert: cert, Bundle: cert}, Policy: noRules(t)}, "no SPIFFE identity"},
		{"another component's SVID", deploy.API{Identity: routerIdentity(t), Policy: noRules(t)}, "herd's components reach it as spiffe://herd.test/herd/api"},
		{"no policy file", deploy.API{Identity: apiIdentity(t), Policy: filepath.Join(dir, "absent.json")}, "no workload gets an atespace grant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.a
			a.Store, a.Snapshots = "memory", t.TempDir()
			ln := listen(t)
			err := waitErr(t, runAPI(t, &a, ln), 10*time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("herd-api with %s: %v; want a refusal to start naming %q", tc.name, err, tc.reason)
			}
			// Nothing serves on the listener: an HTTP/2 preface gets no answer.
			c, derr := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
			if derr == nil {
				_ = c.SetDeadline(time.Now().Add(500 * time.Millisecond))
				_, werr := c.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"))
				buf := make([]byte, 1)
				if _, rerr := c.Read(buf); werr == nil && rerr == nil {
					t.Error("something answered on herd-api's listener after it refused to start")
				}
				c.Close()
			}
		})
	}
}

// TestBreakGlassFlagIsDated (A-60): HS256 bearers are accepted, and counted, only with -break-glass-hs256-until, and
// herd-api and herd-router refuse to start once its day has passed, or with -key-file and no date.
func TestBreakGlassFlagIsDated(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	ln := listen(t)
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02")
	m := auth.NewMeter()
	a := &deploy.API{Identity: apiIdentity(t), Policy: noRules(t), Store: "memory", Snapshots: t.TempDir(),
		BreakGlass: deploy.BreakGlass{Until: tomorrow, Key: key}, Meter: m}
	runAPI(t, a, ln)
	hs, err := auth.Sign(key, auth.Claims{Subject: "everdict", ExpiresAt: time.Now().Add(time.Hour).Unix(), Spaces: map[string]auth.Role{space: auth.RoleRead}})
	if err != nil {
		t.Fatal(err)
	}
	c := noCert(t, ln.Addr().String(), hs)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		_, err = c.ListActors(ctxT(t), &ateapi.ListActorsRequest{Atespace: space})
		if err == nil || status.Code(err) != codes.Unavailable || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("an HS256 bearer under -break-glass-hs256-until %s: %v", tomorrow, err)
	}
	if n := m.Value(auth.MetricBreakGlassCalls, "component", "herd-api", "subject", "everdict"); n != 1 {
		t.Errorf("%s{subject=everdict} = %d after one call, want 1", auth.MetricBreakGlassCalls, n)
	}

	yesterday := time.Now().UTC().Add(-24 * time.Hour).Format("2006-01-02")
	for _, tc := range []struct {
		name   string
		bg     deploy.BreakGlass
		reason string
	}{
		{"a date that has passed", deploy.BreakGlass{Until: yesterday, Key: key}, "has passed"},
		{"a key with no date", deploy.BreakGlass{Key: key}, "needs -break-glass-hs256-until"},
		{"a date that is not one", deploy.BreakGlass{Until: "next week", Key: key}, "want YYYY-MM-DD"},
		{"a date past 30 days", deploy.BreakGlass{Until: time.Now().UTC().Add(32 * 24 * time.Hour).Format("2006-01-02"), Key: key}, "at most 30 days"},
	} {
		api := &deploy.API{Identity: apiIdentity(t), Policy: noRules(t), Store: "memory", Snapshots: t.TempDir(), BreakGlass: tc.bg}
		if err := waitErr(t, runAPI(t, api, listen(t)), 10*time.Second); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("herd-api with %s: %v; want a refusal to start naming %q", tc.name, err, tc.reason)
		}
		rt := &deploy.Router{Identity: routerIdentity(t), API: "127.0.0.1:1", DefaultPort: 8080, BreakGlass: tc.bg}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- rt.Run(ctx, listen(t), listen(t)) }()
		err := waitErr(t, done, 10*time.Second)
		cancel()
		if err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("herd-router with %s: %v; want a refusal to start naming %q", tc.name, err, tc.reason)
		}
	}
}

// TestRouterFailsClosed (A-60): herd-router with no identity, or with herd-api's SVID, does not start.
func TestRouterFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		r      deploy.Router
		reason string
	}{
		{"no Workload API", deploy.Router{Identity: deploy.Identity{Socket: "unix:///tmp/herd-no-agent/api.sock", Wait: 300 * time.Millisecond}}, "no SPIFFE identity"},
		{"herd-api's SVID", deploy.Router{Identity: apiIdentity(t)}, "reach it as spiffe://herd.test/herd/router"},
	} {
		r := tc.r
		r.API, r.DefaultPort = "127.0.0.1:1", 8080
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx, listen(t), listen(t)) }()
		err := waitErr(t, done, 10*time.Second)
		cancel()
		if err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("herd-router with %s: %v; want a refusal to start naming %q", tc.name, err, tc.reason)
		}
	}
}

// TestLocalCAOnlyInAllInOne (A-60): herd all-in-one's development CA cannot serve a real install: a herd-api or
// herd-router that is not all-in-one refuses to start on an SVID of its trust domain, herd.local.
func TestLocalCAOnlyInAllInOne(t *testing.T) {
	dev, err := localca.New(deploy.LocalTrustDomain)
	if err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(policy, []byte(`{"rules": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	api := deploy.Identity{Source: must(dev.Source("spiffe://herd.local/herd/api", localca.Options{}))}
	a := &deploy.API{Identity: api, Policy: policy, Store: "memory", Snapshots: t.TempDir()}
	if err := waitErr(t, runAPI(t, a, listen(t)), 10*time.Second); err == nil || !strings.Contains(err.Error(), "local development CA") {
		t.Errorf("herd-api on herd.local outside all-in-one: %v; want a refusal to start naming the local CA", err)
	}
	r := &deploy.Router{Identity: deploy.Identity{Source: must(dev.Source("spiffe://herd.local/herd/router", localca.Options{}))},
		API: "127.0.0.1:1", DefaultPort: 8080}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, listen(t), listen(t)) }()
	if err := waitErr(t, done, 10*time.Second); err == nil || !strings.Contains(err.Error(), "local development CA") {
		t.Errorf("herd-router on herd.local outside all-in-one: %v; want a refusal to start naming the local CA", err)
	}
}

// TestMetricsDefaultToLoopback (review 9): /metrics carries identities in its labels, so the flags bind it to the
// loopback address by default; an explicit address exposes it.
func TestMetricsDefaultToLoopback(t *testing.T) {
	for _, c := range []struct {
		name  string
		flags func(*flag.FlagSet)
	}{
		{"herd-api", (&deploy.API{}).Flags},
		{"herd-router", (&deploy.Router{}).Flags},
	} {
		fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
		c.flags(fs)
		if d := fs.Lookup("metrics-listen").DefValue; !strings.HasPrefix(d, "127.0.0.1:") {
			t.Errorf("%s -metrics-listen defaults to %q, want the loopback address", c.name, d)
		}
	}
}

// TestShortLivedSVIDsOverLongConnections (PLT-489 review 1, re-review A): a caller's X.509-SVID is authenticated at its
// connection's handshake. With the client's SVIDs of a few seconds rotating and the server's living a day, one client
// connection keeps being answered over three client lifetimes: each call verifies the leaf as of the handshake, and
// the connection drains before the leaf ends, so the client reconnects with its current one.
func TestShortLivedSVIDsOverLongConnections(t *testing.T) {
	prev := auth.DrainMargin
	auth.DrainMargin = time.Second
	t.Cleanup(func() { auth.DrainMargin = prev })
	const ttl = 3 * time.Second
	ln := listen(t)
	// The server's SVID lives a day, the client's 3 s: the connection's life follows the client's leaf, not the server's.
	a := &deploy.API{Identity: apiIdentity(t), Policy: noRules(t),
		Store: "memory", Snapshots: t.TempDir()}
	runAPI(t, a, ln)
	op := newRotating(t, operatorID, ttl)
	c := asSVID(t, ln.Addr().String(), op, "")
	end := time.Now().Add(3 * ttl)
	calls, refused := 0, 0
	for time.Now().Before(end) {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := c.ListActors(cctx, &ateapi.ListActorsRequest{Atespace: space})
		cancel()
		calls++
		if status.Code(err) == codes.Unauthenticated {
			refused++
			if refused == 1 {
				t.Logf("refused: %v", err)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if refused > 0 {
		t.Errorf("%d of %d calls over one client connection were refused Unauthenticated as its SVIDs rotated", refused, calls)
	}
}

// TestHerdletRelinksWithItsCurrentSVID (review 1): a herdlet's link ends Unauthenticated when the node SVID it opened
// with expires; the herdlet links again over a new connection with its current SVID instead of retrying the old one,
// so a node of short-lived SVIDs stays a live worker.
func TestHerdletRelinksWithItsCurrentSVID(t *testing.T) {
	a := startAPI(t)
	const ttl = 3 * time.Second
	h := herdletOn(t, "node-1", nil)
	h.API, h.Snapshots = a.addr, filepath.Join(a.dir, "snapshots")
	h.Identity = deploy.Identity{Source: newRotating(t, "spiffe://herd.test/herd/node/node-1", ttl)}
	runHerdlet(t, h)
	c := client(t, a.addr, "")
	defer c.Close()
	active := func() bool {
		var resp ateapi.ListWorkersResponse
		if err := c.Raw(ctxT(t), "ListWorkers", &ateapi.ListWorkersRequest{}, &resp); err != nil {
			return false
		}
		for _, w := range resp.GetWorkers() {
			if w.GetMetadata().GetName() == "node-1" && w.GetStatus().GetState() == ateapi.WorkerState_WORKER_STATE_ACTIVE {
				return true
			}
		}
		return false
	}
	time.Sleep(3 * ttl)
	deadline := time.Now().Add(ttl)
	for !active() {
		if time.Now().After(deadline) {
			t.Fatalf("node-1 is not a live worker %s after it started, with SVIDs of %s", 4*ttl, ttl)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// switchable is an identity source whose SVID a test replaces.
type switchable struct {
	mu  sync.Mutex
	cur auth.Static
}

func (s *switchable) set(v auth.Static) { s.mu.Lock(); s.cur = v; s.mu.Unlock() }
func (s *switchable) get() auth.Static  { s.mu.Lock(); defer s.mu.Unlock(); return s.cur }
func (s *switchable) GetX509SVID() (*x509svid.SVID, error) {
	return s.get().GetX509SVID()
}
func (s *switchable) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return s.get().GetX509BundleForTrustDomain(td)
}
func (s *switchable) GetJWTBundleForTrustDomain(td spiffeid.TrustDomain) (*jwtbundle.Bundle, error) {
	return s.get().GetJWTBundleForTrustDomain(td)
}

// TestRotationToAnotherIDStops (review 8): a Workload API that rotates a process to another SPIFFE ID is refused: the
// process stops with an error naming both IDs, so its supervisor restarts it and it starts, or refuses to, as what it
// now is.
func TestRotationToAnotherIDStops(t *testing.T) {
	prev := deploy.IdentityCheck
	deploy.IdentityCheck = 100 * time.Millisecond
	t.Cleanup(func() { deploy.IdentityCheck = prev })
	sw := &switchable{}
	sw.set(must(testCA(t).Source("spiffe://herd.test/herd/api", localca.Options{})))
	a := &deploy.API{Identity: deploy.Identity{Source: sw}, Policy: noRules(t), Store: "memory", Snapshots: t.TempDir()}
	done := runAPI(t, a, listen(t))
	time.Sleep(500 * time.Millisecond)
	sw.set(must(testCA(t).Source("spiffe://herd.test/herd/operator/someone", localca.Options{})))
	err := waitErr(t, done, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "spiffe://herd.test/herd/api") || !strings.Contains(err.Error(), "spiffe://herd.test/herd/operator/someone") {
		t.Errorf("herd-api rotated to another SPIFFE ID: %v; want it to stop with an error naming both", err)
	}
}

// TestEgressRequiresItsOwnSVID (review 8): herd-egress runs only as herd-egress of this node's worker.
func TestEgressRequiresItsOwnSVID(t *testing.T) {
	for _, c := range []struct{ id, reason string }{
		{"spiffe://herd.test/herd/node/w1", "not a herd-egress"},
		{"spiffe://herd.test/herd/egress/w2", `worker "w2"`},
	} {
		eg := &deploy.Egress{API: "127.0.0.1:1", Worker: "w1", Identity: deploy.Identity{Source: source(t, c.id)},
			Bindings: filepath.Join(t.TempDir(), "b.json"), CAOut: filepath.Join(t.TempDir(), "ca.pem"), DNSAnswer: "198.18.0.1"}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := eg.Run(ctx, listen(t), must(net.ListenPacket("udp", "127.0.0.1:0")))
		cancel()
		if err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("herd-egress of w1 as %s: %v; want a refusal naming %q", c.id, err, c.reason)
		}
	}
}
