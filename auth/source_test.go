package auth_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/auth/localca"
	"github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"google.golang.org/grpc"
)

// fakeWorkloadAPI is a SPIFFE Workload API serving one workload: its X.509-SVID and the CA's bundles, sending a new
// SVID down every open stream on rotate.
type fakeWorkloadAPI struct {
	workload.UnimplementedSpiffeWorkloadAPIServer
	ca *localca.CA

	mu      sync.Mutex
	svid    *x509svid.SVID
	streams []chan struct{}
}

func (f *fakeWorkloadAPI) response() (*workload.X509SVIDResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var chain []byte
	for _, c := range f.svid.Certificates {
		chain = append(chain, c.Raw...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(f.svid.PrivateKey)
	if err != nil {
		return nil, err
	}
	return &workload.X509SVIDResponse{Svids: []*workload.X509SVID{{SpiffeId: f.svid.ID.String(), X509Svid: chain, X509SvidKey: key, Bundle: f.ca.Root.Raw}}}, nil
}

func (f *fakeWorkloadAPI) FetchX509SVID(_ *workload.X509SVIDRequest, s grpc.ServerStreamingServer[workload.X509SVIDResponse]) error {
	ch := make(chan struct{}, 1)
	f.mu.Lock()
	f.streams = append(f.streams, ch)
	f.mu.Unlock()
	for {
		r, err := f.response()
		if err != nil {
			return err
		}
		if err := s.Send(r); err != nil {
			return err
		}
		select {
		case <-s.Context().Done():
			return nil
		case <-ch:
		}
	}
}

func (f *fakeWorkloadAPI) FetchJWTBundles(_ *workload.JWTBundlesRequest, s grpc.ServerStreamingServer[workload.JWTBundlesResponse]) error {
	jwks, err := f.ca.JWTBundle().Marshal()
	if err != nil {
		return err
	}
	if err := s.Send(&workload.JWTBundlesResponse{Bundles: map[string][]byte{f.ca.TD.Name(): jwks}}); err != nil {
		return err
	}
	<-s.Context().Done()
	return nil
}

func (f *fakeWorkloadAPI) rotate(sv *x509svid.SVID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.svid = sv
	for _, ch := range f.streams {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// serveWorkloadAPI serves f on a unix socket and returns its Workload API address.
func serveWorkloadAPI(t *testing.T, f *fakeWorkloadAPI) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	workload.RegisterSpiffeWorkloadAPIServer(g, f)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	return "unix://" + sock
}

func mustCA(t *testing.T, td string) *localca.CA {
	t.Helper()
	ca, err := localca.New(td)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func mustSVID(t *testing.T, ca *localca.CA, id string, o localca.Options) *x509svid.SVID {
	t.Helper()
	sv, err := ca.SVID(id, o)
	if err != nil {
		t.Fatal(err)
	}
	return sv
}

// TestWorkloadAPISource (PLT-489, A-60): the identity comes from the Workload API: the process's X.509-SVID, the X.509
// and JWT bundles, and a rotated SVID once the agent streams it, with no restart. An agent that is not there is an
// error within the wait, naming the address: the component refuses to start.
func TestWorkloadAPISource(t *testing.T) {
	ca := mustCA(t, td)
	f := &fakeWorkloadAPI{ca: ca, svid: mustSVID(t, ca, "spiffe://herd.test/herd/api", localca.Options{})}
	addr := serveWorkloadAPI(t, f)
	src, err := auth.WorkloadAPI(context.Background(), addr, 10*time.Second)
	if err != nil {
		t.Fatalf("WorkloadAPI: %v", err)
	}
	defer src.Close()
	self, err := auth.Self(src)
	if err != nil || self.String() != "spiffe://herd.test/herd/api" {
		t.Fatalf("Self = %v, %v; want the SVID the agent sent", self, err)
	}
	if b, err := src.GetJWTBundleForTrustDomain(ca.TD); err != nil || b.Empty() {
		t.Errorf("JWT bundle from the Workload API: %v, %v", b, err)
	}

	// The authenticator built on it verifies an SVID of the CA and a JWT-SVID whose aud is herd-api's own ID.
	s, err := auth.NewSPIFFE(auth.SPIFFEConfig{Source: src, Audience: self.String()})
	if err != nil {
		t.Fatalf("NewSPIFFE on the Workload API: %v", err)
	}
	if s.TrustDomain().Name() != td {
		t.Errorf("trust domain %s, want the SVID's %s", s.TrustDomain(), td)
	}
	op := mustSVID(t, ca, "spiffe://herd.test/herd/operator/alice", localca.Options{})
	if p, err := s.Authenticate(withChain(op.Certificates), ""); err != nil || p.Spaces[auth.AllSpaces] != auth.RoleOperator {
		t.Errorf("an operator's X.509-SVID against the Workload API's bundle: %+v, %v", p, err)
	}
	tok, err := ca.JWTSVID("spiffe://herd.test/herd/operator/alice", []string{self.String()}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(context.Background(), tok); err != nil {
		t.Errorf("a JWT-SVID against the Workload API's JWT bundle: %v", err)
	}

	next := mustSVID(t, ca, "spiffe://herd.test/herd/api", localca.Options{})
	f.rotate(next)
	deadline := time.Now().Add(5 * time.Second)
	for {
		cur, _ := src.GetX509SVID()
		if cur.Certificates[0].SerialNumber.Cmp(next.Certificates[0].SerialNumber) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rotated SVID the agent streamed was not taken")
		}
		time.Sleep(20 * time.Millisecond)
	}

	_, err = auth.WorkloadAPI(context.Background(), "unix:///tmp/no-such-agent/api.sock", 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "/tmp/no-such-agent/api.sock") {
		t.Errorf("no agent: %v, want an error naming the address", err)
	}
}

// TestSPIFFEOnSourceTrustsOneDomain (E2): on an identity source the authenticator trusts the SVID's trust domain only,
// and a configured trust domain that is not the SVID's is refused at start.
func TestSPIFFEOnSourceTrustsOneDomain(t *testing.T) {
	ca, other := mustCA(t, td), mustCA(t, "other.test")
	src, err := ca.Source("spiffe://herd.test/herd/api", localca.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.NewSPIFFE(auth.SPIFFEConfig{Source: src, TrustDomain: "other.test", Audience: "spiffe://herd.test/herd/api"}); err == nil {
		t.Error("a configured trust domain other than the SVID's was accepted")
	}
	s, err := auth.NewSPIFFE(auth.SPIFFEConfig{Source: src, Audience: "spiffe://herd.test/herd/api"})
	if err != nil {
		t.Fatal(err)
	}
	foreign := mustSVID(t, other, "spiffe://other.test/herd/operator/alice", localca.Options{})
	_, err = s.Authenticate(withChain(foreign.Certificates), "")
	wantRefused(t, "an operator SVID of another trust domain", err, "foreign trust domain")
}

// TestFileSource (A-60): where a cluster has no Workload API, the SVID and bundle come from files, read again when they
// change.
func TestFileSource(t *testing.T) {
	ca := mustCA(t, td)
	dir := t.TempDir()
	cert, key, err := localca.WriteSVID(dir, "svid", mustSVID(t, ca, "spiffe://herd.test/herd/api", localca.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "bundle.pem")
	if err := ca.WriteBundle(bundle); err != nil {
		t.Fatal(err)
	}
	src, err := auth.Files(cert, key, bundle, "")
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	first, _ := src.GetX509SVID()
	next := mustSVID(t, ca, "spiffe://herd.test/herd/api", localca.Options{})
	if _, _, err := localca.WriteSVID(dir, "svid", next); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	for _, p := range []string{cert, key} {
		if err := os.Chtimes(p, later, later); err != nil {
			t.Fatal(err)
		}
	}
	cur, _ := src.GetX509SVID()
	if cur.Certificates[0].SerialNumber.Cmp(first.Certificates[0].SerialNumber) == 0 || cur.Certificates[0].SerialNumber.Cmp(next.Certificates[0].SerialNumber) != 0 {
		t.Error("the rewritten SVID files were not read again")
	}
	if _, err := auth.Files(cert, "", bundle, ""); err == nil {
		t.Error("an SVID with no key file was accepted")
	}
}

// TestServerAndClientTLS (A-60): herd's listener presents its SVID and leaves the client certificate optional at the
// TLS layer; herd's dialer presents its own and accepts only the server ID it names.
func TestServerAndClientTLS(t *testing.T) {
	ca := mustCA(t, td)
	api, err := ca.Source("spiffe://herd.test/herd/api", localca.Options{})
	if err != nil {
		t.Fatal(err)
	}
	node, err := ca.Source("spiffe://herd.test/herd/node/w1", localca.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", auth.ServerTLS(api))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	peers := make(chan int, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			tc := c.(*tls.Conn)
			if tc.Handshake() == nil {
				peers <- len(tc.ConnectionState().PeerCertificates)
			} else {
				peers <- -1
			}
			c.Close()
		}
	}()
	dial := func(cfg *tls.Config) error {
		c, err := tls.Dial("tcp", ln.Addr().String(), cfg)
		if err != nil {
			return err
		}
		defer c.Close()
		return c.Handshake()
	}
	cfg, err := auth.ClientTLS(node, auth.HerdID(spiffeid.RequireTrustDomainFromString(td), "api"))
	if err != nil {
		t.Fatal(err)
	}
	if err := dial(cfg); err != nil {
		t.Fatalf("a node dialing herd-api: %v", err)
	}
	if n := <-peers; n != 1 {
		t.Errorf("herd-api saw %d client certificates, want the node's", n)
	}
	// A client with no certificate (a JWT-SVID bearer) completes the handshake.
	if err := dial(&tls.Config{InsecureSkipVerify: true}); err != nil {
		t.Fatalf("a client with no certificate: %v", err)
	}
	if n := <-peers; n != 0 {
		t.Errorf("herd-api saw %d client certificates from a client with none", n)
	}
	// A dialer expecting herd-router refuses herd-api's SVID.
	wrong, err := auth.ClientTLS(node, auth.HerdID(spiffeid.RequireTrustDomainFromString(td), "router"))
	if err != nil {
		t.Fatal(err)
	}
	if err := dial(wrong); err == nil {
		t.Error("a dialer expecting herd-router accepted herd-api's SVID")
	}
}
