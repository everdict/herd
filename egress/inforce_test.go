package egress_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/egress"
)

// TestPolicyInForceWhenAcknowledged (PLT-229, criterion policy-in-force): a policy write is in force when herd-api
// acknowledges it. The syncer's poll cannot help here (its interval is an hour, after one first read); every flow is
// issued the moment Create/Update/DeleteActorEgressPolicy returns and must be decided by the policy that call wrote:
//   - Create: a TLS passthrough flow and a cleartext request to a newly listed host are allowed (the cluster probe:
//     refused "no rule allows tls" until the next poll).
//   - Update that drops a host: a new flow and the next request on an open connection are refused.
//   - Delete: everything is refused.
func TestPolicyInForceWhenAcknowledged(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := e.Client.CreateActorTemplate(ctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "tpl"}, Containers: []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
		SnapshotConfig: &ateapi.SnapshotConfig{GoldenSnapshotConfig: &ateapi.GoldenSnapshotConfig{Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED}},
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

	w := newWorld(t)
	w.resolve("a.example", []string{"198.51.100.50"})
	w.resolve("b.example", []string{"198.51.100.51"})
	g := newGateway(t, w)
	a := egress.Actor{Space: e.Space, Name: "a1", UID: act.GetMetadata().GetUid()}
	g.Bind(a)
	src := &countingSource{PolicySource: egress.ControlSource{Conn: e.Client.Conn()}}
	s := &egress.Syncer{Gateway: g.Gateway, Source: src, Interval: time.Hour}
	go func() { _ = s.Run(ctx) }()
	waitFor(t, 5*time.Second, "the syncer's first read", func() bool { return src.reads.Load() >= 1 })

	c := g.client(t, a, "198.51.100.50")
	if code, _ := get(t, c, "http://a.example/", nil); code != 403 {
		t.Fatalf("http://a.example with no policy: %d, want 403", code)
	}

	pol, err := e.Client.CreateActorEgressPolicy(ctx, &ateapi.CreateActorEgressPolicyRequest{Actor: ref, EgressPolicy: &ateapi.EgressPolicy{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "default"},
		Rules:    []*ateapi.EgressRule{passRule(8443, "a.example"), httpRule(nil, "a.example", "b.example")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got, detail := passthroughOf(t, g, a, "198.51.100.50", 8443, "a.example"); got != "ok" {
		t.Errorf("TLS passthrough to a.example right after CreateActorEgressPolicy returned: %s (%s), want allowed", got, detail)
	}
	if code, body := get(t, c, "http://a.example/", nil); code != 200 {
		t.Errorf("http://a.example right after CreateActorEgressPolicy returned: %d %q, want 200", code, body)
	}

	// The update drops a.example: the open connection's next request and a new flow are refused at once.
	if _, err := e.Client.UpdateActorEgressPolicy(ctx, &ateapi.UpdateActorEgressPolicyRequest{Actor: ref, EgressPolicy: &ateapi.EgressPolicy{
		Metadata: pol.GetMetadata(), Rules: []*ateapi.EgressRule{httpRule(nil, "b.example")},
	}}); err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, c, "http://a.example/", nil); code != 403 {
		t.Errorf("http://a.example on the open connection right after UpdateActorEgressPolicy returned: %d, want 403", code)
	}
	if got, _ := passthroughOf(t, g, a, "198.51.100.50", 8443, "a.example"); got != "refused" {
		t.Errorf("TLS passthrough to a.example right after UpdateActorEgressPolicy returned: %s, want refused", got)
	}
	if code, body := get(t, g.client(t, a, "198.51.100.51"), "http://b.example/", nil); code != 200 {
		t.Errorf("http://b.example right after UpdateActorEgressPolicy returned: %d %q, want 200", code, body)
	}

	if _, err := e.Client.DeleteActorEgressPolicy(ctx, &ateapi.DeleteActorEgressPolicyRequest{Actor: ref}); err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, g.client(t, a, "198.51.100.51"), "http://b.example/", nil); code != 403 {
		t.Errorf("http://b.example right after DeleteActorEgressPolicy returned: %d, want 403", code)
	}
}

type countingSource struct {
	egress.PolicySource
	reads atomic.Int64
}

func (c *countingSource) EgressPolicy(ctx context.Context, a egress.Actor) (*ateapi.EgressPolicy, error) {
	defer c.reads.Add(1)
	return c.PolicySource.EgressPolicy(ctx, a)
}

// passthroughOf is passthrough for any actor.
func passthroughOf(t *testing.T, g *gw, a egress.Actor, ip string, port uint16, sni string) (string, string) {
	t.Helper()
	conn := g.flow(t, a, netip.AddrPortFrom(netip.MustParseAddr(ip), port))
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	tc := tls.Client(conn, &tls.Config{ServerName: sni, RootCAs: g.world.upCA.Pool()})
	if err := tc.Handshake(); err != nil {
		return "refused", err.Error()
	}
	fmt.Fprintf(tc, "hello\n")
	b, _ := io.ReadAll(tc)
	if !strings.HasPrefix(string(b), "passthrough "+sni) {
		return "bad", string(b)
	}
	return "ok", string(b)
}

// TestPolicyRevocationInForceWhenAcknowledged (PLT-229, criterion policy-in-force): the narrowing half on its own.
// herd-egress already holds a policy listing a.example and b.example (read once, then an hour's poll interval); an
// Update that drops a.example, then a Delete, are in force when they return: the next request on the open
// connection and a new flow are decided by the written policy, not the one herd-egress held.
func TestPolicyRevocationInForceWhenAcknowledged(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := e.Client.CreateActorTemplate(ctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "tpl"}, Containers: []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
		SnapshotConfig: &ateapi.SnapshotConfig{GoldenSnapshotConfig: &ateapi.GoldenSnapshotConfig{Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED}},
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
		Rules:    []*ateapi.EgressRule{passRule(8443, "a.example"), httpRule(nil, "a.example", "b.example")},
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
	src := &countingSource{PolicySource: egress.ControlSource{Conn: e.Client.Conn()}}
	s := &egress.Syncer{Gateway: g.Gateway, Source: src, Interval: time.Hour}
	go func() { _ = s.Run(ctx) }()
	waitFor(t, 5*time.Second, "the syncer's first read", func() bool { return src.reads.Load() >= 1 })

	c := g.client(t, a, "198.51.100.50")
	if code, body := get(t, c, "http://a.example/", nil); code != 200 {
		t.Fatalf("http://a.example under the first policy: %d %q, want 200", code, body)
	}

	if _, err := e.Client.UpdateActorEgressPolicy(ctx, &ateapi.UpdateActorEgressPolicyRequest{Actor: ref, EgressPolicy: &ateapi.EgressPolicy{
		Metadata: pol.GetMetadata(), Rules: []*ateapi.EgressRule{httpRule(nil, "b.example")},
	}}); err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, c, "http://a.example/", nil); code != 403 {
		t.Errorf("http://a.example on the open connection right after UpdateActorEgressPolicy returned: %d, want 403", code)
	}
	if got, _ := passthroughOf(t, g, a, "198.51.100.50", 8443, "a.example"); got != "refused" {
		t.Errorf("TLS passthrough to a.example right after UpdateActorEgressPolicy returned: %s, want refused", got)
	}
	b := g.client(t, a, "198.51.100.51")
	if code, body := get(t, b, "http://b.example/", nil); code != 200 {
		t.Errorf("http://b.example right after UpdateActorEgressPolicy returned: %d %q, want 200", code, body)
	}

	if _, err := e.Client.DeleteActorEgressPolicy(ctx, &ateapi.DeleteActorEgressPolicyRequest{Actor: ref}); err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, b, "http://b.example/", nil); code != 403 {
		t.Errorf("http://b.example right after DeleteActorEgressPolicy returned: %d, want 403", code)
	}
}

