package router_test

import (
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/apiserver"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/router"
	"github.com/everdict/herd/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// TestRequestsAreTheCreators (PLT-489, ADR 0001 A-60): a request through the router to an actor (an exec, a file
// read, a resume on request) is a write on it, so it is its creator's or an operator's. Another caller holding write
// in the space is refused permission-denied before anything reaches the node; an actor with no creator recorded is
// reached by break-glass under an allowance the router counts, once per request.
func TestRequestsAreTheCreators(t *testing.T) {
	s := startStack(t, stackOpts{budget: time.Second})
	ctx := ctxT(t, 30*time.Second)
	s.createActor(space1, "a1")

	before, dials := s.tun.Opened(), s.dials.Load()
	intruder := s.token(map[string]auth.Role{space1: auth.RoleWrite}, false, "intruder")
	reason, err := listDir(ctx, s.client(space1+"/a1", intruder))
	wantCode(t, "another writer of space-1 reaching the creator's actor", err, codes.PermissionDenied)
	if reason != string(router.ReasonPermissionDenied) {
		t.Errorf("herd-reason = %q, want %q", reason, router.ReasonPermissionDenied)
	}
	if s.tun.Opened() != before || s.dials.Load() != dials {
		t.Errorf("a refused request reached the tunnel (opened %d → %d, dials %d → %d)", before, s.tun.Opened(), dials, s.dials.Load())
	}

	for who, tok := range map[string]string{
		"the creator": s.clientToken(),
		"an operator": s.token(map[string]auth.Role{space1: auth.RoleOperator}, false, "ops"),
	} {
		if _, err := listDir(ctx, s.client(space1+"/a1", tok)); err != nil {
			t.Errorf("a request by %s: %v", who, err)
		}
	}

	// An actor created before PLT-489 has no creator recorded.
	s.createActor(space1, "legacy")
	k := store.Key{Kind: store.KindActor, Space: space1, Name: "legacy"}
	r, err := s.records.Get(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	a := &ateapi.Actor{}
	if err := proto.Unmarshal(r.Data, a); err != nil {
		t.Fatal(err)
	}
	a.Status.Creator = ""
	if r.Data, err = proto.Marshal(a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.records.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := listDir(ctx, s.client(space1+"/legacy", intruder)); err != nil {
		t.Fatalf("a break-glass request to an actor with no creator: %v", err)
	}
	if n := s.meter.Value(auth.MetricUnownedActorWrites, "allowance", string(auth.AllowUnownedBreakGlass), "caller", "hs256:intruder"); n != 1 {
		t.Errorf("%s{allowance=%s,caller=hs256:intruder} = %d after one request, want 1", auth.MetricUnownedActorWrites, auth.AllowUnownedBreakGlass, n)
	}
}

// TestRouterAsksHerdAPI (review 7): herd-api is the one decider. The router keeps no policy: each request is decided
// by herd-api's CheckActorWrite with the client's own bearer, and herd-api's refusal is the router's answer.
func TestRouterAsksHerdAPI(t *testing.T) {
	if router.CheckActorWriteMethod != apiserver.CheckActorWriteMethod {
		t.Fatalf("the router calls %s, herd-api serves %s", router.CheckActorWriteMethod, apiserver.CheckActorWriteMethod)
	}
	s := startStack(t, stackOpts{budget: time.Second})
	ctx := ctxT(t, 30*time.Second)
	s.createActor(space1, "a1")
	before := s.checks.Load()
	if _, err := listDir(ctx, s.client(space1+"/a1", s.clientToken())); err != nil {
		t.Fatalf("the creator's request: %v", err)
	}
	if n := s.checks.Load() - before; n != 1 {
		t.Errorf("one request asked herd-api %d times, want once", n)
	}
	intruder := s.token(map[string]auth.Role{space1: auth.RoleWrite}, false, "intruder")
	reason, err := listDir(ctx, s.client(space1+"/a1", intruder))
	wantCode(t, "another writer's request", err, codes.PermissionDenied)
	if reason != string(router.ReasonPermissionDenied) {
		t.Errorf("herd-reason %q, want %q", reason, router.ReasonPermissionDenied)
	}
}

// TestParkedRequestDecidesAgainForANewActor (review 6): the decision is for one actor uid. A request whose actor is
// deleted and created again under the same name by another caller, between the decision and the connection, is
// decided again for the new actor, and refused.
func TestParkedRequestDecidesAgainForANewActor(t *testing.T) {
	s := startStack(t, stackOpts{budget: 2 * time.Second})
	ctx := ctxT(t, 30*time.Second)
	s.createActor(space1, "a1")
	intruderTok := s.token(map[string]auth.Role{space1: auth.RoleWrite}, false, "intruder")
	intruder := conformance.NewClient(s.apiAddr, intruderTok)
	defer intruder.Close()
	var once sync.Once
	h := func() {
		once.Do(func() {
			if _, err := s.api.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: &ateapi.ObjectRef{Atespace: space1, Name: "a1"}}); err != nil {
				t.Errorf("DeleteActor: %v", err)
			}
			if _, err := intruder.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
				Metadata: &ateapi.ResourceMetadata{Atespace: space1, Name: "a1"}, ActorTemplate: &ateapi.ObjectRef{Atespace: space1, Name: "tpl"}}}); err != nil {
				t.Errorf("CreateActor by the intruder: %v", err)
			}
		})
	}
	s.afterCheck.Store(&h)
	_, err := listDir(ctx, s.client(space1+"/a1", s.clientToken()))
	wantCode(t, "the client's request reaching the intruder's a1", err, codes.PermissionDenied)
}

