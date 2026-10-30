package egress_test

import (
	"context"
	"errors"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/egress/policy"
)

// TestPolicyUpdateReachesLiveActor (criterion platform-plumbing, "a policy update applies to live actors within
// seconds"): herd-api holds the actor's EgressPolicy; herd-egress's Syncer reads it over the Control API. The actor
// is running and holds open connections (a kept-alive cleartext one and an intercepted TLS one) when the policy is
// updated through UpdateActorEgressPolicy. The very next request on those same connections is decided on the new
// policy, within seconds, and a newly listed host becomes reachable. Applying policies only when an actor is first
// bound (new actors only) leaves the old host reachable.
func TestPolicyUpdateReachesLiveActor(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := e.Client.CreateActorTemplate(ctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "tpl"}, Containers: []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
	}}); err != nil {
		t.Fatal(err)
	}
	act, err := e.Client.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "a1"}, ActorTemplate: &ateapi.ObjectRef{Atespace: e.Space, Name: "tpl"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ref := &ateapi.ObjectRef{Atespace: e.Space, Name: "a1"}
	pol, err := e.Client.CreateActorEgressPolicy(ctx, &ateapi.CreateActorEgressPolicyRequest{Actor: ref, EgressPolicy: &ateapi.EgressPolicy{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "default"},
		Rules:    []*ateapi.EgressRule{httpRule(nil, "a.example"), httpsRule(nil, "a.example", "b.example")},
	}})
	if err != nil {
		t.Fatal(err)
	}

	w := newWorld(t)
	w.resolve("a.example", []string{"198.51.100.50"})
	w.resolve("b.example", []string{"198.51.100.51"})
	g := newGateway(t, w)
	a := egress.Actor{Space: e.Space, Name: "a1", UID: act.GetMetadata().GetUid()}
	g.Bind(a)
	syncErrs := make(chan error, 16)
	s := &egress.Syncer{Gateway: g.Gateway, Source: egress.ControlSource{Conn: e.Client.Conn()}, Interval: 200 * time.Millisecond,
		OnError: func(_ egress.Actor, err error) {
			select {
			case syncErrs <- err:
			default:
			}
		}}
	go func() { _ = s.Run(ctx) }()

	c := g.client(t, a, "198.51.100.50")
	waitFor(t, 5*time.Second, "initial policy", func() bool {
		code, _ := get(t, c, "http://a.example/", nil)
		return code == 200
	})
	if code, body := get(t, c, "https://a.example/", nil); code != 200 {
		t.Fatalf("https://a.example before the update: %d %q", code, body)
	}
	if code, _ := get(t, c, "http://b.example/", nil); code != 403 {
		t.Fatalf("http://b.example before the update: %d, want 403", code)
	}
	flowsBefore := g.flows.Load()

	// The update: a.example is no longer listed for anything; b.example gets cleartext.
	if _, err := e.Client.UpdateActorEgressPolicy(ctx, &ateapi.UpdateActorEgressPolicyRequest{Actor: ref, EgressPolicy: &ateapi.EgressPolicy{
		Metadata: pol.GetMetadata(), Rules: []*ateapi.EgressRule{httpRule(nil, "b.example"), httpsRule(nil, "b.example")},
	}}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	waitFor(t, 5*time.Second, "update to reach the open connections", func() bool {
		c1, _ := get(t, c, "http://a.example/", nil)
		c2, _ := get(t, c, "https://a.example/", nil)
		return c1 == 403 && c2 == 403
	})
	took := time.Since(start)
	if got := g.flows.Load(); got != flowsBefore {
		t.Errorf("the refusals came on %d new flows; they must come on the actor's open connections", got-flowsBefore)
	}
	if code, body := get(t, c, "http://b.example/", nil); code != 200 {
		t.Errorf("http://b.example after the update: %d %q", code, body)
	}
	t.Logf("policy update reached the live actor's open connections in %v", took)
	if took > 3*time.Second {
		t.Errorf("update took %v, want within seconds (<= 3 s at a 200 ms interval)", took)
	}

	// Deleting the policy refuses everything (no policy is deny-all, not "keep the last one").
	if _, err := e.Client.DeleteActorEgressPolicy(ctx, &ateapi.DeleteActorEgressPolicyRequest{Actor: ref}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "delete to deny all", func() bool {
		code, _ := get(t, c, "http://b.example/", nil)
		return code == 403
	})
	select {
	case err := <-syncErrs:
		t.Errorf("syncer error: %v", err)
	default:
	}
}

// TestSyncerUnknownKeepsPolicy: a source that cannot answer leaves the actor on its last policy (unknown is not
// absent); a policy that does not compile denies all.
func TestSyncerUnknownKeepsPolicy(t *testing.T) {
	w := newWorld(t)
	w.resolve("a.example", []string{"198.51.100.50"})
	g := newGateway(t, w)
	src := &fakeSource{p: &ateapi.EgressPolicy{Rules: []*ateapi.EgressRule{httpRule(nil, "a.example")}}}
	g.Bind(actor1)
	s := &egress.Syncer{Gateway: g.Gateway, Source: src}
	ctx := context.Background()
	if err := s.SyncActor(ctx, actor1); err != nil {
		t.Fatal(err)
	}
	c := g.client(t, actor1, "198.51.100.50")
	if code, _ := get(t, c, "http://a.example/", nil); code != 200 {
		t.Fatalf("allowed host: %d", code)
	}
	src.err = errors.New("herd-api unreachable")
	if err := s.SyncActor(ctx, actor1); err == nil {
		t.Fatal("SyncActor with an unreachable source answered nil")
	}
	if code, _ := get(t, c, "http://a.example/", nil); code != 200 {
		t.Errorf("after an unknown answer: %d, want the last policy kept", code)
	}
	src.err = nil
	src.p = &ateapi.EgressPolicy{Rules: []*ateapi.EgressRule{httpRule(nil, "a.example"), httpRule(nil, "a.example")}}
	if err := s.SyncActor(ctx, actor1); !errors.Is(err, policy.ErrInvalid) {
		t.Errorf("SyncActor with a tying policy: %v, want ErrInvalid", err)
	}
	if code, _ := get(t, c, "http://a.example/", nil); code != 403 {
		t.Errorf("after an invalid policy: %d, want 403 (fail closed)", code)
	}
	// Re-binding after an unbind starts from deny-all and is synced again.
	src.p = &ateapi.EgressPolicy{Rules: []*ateapi.EgressRule{httpRule(nil, "a.example")}}
	if err := s.SyncActor(ctx, actor1); err != nil {
		t.Fatal(err)
	}
	g.Unbind(actor1)
	g.Bind(actor1)
	if err := s.SyncActor(ctx, actor1); err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, c, "http://a.example/", nil); code != 200 {
		t.Errorf("after re-bind and sync: %d, want 200", code)
	}
}

type fakeSource struct {
	p   *ateapi.EgressPolicy
	err error
}

func (f *fakeSource) EgressPolicy(context.Context, egress.Actor) (*ateapi.EgressPolicy, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.p == nil {
		return nil, egress.ErrNoPolicy
	}
	return f.p, nil
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
