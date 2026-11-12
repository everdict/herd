package conformance_test

import (
	"context"
	"strings"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/auth/spiffetest"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	spiffeTD       = "herd.test"
	spiffeAudience = "spiffe://herd.test/herd/api"
	runner1        = "spiffe://herd.test/everdict/ws/acme/runner/r1"
	runner2        = "spiffe://herd.test/everdict/ws/acme/runner/r2"
	operatorID     = "spiffe://herd.test/herd/operator/alice"
)

// spiffeEnv starts herd-api authenticating SPIFFE SVIDs beside HS256 break-glass (auth.Chain), with a policy that
// lets two runners write in the harness's space; alice, an operator, holds operator everywhere by the built-in
// convention.
func spiffeEnv(t *testing.T, herdlets ...conformance.HerdletSpec) (*conformance.Env, *spiffetest.CA) {
	t.Helper()
	ca := spiffetest.New(t, spiffeTD)
	cfg := ca.Config(t, t.TempDir(), spiffeAudience, `{"rules": [
	  {"id": "`+runner1+`", "spaces": {"space-1": "write"}},
	  {"id": "`+runner2+`", "spaces": {"space-1": "write"}}
	]}`)
	sv, err := auth.NewSPIFFE(cfg)
	if err != nil {
		t.Fatal(err)
	}
	prev := conformance.Authn
	conformance.Authn = func(key []byte, m *auth.Meter) auth.Authenticator {
		return auth.Chain{SVID: sv, BreakGlass: auth.StaticKey{Key: key}, Meter: m}
	}
	t.Cleanup(func() { conformance.Authn = prev })
	return conformance.Start(t, herdlets...), ca
}

func svidClient(t *testing.T, e *conformance.Env, ca *spiffetest.CA, id string) *conformance.Client {
	t.Helper()
	c := conformance.NewClient(e.Addr, ca.JWTSVID(t, id, []string{spiffeAudience}, time.Now().Add(time.Hour)))
	t.Cleanup(c.Close)
	return c
}

// actorWrites are the writes on an existing actor (W4), each as one call by c on actor name.
func actorWrites(e *conformance.Env, name string) map[string]func(context.Context, *conformance.Client) error {
	ref := func() *ateapi.ObjectRef { return oref(e, name) }
	rule := []*ateapi.EgressRule{{Https: &ateapi.HttpsRule{Hostnames: []string{"api.example.com"}}}}
	return map[string]func(context.Context, *conformance.Client) error{
		"SuspendActor": func(ctx context.Context, c *conformance.Client) error {
			_, err := c.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: ref()})
			return err
		},
		"ResumeActor": func(ctx context.Context, c *conformance.Client) error {
			_, err := c.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref()})
			return err
		},
		"DeleteActor": func(ctx context.Context, c *conformance.Client) error {
			_, err := c.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref()})
			return err
		},
		"UpdateActor": func(ctx context.Context, c *conformance.Client) error {
			a, err := e.Client.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref()})
			if err != nil {
				return err
			}
			_, err = c.UpdateActor(ctx, &ateapi.UpdateActorRequest{Actor: &ateapi.Actor{Metadata: a.GetMetadata(),
				WorkerSelector: &ateapi.Selector{MatchLabels: map[string]string{"moved": "by-" + name}}}})
			return err
		},
		"CreateTag (fork from)": func(ctx context.Context, c *conformance.Client) error {
			_, err := c.CreateTag(ctx, &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
				Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "tag-of-" + name}, SourceActor: ref()}})
			return err
		},
		"CreateActorEgressPolicy": func(ctx context.Context, c *conformance.Client) error {
			_, err := c.CreateActorEgressPolicy(ctx, &ateapi.CreateActorEgressPolicyRequest{Actor: ref(), EgressPolicy: &ateapi.EgressPolicy{Rules: rule}})
			return err
		},
		"UpdateActorEgressPolicy": func(ctx context.Context, c *conformance.Client) error {
			_, err := c.UpdateActorEgressPolicy(ctx, &ateapi.UpdateActorEgressPolicyRequest{Actor: ref(), EgressPolicy: &ateapi.EgressPolicy{Rules: rule}})
			return err
		},
		"DeleteActorEgressPolicy": func(ctx context.Context, c *conformance.Client) error {
			_, err := c.DeleteActorEgressPolicy(ctx, &ateapi.DeleteActorEgressPolicyRequest{Actor: ref()})
			return err
		},
	}
}

