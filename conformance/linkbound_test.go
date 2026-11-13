package conformance_test

import (
	"encoding/json"
	"testing"

	"time"

	"github.com/everdict/herd/link"
	"github.com/everdict/herd/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/conformance"
)

// TestLinkEndsWithItsCredential (PLT-489 review 2): a herdlet link opened with an HS256 credential closes at the
// break-glass allowance's end, and the herdlet cannot link again after it: a stream lives no longer than the
// credential that opened it.
func TestLinkEndsWithItsCredential(t *testing.T) {
	until := time.Now().Add(2 * time.Second)
	prev := conformance.Authn
	conformance.Authn = func(key []byte, m *auth.Meter) auth.Authenticator {
		return auth.Chain{BreakGlass: auth.StaticKey{Key: key}, BreakGlassUntil: until, Meter: m}
	}
	t.Cleanup(func() { conformance.Authn = prev })
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 1})
	connected := func() bool {
		for _, w := range e.Hub.Workers() {
			if w.Worker == "h1" && w.Connected {
				return true
			}
		}
		return false
	}
	if !connected() {
		t.Fatal("h1 is not linked before the allowance ends")
	}
	deadline := time.Now().Add(5 * time.Second)
	for connected() {
		if time.Now().After(deadline) {
			t.Fatalf("h1's HS256 link is still up %s after the break-glass allowance ended", time.Since(until).Round(time.Second))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestHS256CannotTakeAnSVIDWorker (re-review 5): a worker a node SVID registered is that node's: an HS256 herdlet
// credential naming it is refused its link.
func TestHS256CannotTakeAnSVIDWorker(t *testing.T) {
	e := conformance.Start(t)
	ctx := ctxT(t)
	st, _ := json.Marshal(map[string]string{"identity": "spiffe://herd.test/herd/node/h1"})
	if _, err := e.Store.Create(ctx, store.Record{Key: store.Key{Kind: store.KindWorker, Name: "h1"}, State: st}); err != nil {
		t.Fatal(err)
	}
	c := conformance.NewClient(e.Addr, e.TokenFor(t, "h1", nil, true))
	defer c.Close()
	cs, err := c.Conn().NewStream(ctx, &link.StreamDesc, link.LinkMethod, grpc.CallContentSubtype(link.CodecName))
	if err == nil {
		_ = cs.SendMsg(map[string]any{"register": map[string]any{"worker": "h1"}})
		var m map[string]any
		err = cs.RecvMsg(&m)
	}
	wantCode(t, "an HS256 herdlet linking as h1, which a node SVID registered", err, codes.PermissionDenied)
}
