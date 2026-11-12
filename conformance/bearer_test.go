package conformance_test

import (
	"crypto/x509"
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/apiserver"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/join"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
)

// TestRawTokenIsNoBearer (PLT-489 review 10): an authorization header without the "Bearer " scheme is refused, not
// read whole as the token: a raw JWT there is not a bearer.
func TestRawTokenIsNoBearer(t *testing.T) {
	e := conformance.Start(t)
	ctx := ctxT(t)
	tok := e.Token(t, map[string]auth.Role{e.Space: auth.RoleWrite}, false)
	// A caller with a client certificate (any chain: the shared-key authenticator does not read it) and a raw JWT.
	peer := auth.WithPeerChain(ctx, []*x509.Certificate{{}})
	rctx := metadata.NewIncomingContext(peer, metadata.Pairs("authorization", tok))
	_, err := e.Server.ListActors(rctx, &ateapi.ListActorsRequest{Atespace: e.Space})
	wantCode(t, "ListActors with a raw JWT as the authorization header", err, codes.Unauthenticated)
	if _, err := e.Client.ListActors(ctx, &ateapi.ListActorsRequest{Atespace: e.Space}); err != nil {
		t.Fatalf("ListActors with the same token as a bearer: %v", err)
	}

	js := &join.Server{Key: e.Key, Authn: auth.StaticKey{Key: e.Key}}
	jt, err := join.MintToken(e.Key, 3600e9)
	if err != nil {
		t.Fatal(err)
	}
	_, err = js.Join(metadata.NewIncomingContext(peer, metadata.Pairs("authorization", jt)), &join.Request{Worker: "w1"})
	wantCode(t, "a join with a raw join token", err, codes.Unauthenticated)
}

// TestDirectCheckIsCountedForHerdAPI (re-review 6c): a CheckActorWrite whose caller is not herd-router is counted for
// herd-api (by its authenticator), never for herd-router.
func TestDirectCheckIsCountedForHerdAPI(t *testing.T) {
	prev := conformance.Authn
	conformance.Authn = func(key []byte, m *auth.Meter) auth.Authenticator {
		return auth.Chain{BreakGlass: auth.StaticKey{Key: key}, Meter: m, Component: "herd-api"}
	}
	t.Cleanup(func() { conformance.Authn = prev })
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 1})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	before := e.Meter.Value(auth.MetricBreakGlassCalls, "component", "herd-api", "subject", "test-1")
	if _, err := apiserver.CallCheckActorWrite(ctx, e.Client.Conn(), e.Token(t, map[string]auth.Role{e.Space: auth.RoleWrite}, false), e.Space, "a1"); err != nil {
		t.Fatalf("CheckActorWrite: %v", err)
	}
	if n := e.Meter.Value(auth.MetricBreakGlassCalls, "component", "herd-router", "subject", "test-1"); n != 0 {
		t.Errorf("a direct CheckActorWrite counted %d for herd-router, want 0", n)
	}
	if n := e.Meter.Value(auth.MetricBreakGlassCalls, "component", "herd-api", "subject", "test-1") - before; n != 1 {
		t.Errorf("a direct CheckActorWrite counted %d for herd-api, want 1", n)
	}
}