func wantDenied(t *testing.T, what string, err error, creator string) {
	t.Helper()
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("%s: %v, want PermissionDenied", what, err)
		return
	}
	if !strings.Contains(err.Error(), creator) {
		t.Errorf("%s: refused with %q, which does not name the creator %s", what, err, creator)
	}
}

// TestWritesAreTheCreators (PLT-489, A-60, spec W4): every write on an actor (suspend, resume, delete, update, a tag
// of it, its egress policy) by another caller holding write in the space, SVID or HS256, is refused PermissionDenied
// naming the creator, and changes nothing; the creator's and an operator's go through, uncounted. Reads stay the
// space's.
func TestWritesAreTheCreators(t *testing.T) {
	e, ca := spiffeEnv(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	r1, r2, op := svidClient(t, e, ca, runner1), svidClient(t, e, ca, runner2), svidClient(t, e, ca, operatorID)
	a, err := r1.CreateActor(ctx, actorReq(e, "a1", "tpl", nil))
	if err != nil {
		t.Fatalf("CreateActor by %s: %v", runner1, err)
	}
	if a.GetStatus().GetCreator() != runner1 {
		t.Fatalf("status.creator %q, want the JWT-SVID's %s", a.GetStatus().GetCreator(), runner1)
	}
	// The egress update and delete need a policy to act on; the creator sets it.
	if _, err := r1.CreateActorEgressPolicy(ctx, &ateapi.CreateActorEgressPolicyRequest{Actor: oref(e, "a1"),
		EgressPolicy: &ateapi.EgressPolicy{Rules: []*ateapi.EgressRule{{Https: &ateapi.HttpsRule{Hostnames: []string{"a.example.com"}}}}}}); err != nil {
		t.Fatalf("CreateActorEgressPolicy by the creator: %v", err)
	}
	before, _ := getActor(t, e, "a1")
	for name, write := range actorWrites(e, "a1") {
		if name == "CreateActorEgressPolicy" {
			// The policy exists: another caller's create must be refused for who it is, not as AlreadyExists.
			_, _ = r1.DeleteActorEgressPolicy(ctx, &ateapi.DeleteActorEgressPolicyRequest{Actor: oref(e, "a1")})
		}
		for who, c := range map[string]*conformance.Client{runner2: r2, "hs256:test-1": e.Client} {
			wantDenied(t, name+" by "+who, write(ctx, c), runner1)
		}
		if name == "CreateActorEgressPolicy" {
			if _, err := r1.CreateActorEgressPolicy(ctx, &ateapi.CreateActorEgressPolicyRequest{Actor: oref(e, "a1"),
				EgressPolicy: &ateapi.EgressPolicy{Rules: []*ateapi.EgressRule{{Https: &ateapi.HttpsRule{Hostnames: []string{"a.example.com"}}}}}}); err != nil {
				t.Fatalf("CreateActorEgressPolicy by the creator: %v", err)
			}
		}
	}
	after, _ := getActor(t, e, "a1")
	if after.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING || after.GetMetadata().GetVersion() != before.GetMetadata().GetVersion() {
		t.Fatalf("after the refused writes the actor is %v at version %d, want RUNNING at %d", after.GetStatus().GetState(),
			after.GetMetadata().GetVersion(), before.GetMetadata().GetVersion())
	}
	if _, err := e.Client.GetTag(ctx, &ateapi.GetTagRequest{Tag: oref(e, "tag-of-a1")}); status.Code(err) != codes.NotFound {
		t.Errorf("a refused CreateTag left a tag: %v", err)
	}
	if _, err := r2.GetActor(ctx, &ateapi.GetActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Errorf("GetActor by another writer of the space: %v (reads stay the space's)", err)
	}

	if _, err := op.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("SuspendActor by an operator: %v", err)
	}
	if _, err := r1.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("ResumeActor by the creator: %v", err)
	}
	if _, err := r1.CreateTag(ctx, &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "t1"}, SourceActor: oref(e, "a1")}}); err != nil {
		t.Fatalf("CreateTag of its actor by the creator: %v", err)
	}
	if _, err := r1.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("DeleteActor by the creator: %v", err)
	}
	for _, allowance := range []auth.Allowance{auth.AllowUnownedOperator, auth.AllowUnownedBreakGlass} {
		for _, caller := range []string{operatorID, runner1, "hs256:test-1"} {
			if n := e.Meter.Value(auth.MetricUnownedActorWrites, "allowance", string(allowance), "caller", caller); n != 0 {
				t.Errorf("%s{allowance=%s,caller=%s} = %d; no actor here lacks a creator", auth.MetricUnownedActorWrites, allowance, caller, n)
			}
		}
	}
}

