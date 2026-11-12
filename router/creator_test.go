package router_test

import (
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
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
