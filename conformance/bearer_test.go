package conformance_test

import (
	"crypto/x509"
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
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
