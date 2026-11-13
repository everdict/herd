package deploy

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/auth/localca"
	"github.com/spiffe/go-spiffe/v2/bundle/jwtbundle"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type rotatingSource struct {
	mu  sync.Mutex
	cur auth.Static
}

func (r *rotatingSource) get() auth.Static { r.mu.Lock(); defer r.mu.Unlock(); return r.cur }
func (r *rotatingSource) GetX509SVID() (*x509svid.SVID, error) {
	return r.get().GetX509SVID()
}
func (r *rotatingSource) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return r.get().GetX509BundleForTrustDomain(td)
}
func (r *rotatingSource) GetJWTBundleForTrustDomain(td spiffeid.TrustDomain) (*jwtbundle.Bundle, error) {
	return r.get().GetJWTBundleForTrustDomain(td)
}

// TestEgressConnectionOutlivesItsSVIDs (re-review 2): herd-egress's connection to herd-api, whose SVID lives a day,
// keeps answering over three lifetimes of herd-egress's own 3 s SVIDs: when herd-api drains the connection before the
// leaf ends, herd-egress's client dials again with its current SVID and the call is answered.
func TestEgressConnectionOutlivesItsSVIDs(t *testing.T) {
	prev := auth.DrainMargin
	auth.DrainMargin = time.Second
	t.Cleanup(func() { auth.DrainMargin = prev })
	ca, err := localca.New("herd.test")
	if err != nil {
		t.Fatal(err)
	}
	api, err := ca.Source("spiffe://herd.test/herd/api", localca.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pol := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(pol, []byte(`{"rules": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a := &API{Identity: Identity{Source: api}, Policy: pol, Store: "memory", Snapshots: t.TempDir()}
	go func() { _ = a.Run(ctx, ln) }()

	const ttl = 3 * time.Second
	eg := &rotatingSource{}
	issue := func() {
		s, err := ca.Source("spiffe://herd.test/herd/egress/w1", localca.Options{TTL: ttl, NotBefore: time.Now()})
		if err != nil {
			t.Error(err)
			return
		}
		eg.mu.Lock()
		eg.cur = s
		eg.mu.Unlock()
	}
	issue()
	go func() {
		tk := time.NewTicker(ttl / 4)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				issue()
			}
		}
	}()
	conn, err := redialing(ln.Addr().String(), eg, auth.HerdID(spiffeid.RequireTrustDomainFromString("herd.test"), "api"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var c grpc.ClientConnInterface = conn
	refused := 0
	for end := time.Now().Add(3 * ttl); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		cctx, ccancel := context.WithTimeout(ctx, 2*time.Second)
		err := c.Invoke(cctx, "/substrate.ateapi.v1.Control/GetActorEgressPolicy",
			&ateapi.GetActorEgressPolicyRequest{Actor: &ateapi.ObjectRef{Atespace: "a", Name: "x"}}, new(ateapi.EgressPolicy))
		ccancel()
		if code := status.Code(err); code == codes.Unauthenticated || code == codes.Unavailable {
			refused++
			t.Logf("refused: %v", err)
		}
	}
	if refused > 0 {
		t.Errorf("%d calls of herd-egress's connection were refused as its SVIDs rotated", refused)
	}
}

// TestRedialRetriesReadsOnly (final review): redialConn tries a call again on a new connection only for a read; a
// write is tried once. herd-egress and the herdlet's in-process egress use it for GetActorEgressPolicy alone.
func TestRedialRetriesReadsOnly(t *testing.T) {
	g := grpc.NewServer()
	calls := map[string]int{}
	var mu sync.Mutex
	g.RegisterService(&grpc.ServiceDesc{ServiceName: "substrate.ateapi.v1.Control", HandlerType: (*any)(nil), Methods: []grpc.MethodDesc{
		{MethodName: "GetActorEgressPolicy", Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			mu.Lock()
			calls["read"]++
			mu.Unlock()
			return nil, status.Error(codes.Unavailable, "draining")
		}},
		{MethodName: "DeleteActorEgressPolicy", Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			mu.Lock()
			calls["write"]++
			mu.Unlock()
			return nil, status.Error(codes.Unavailable, "draining")
		}},
	}}, struct{}{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	defer g.Stop()
	r := &redialConn{dialFn: func() (*grpc.ClientConn, error) {
		return grpc.NewClient("passthrough:///"+ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	}}
	if r.cc, err = r.dialFn(); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	_ = r.Invoke(ctx, "/substrate.ateapi.v1.Control/GetActorEgressPolicy", &ateapi.GetActorEgressPolicyRequest{}, new(ateapi.EgressPolicy))
	_ = r.Invoke(ctx, "/substrate.ateapi.v1.Control/DeleteActorEgressPolicy", &ateapi.DeleteActorEgressPolicyRequest{}, new(ateapi.EgressPolicy))
	if calls["read"] != 2 || calls["write"] != 1 {
		t.Errorf("a read was tried %d times and a write %d; want 2 and 1", calls["read"], calls["write"])
	}
	for m := range redialReads {
		if !strings.Contains(m, "/Get") && !strings.Contains(m, "/List") {
			t.Errorf("redialReads lists %s, which is no read", m)
		}
	}
}
