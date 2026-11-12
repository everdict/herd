package deploy_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/auth/spiffetest"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/deploy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const (
	spiffeTD  = "herd.test"
	apiID     = "spiffe://herd.test/herd/api"
	runnerID1 = "spiffe://herd.test/everdict/ws/acme/runner/r1"
)

// mtlsClient dials herd-api's mTLS listener presenting cert, trusting herd-api's SVID by the root alone (an SVID
// carries a URI SAN and no DNS name).
func mtlsClient(t *testing.T, addr string, root *x509.Certificate, cert *tls.Certificate) *grpc.ClientConn {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(root)
	cfg := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("herd-api presented no certificate")
			}
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			return err
		}}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	cc, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	return cc
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// TestAPIServesSPIFFE (PLT-489, ADR 0001 A-60): herd-api with -spiffe-* and -tls-listen authenticates a caller with no
// bearer by its X.509-SVID over mTLS and a JWT-SVID bearer on its cleartext listener, each mapped by the policy;
// refuses a client certificate outside the bundle; still accepts an HS256 bearer, and serves the break-glass count
// at /metrics.
func TestAPIServesSPIFFE(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "jwt.key")
	key, err := deploy.EnsureKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ca := spiffetest.New(t, spiffeTD)
	cfg := ca.Config(t, dir, apiID, `{"rules": [{"id": "`+runnerID1+`", "spaces": {"team-a": "write"}}]}`)
	certFile, keyPEM := spiffetest.WriteKeyPair(t, dir, "herd-api", ca.TLSCertificate(t, apiID))
	ln, tln, mln := listen(t), listen(t), listen(t)
	a := &deploy.API{KeyFile: keyFile, Store: "bolt:" + filepath.Join(dir, "herd.db"), Snapshots: filepath.Join(dir, "snapshots"),
		CallTimeout: 5 * time.Second, Reconcile: time.Second,
		Identity:    deploy.Identity{TrustDomain: cfg.TrustDomain, Bundle: cfg.BundleFile, Audience: cfg.Audience, Policy: cfg.PolicyFile},
		TLSListener: tln, TLSCert: certFile, TLSKey: keyPEM, MetricsListener: mln}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("herd-api: %v", err)
		}
	})
	cctx, ccancel := context.WithTimeout(ctx, 20*time.Second)
	defer ccancel()
	tpl := func(name string) *ateapi.CreateActorTemplateRequest {
		return &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
			Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: name}, Containers: []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
			SnapshotConfig: &ateapi.SnapshotConfig{GoldenSnapshotConfig: &ateapi.GoldenSnapshotConfig{Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED}}}}
	}
	const control = "/substrate.ateapi.v1.Control/"

	// mTLS, no bearer: the X.509-SVID is the caller.
	runner := ca.TLSCertificate(t, runnerID1)
	cc := mtlsClient(t, tln.Addr().String(), ca.Cert, &runner)
	var deadline = time.Now().Add(10 * time.Second)
	for {
		err = cc.Invoke(cctx, control+"CreateActorTemplate", tpl("by-x509"), new(ateapi.ActorTemplate))
		if err == nil || status.Code(err) != codes.Unavailable || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("CreateActorTemplate over mTLS as %s: %v", runnerID1, err)
	}
	err = cc.Invoke(cctx, control+"ListActors", &ateapi.ListActorsRequest{Atespace: "team-b"}, new(ateapi.ListActorsResponse))
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), runnerID1) {
		t.Errorf("ListActors in a space the policy does not grant, over mTLS: %v; want PermissionDenied naming %s", err, runnerID1)
	}

	// A client certificate of another issuer is refused with its reason.
	stranger := spiffetest.New(t, spiffeTD).TLSCertificate(t, runnerID1)
	err = mtlsClient(t, tln.Addr().String(), ca.Cert, &stranger).Invoke(cctx, control+"ListActors", &ateapi.ListActorsRequest{Atespace: space}, new(ateapi.ListActorsResponse))
	if status.Code(err) != codes.Unauthenticated || !strings.Contains(err.Error(), "does not chain to the trust bundle") {
		t.Errorf("a client certificate outside the bundle: %v; want Unauthenticated naming the bundle", err)
	}
	// No client certificate at all: the mTLS listener does not complete the handshake.
	err = mtlsClient(t, tln.Addr().String(), ca.Cert, nil).Invoke(cctx, control+"ListActors", &ateapi.ListActorsRequest{Atespace: space}, new(ateapi.ListActorsResponse))
	if err == nil {
		t.Error("a call with no client certificate was answered on the mTLS listener")
	}

	// A JWT-SVID bearer on the cleartext listener.
	jc := conformance.NewClient(ln.Addr().String(), ca.JWTSVID(t, runnerID1, []string{apiID}, time.Now().Add(time.Hour)))
	defer jc.Close()
	if _, err := jc.CreateActorTemplate(cctx, tpl("by-jwt")); err != nil {
		t.Fatalf("CreateActorTemplate with a JWT-SVID bearer: %v", err)
	}

	// HS256 break-glass, counted.
	tok, err := auth.Sign(key, auth.Claims{Subject: "ops", ExpiresAt: time.Now().Add(time.Hour).Unix(), Spaces: map[string]auth.Role{space: auth.RoleRead}})
	if err != nil {
		t.Fatal(err)
	}
	hc := conformance.NewClient(ln.Addr().String(), tok)
	defer hc.Close()
	if _, err := hc.ListActors(cctx, &ateapi.ListActorsRequest{Atespace: space}); err != nil {
		t.Fatalf("ListActors with an HS256 bearer: %v", err)
	}
	resp, err := http.Get("http://" + mln.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if want := auth.MetricBreakGlassCalls + `{subject="ops"} 1`; !strings.Contains(string(body), want) {
		t.Errorf("/metrics:\n%s\ndoes not carry %q", body, want)
	}
}
