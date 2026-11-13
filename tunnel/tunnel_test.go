package tunnel_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestFrameCodec(t *testing.T) {
	c := tunnel.FrameCodec{}
	for _, f := range []tunnel.Frame{
		{Kind: tunnel.FrameData, Data: []byte("bytes\x00\xff")},
		{Kind: tunnel.FrameData, Data: []byte{}},
		{Kind: tunnel.FrameCut, Reason: tunnel.ReasonSuspended, Detail: "snapshot for suspend"},
		{Kind: tunnel.FrameClose},
	} {
		b, err := c.Marshal(&f)
		if err != nil {
			t.Fatal(err)
		}
		var g tunnel.Frame
		if err := c.Unmarshal(b, &g); err != nil {
			t.Fatal(err)
		}
		if g.Kind != f.Kind || string(g.Data) != string(f.Data) || g.Reason != f.Reason || g.Detail != f.Detail {
			t.Errorf("round trip %+v → %+v", f, g)
		}
	}
	for _, bad := range [][]byte{nil, {9}, {byte(tunnel.FrameCut), 50, 'x'}} {
		if err := c.Unmarshal(bad, new(tunnel.Frame)); err == nil {
			t.Errorf("Unmarshal(%v) accepted a malformed frame", bad)
		}
	}
	if _, err := c.Marshal(proto.Message(nil)); err == nil {
		t.Error("Marshal of a non-frame succeeded")
	}
}

// TestOnlyItsOwnHerdletOpensATunnel: a client credential cannot open a tunnel, a herdlet credential opens only the
// tunnel of the worker it names, and Dial to a worker with no tunnel is node-unreachable at once.
func TestOnlyItsOwnHerdletOpensATunnel(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	srv := &tunnel.Server{Authn: auth.StaticKey{Key: key}, WorkerIdentity: func(_ context.Context, w string) (string, error) {
		if w == "svid-node" {
			return "spiffe://herd.test/herd/node/svid-node", nil
		}
		return "", nil
	}}
	g := grpc.NewServer()
	srv.Register(g)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(l) }()
	defer g.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hello := func(sub string, herdlet bool, worker string) error {
		tok, err := auth.Sign(key, auth.Claims{Subject: sub, ExpiresAt: time.Now().Add(time.Hour).Unix(), Herdlet: herdlet,
			Spaces: map[string]auth.Role{auth.AllSpaces: auth.RoleWrite}})
		if err != nil {
			t.Fatal(err)
		}
		cc, err := grpc.NewClient("passthrough:///"+l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithPerRPCCredentials(auth.Bearer(tok)))
		if err != nil {
			t.Fatal(err)
		}
		defer cc.Close()
		cs, err := cc.NewStream(ctx, &tunnel.ControlStreamDesc, tunnel.ControlMethod, grpc.CallContentSubtype(tunnel.ControlCodec))
		if err != nil {
			return err
		}
		b, _ := json.Marshal(tunnel.Up{Hello: &tunnel.Hello{Worker: worker}})
		if err := cs.SendMsg(json.RawMessage(b)); err != nil {
			return err
		}
		var m json.RawMessage
		return cs.RecvMsg(&m)
	}
	if err := hello("everdict", false, "h1"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("client credential opening a tunnel: %v", err)
	}
	if err := hello("h2", true, "h1"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("herdlet h2 opening h1's tunnel: %v", err)
	}
	if err := hello("h1", true, "h1"); err != nil {
		t.Errorf("herdlet h1 opening its tunnel: %v", err)
	}
	// A worker a node SVID registered is that node's (re-review 5).
	if err := hello("svid-node", true, "svid-node"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("an HS256 tunnel for a worker a node SVID registered: %v, want PermissionDenied", err)
	}
	_, err = srv.Dial(ctx, "h9", "vm-1", 8080)
	if te, ok := err.(*tunnel.Error); !ok || te.Reason != tunnel.ReasonNodeUnreachable {
		t.Errorf("Dial to a worker with no tunnel: %v", err)
	}
}

// fakeAuthn answers "svid:<worker>" as that worker's node SVID and "hs:<worker>" as its HS256 herdlet credential.
type fakeAuthn struct{}

func (fakeAuthn) Authenticate(_ context.Context, tok string) (auth.Principal, error) {
	switch {
	case strings.HasPrefix(tok, "svid:"):
		w := strings.TrimPrefix(tok, "svid:")
		return auth.Principal{Subject: "spiffe://herd.test/herd/node/" + w, Worker: w, Herdlet: true, Scheme: auth.SchemeX509SVID,
			Expires: time.Now().Add(time.Hour)}, nil
	case strings.HasPrefix(tok, "hs:"):
		return auth.Principal{Subject: strings.TrimPrefix(tok, "hs:"), Herdlet: true}, nil
	}
	return auth.Principal{}, auth.ErrUnauthenticated
}

// TestHS256TunnelCannotReplaceAnSVIDOne (R3): with the swap, under the lock, an HS256 tunnel cannot replace the
// tunnel a node SVID holds, even when herd-api's record does not name the SVID yet (WorkerIdentity answers none).
func TestHS256TunnelCannotReplaceAnSVIDOne(t *testing.T) {
	srv := &tunnel.Server{Authn: fakeAuthn{}, WorkerIdentity: func(context.Context, string) (string, error) { return "", nil }}
	g := grpc.NewServer()
	srv.Register(g)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(l) }()
	defer g.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	open := func(tok string) (grpc.ClientStream, error) {
		cc, err := grpc.NewClient("passthrough:///"+l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithPerRPCCredentials(auth.Bearer(tok)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cc.Close() })
		cs, err := cc.NewStream(ctx, &tunnel.ControlStreamDesc, tunnel.ControlMethod, grpc.CallContentSubtype(tunnel.ControlCodec))
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(tunnel.Up{Hello: &tunnel.Hello{Worker: "w1"}})
		if err := cs.SendMsg(json.RawMessage(b)); err != nil {
			return nil, err
		}
		var m json.RawMessage
		return cs, cs.RecvMsg(&m)
	}
	if _, err := open("svid:w1"); err != nil {
		t.Fatalf("w1's SVID tunnel: %v", err)
	}
	if _, err := open("hs:w1"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("an HS256 tunnel replacing w1's SVID tunnel: %v, want PermissionDenied", err)
	}
}