// forgetCreator rewrites an actor's record as herd-api before PLT-489 wrote it: no creator.
func forgetCreator(t *testing.T, e *conformance.Env, name string) {
	t.Helper()
	ctx := ctxT(t)
	k := store.Key{Kind: store.KindActor, Space: e.Space, Name: name}
	r, err := e.Store.Get(ctx, k)
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
	if _, err := e.Store.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
}

// TestUnownedActorWrites (A-60): an actor with no creator recorded (created before PLT-489) is writable only by an
// operator or by break-glass, and each such write is counted by allowance and caller; an SVID writer of the space is
// refused.
func TestUnownedActorWrites(t *testing.T) {
	e, ca := spiffeEnv(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	r1, op := svidClient(t, e, ca, runner1), svidClient(t, e, ca, operatorID)
	mkActor(t, e, "legacy", "tpl", nil)
	forgetCreator(t, e, "legacy")

	_, err := r1.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "legacy")})
	wantDenied(t, "SuspendActor of an unowned actor by an SVID writer", err, "no creator")
	if _, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "legacy")}); err != nil {
		t.Fatalf("SuspendActor of an unowned actor by break-glass: %v", err)
	}
	if _, err := op.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "legacy")}); err != nil {
		t.Fatalf("ResumeActor of an unowned actor by an operator: %v", err)
	}
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "legacy")}); err != nil {
		t.Fatalf("DeleteActor of an unowned actor by break-glass: %v", err)
	}
	for _, c := range []struct {
		allowance auth.Allowance
		caller    string
		want      uint64
	}{
		{auth.AllowUnownedBreakGlass, "hs256:test-1", 2},
		{auth.AllowUnownedOperator, operatorID, 1},
		{auth.AllowUnownedBreakGlass, runner1, 0},
	} {
		if n := e.Meter.Value(auth.MetricUnownedActorWrites, "allowance", string(c.allowance), "caller", c.caller); n != c.want {
			t.Errorf("%s{allowance=%s,caller=%s} = %d, want %d", auth.MetricUnownedActorWrites, c.allowance, c.caller, n, c.want)
		}
	}
}

// TestEgressPolicyIsTheActorsByUID (review 5): an egress policy belongs to the actor uid it was written for. A policy
// record of an earlier actor of the same name (what a write racing that actor's delete leaves) is not the new actor's:
// GetActorEgressPolicy answers NotFound, and the new actor's own create replaces it.
func TestEgressPolicyIsTheActorsByUID(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	rule := []*ateapi.EgressRule{{Https: &ateapi.HttpsRule{Hostnames: []string{"old.example.com"}}}}
	if _, err := e.Client.CreateActorEgressPolicy(ctx, &ateapi.CreateActorEgressPolicyRequest{Actor: oref(e, "a1"), EgressPolicy: &ateapi.EgressPolicy{Rules: rule}}); err != nil {
		t.Fatal(err)
	}
	k := store.Key{Kind: store.KindEgressPolicy, Space: e.Space, Name: "a1"}
	left, err := e.Store.Get(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatal(err)
	}
	// The racing write lands after the delete removed the policy.
	left.UID, left.Version = "", 0
	if _, err := e.Store.Create(ctx, left); err != nil {
		t.Fatal(err)
	}
	mkActor(t, e, "a1", "tpl", nil)
	_, err = e.Client.GetActorEgressPolicy(ctx, &ateapi.GetActorEgressPolicyRequest{Actor: oref(e, "a1")})
	wantCode(t, "the new a1's egress policy, with the earlier a1's record left", err, codes.NotFound)
	if _, err := e.Client.CreateActorEgressPolicy(ctx, &ateapi.CreateActorEgressPolicyRequest{Actor: oref(e, "a1"),
		EgressPolicy: &ateapi.EgressPolicy{Rules: []*ateapi.EgressRule{{Https: &ateapi.HttpsRule{Hostnames: []string{"new.example.com"}}}}}}); err != nil {
		t.Fatalf("the new a1's own create over the earlier a1's record: %v", err)
	}
	p, err := e.Client.GetActorEgressPolicy(ctx, &ateapi.GetActorEgressPolicyRequest{Actor: oref(e, "a1")})
	if err != nil || p.GetRules()[0].GetHttps().GetHostnames()[0] != "new.example.com" {
		t.Errorf("the new a1's egress policy: %v, %v; want its own", p, err)
	}
}