// TestBreakGlassCountedOncePerRequest (review, small items): a break-glass bearer's request through the router is
// counted once, for herd-router (the component the client called), however many times the parked request reads the
// actor again.
func TestBreakGlassCountedOncePerRequest(t *testing.T) {
	s := startStack(t, stackOpts{budget: 5 * time.Second})
	ctx := ctxT(t, 30*time.Second)
	s.createActor(space1, "a1")
	if err := s.suspend(space1, "a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := listDir(ctx, s.client(space1+"/a1", s.clientToken())); err != nil {
		t.Fatalf("a request that resumes a1: %v", err)
	}
	if n := s.meter.Value(auth.MetricBreakGlassCalls, "component", "herd-router", "subject", "client"); n != 1 {
		t.Errorf("%s{component=herd-router,subject=client} = %d after one request, want 1", auth.MetricBreakGlassCalls, n)
	}
}

// spliceOpenFor opens a CONNECT splice to a1 with tok and answers whether it is still open after d.
func spliceOpenFor(t *testing.T, s *stack, tok string, d time.Duration) bool {
	t.Helper()
	conn := connect(t, s, space1+"/a1", tok, "a1.space-1:8080")
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(d))
	// The guest's HTTP/2 server speaks first; read until the splice ends or d passes.
	buf := make([]byte, 4096)
	var err error
	for err == nil {
		_, err = conn.Read(buf)
	}
	ne, ok := err.(interface{ Timeout() bool })
	return ok && ne.Timeout()
}

// TestStreamOutlivesItsJWT (re-review 4): a stream the router carries is authorized at its open; a bearer whose exp
// passes while it runs (a JWT-SVID lives 5 minutes by default) does not cut an exec or an attach.
func TestStreamOutlivesItsJWT(t *testing.T) {
	s := startStack(t, stackOpts{budget: time.Second})
	s.createActor(space1, "a1")
	tok, err := auth.Sign(s.key, auth.Claims{Subject: "client", ExpiresAt: time.Now().Add(2 * time.Second).Unix(),
		Spaces: map[string]auth.Role{space1: auth.RoleWrite}})
	if err != nil {
		t.Fatal(err)
	}
	if !spliceOpenFor(t, s, tok, 5*time.Second) {
		t.Error("a splice was cut when its bearer's exp passed (2 s)")
	}
}

// TestStreamEndsAtBreakGlassEnd (review 2): a stream an HS256 break-glass bearer opened ends at the allowance's end.
func TestStreamEndsAtBreakGlassEnd(t *testing.T) {
	s := startStack(t, stackOpts{budget: time.Second, breakGlassUntil: time.Now().Add(3 * time.Second)})
	s.createActor(space1, "a1")
	if spliceOpenFor(t, s, s.clientToken(), 6*time.Second) {
		t.Error("an HS256 splice is still open 3 s after the break-glass allowance ended")
	}
}