// TestRefusalReadIsBounded (PLT-229): the read a refusal waits for is bounded by Syncer.Wait. With herd-api hung, a
// refused flow is refused after about Wait, not held until the source answers.
func TestRefusalReadIsBounded(t *testing.T) {
	w := newWorld(t)
	w.resolve("a.example", []string{"198.51.100.50"})
	g := newGateway(t, w)
	g.Bind(actor1)
	src := &hangingSource{}
	s := &egress.Syncer{Gateway: g.Gateway, Source: src, Interval: time.Hour, Wait: 300 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	waitFor(t, 5*time.Second, "the syncer's first read", func() bool { return src.reads.Load() >= 1 })
	src.hang.Store(true)
	start := time.Now()
	code, _ := get(t, g.client(t, actor1, "198.51.100.50"), "http://a.example/", nil)
	took := time.Since(start)
	if code != 403 {
		t.Errorf("http://a.example with no policy and herd-api hung: %d, want 403", code)
	}
	if took < 300*time.Millisecond || took > 2*time.Second {
		t.Errorf("the refusal took %v, want about Wait (300ms)", took)
	}
	if src.reads.Load() < 2 {
		t.Errorf("reads = %d: the refusal did not read the policy again", src.reads.Load())
	}
}

type hangingSource struct {
	hang  atomic.Bool
	reads atomic.Int64
}

func (h *hangingSource) EgressPolicy(ctx context.Context, _ egress.Actor) (*ateapi.EgressPolicy, error) {
	h.reads.Add(1)
	if h.hang.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, egress.ErrNoPolicy
}
