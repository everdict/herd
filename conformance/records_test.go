package conformance_test

import (
	"os"
	"strings"
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/conformance"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSubsetServed: every method of the pinned subset is served over gRPC, and nothing outside it.
func TestSubsetServed(t *testing.T) {
	e := conformance.Start(t)
	b, err := os.ReadFile("../api/ateapi/v1/subset.txt")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		m := strings.TrimSpace(line)
		if m == "" || strings.HasPrefix(m, "#") {
			continue
		}
		n++
		err := e.Client.Raw(ctxT(t), m, &ateapi.GetActorRequest{}, &ateapi.Actor{})
		if c := status.Code(err); c == codes.Unimplemented || c == codes.OK {
			t.Errorf("%s with an empty request: %v (want a served method refusing the request)", m, err)
		}
	}
	if n != 17 {
		t.Errorf("subset.txt lists %d methods, want 17", n)
	}
	for _, m := range []string{"RevertActor", "ListTags", "UpdateTag", "ListActorTemplates"} {
		wantCode(t, m, e.Client.Raw(ctxT(t), m, &ateapi.GetActorRequest{}, &ateapi.Actor{}), codes.Unimplemented)
	}
}

// TestStaleVersionRefused: an update or delete carrying a stale uid or version is refused with Aborted and changes
// nothing; an update without uid+version is refused.
func TestStaleVersionRefused(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	a := mkActor(t, e, "a1", "tpl", nil)
	stale := a.GetMetadata()

	upd := func(md *ateapi.ResourceMetadata, zone string) (*ateapi.Actor, error) {
		return e.Client.UpdateActor(ctx, &ateapi.UpdateActorRequest{Actor: &ateapi.Actor{
			Metadata: md, WorkerSelector: &ateapi.Selector{MatchLabels: map[string]string{"zone": zone}},
		}})
	}
	fresh, err := upd(stale, "z1")
	if err != nil {
		t.Fatalf("UpdateActor at the current version: %v", err)
	}
	if fresh.GetMetadata().GetVersion() <= stale.GetVersion() || fresh.GetMetadata().GetUid() != stale.GetUid() {
		t.Errorf("version %d → %d, uid %q → %q", stale.GetVersion(), fresh.GetMetadata().GetVersion(), stale.GetUid(), fresh.GetMetadata().GetUid())
	}
	_, err = upd(stale, "lost")
	wantCode(t, "UpdateActor with a stale version", err, codes.Aborted)
	wrongUID := &ateapi.ResourceMetadata{Atespace: e.Space, Name: "a1", Uid: "00000000-0000-4000-8000-000000000000", Version: fresh.GetMetadata().GetVersion()}
	_, err = upd(wrongUID, "lost")
	wantCode(t, "UpdateActor with another uid", err, codes.Aborted)
	_, err = upd(&ateapi.ResourceMetadata{Atespace: e.Space, Name: "a1"}, "lost")
	wantCode(t, "UpdateActor without uid+version", err, codes.InvalidArgument)
	got, _ := getActor(t, e, "a1")
	if z := got.GetWorkerSelector().GetMatchLabels()["zone"]; z != "z1" || got.GetMetadata().GetVersion() != fresh.GetMetadata().GetVersion() {
		t.Errorf("after refused updates: zone %q version %d, want z1 at %d", z, got.GetMetadata().GetVersion(), fresh.GetMetadata().GetVersion())
	}
	_, err = e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "a1"), Options: &ateapi.DeleteOptions{Uid: stale.GetUid(), Version: stale.GetVersion()}})
	wantCode(t, "DeleteActor with a stale version", err, codes.Aborted)
	if p := present(t, e.Herdlets["h1"], a); p.String() != "present" {
		t.Errorf("a refused delete touched the VM: %v", p)
	}

	// Egress policy: the same precondition.
	pol, err := e.Client.CreateActorEgressPolicy(ctx, &ateapi.CreateActorEgressPolicyRequest{Actor: oref(e, "a1"), EgressPolicy: &ateapi.EgressPolicy{
		Rules: []*ateapi.EgressRule{{Https: &ateapi.HttpsRule{Hostnames: []string{"api.github.com"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pol2, err := e.Client.UpdateActorEgressPolicy(ctx, &ateapi.UpdateActorEgressPolicyRequest{Actor: oref(e, "a1"), EgressPolicy: &ateapi.EgressPolicy{
		Metadata: pol.GetMetadata(), Rules: []*ateapi.EgressRule{{Https: &ateapi.HttpsRule{Hostnames: []string{"pypi.org"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Client.UpdateActorEgressPolicy(ctx, &ateapi.UpdateActorEgressPolicyRequest{Actor: oref(e, "a1"), EgressPolicy: &ateapi.EgressPolicy{
		Metadata: pol.GetMetadata(), Rules: []*ateapi.EgressRule{{Https: &ateapi.HttpsRule{Hostnames: []string{"evil.example"}}}},
	}})
	wantCode(t, "UpdateActorEgressPolicy with a stale version", err, codes.Aborted)
	cur, err := e.Client.GetActorEgressPolicy(ctx, &ateapi.GetActorEgressPolicyRequest{Actor: oref(e, "a1")})
	if err != nil || cur.GetRules()[0].GetHttps().GetHostnames()[0] != "pypi.org" || cur.GetMetadata().GetVersion() != pol2.GetMetadata().GetVersion() {
		t.Errorf("egress policy after a refused update: %v, %v", cur, err)
	}
}

// TestRecordsRoundTrip: templates, actors, tags and egress policies are kept and listed with their identity.
func TestRecordsRoundTrip(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 8})
	ctx := ctxT(t)
	tpl := mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_UNSPECIFIED)
	got, err := e.Client.GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, "tpl")})
	if err != nil || got.GetMetadata().GetUid() != tpl.GetMetadata().GetUid() || got.GetContainers()[0].GetImage() != "ghcr.io/everdict/agent:1" {
		t.Fatalf("GetActorTemplate = %v, %v", got, err)
	}
	_, err = e.Client.CreateActorTemplate(ctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "tpl"}, Containers: []*ateapi.Container{{Name: "x", Image: "x"}},
	}})
	wantCode(t, "CreateActorTemplate twice", err, codes.AlreadyExists)
	for _, n := range []string{"c", "a", "b"} {
		mkActor(t, e, n, "tpl", nil)
	}
	_, err = e.Client.CreateActor(ctx, actorReq(e, "a", "tpl", nil))
	wantCode(t, "CreateActor twice", err, codes.AlreadyExists)
	page, err := e.Client.ListActors(ctx, &ateapi.ListActorsRequest{Atespace: e.Space, Options: &ateapi.ListOptions{PageSize: 2}})
	if err != nil || len(page.GetActors()) != 2 || page.GetActors()[0].GetMetadata().GetName() != "a" || page.GetNextPageToken() == "" {
		t.Fatalf("ListActors page 1 = %v, %v", page, err)
	}
	page2, err := e.Client.ListActors(ctx, &ateapi.ListActorsRequest{Atespace: e.Space, Options: &ateapi.ListOptions{PageSize: 2, PageToken: page.GetNextPageToken()}})
	if err != nil || len(page2.GetActors()) != 1 || page2.GetActors()[0].GetMetadata().GetName() != "c" || page2.GetNextPageToken() != "" {
		t.Fatalf("ListActors page 2 = %v, %v", page2, err)
	}
	_, err = e.Client.DeleteActorTemplate(ctx, &ateapi.DeleteActorTemplateRequest{ActorTemplate: oref(e, "tpl")})
	wantCode(t, "DeleteActorTemplate in use", err, codes.FailedPrecondition)
	_, err = e.Client.GetActor(ctx, &ateapi.GetActorRequest{Actor: oref(e, "nope")})
	wantCode(t, "GetActor missing", err, codes.NotFound)
	if _, err := e.Client.CreateActorEgressPolicy(ctx, &ateapi.CreateActorEgressPolicyRequest{Actor: oref(e, "a"), EgressPolicy: &ateapi.EgressPolicy{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Client.DeleteActorEgressPolicy(ctx, &ateapi.DeleteActorEgressPolicyRequest{Actor: oref(e, "a")}); err != nil {
		t.Fatal(err)
	}
	_, err = e.Client.GetActorEgressPolicy(ctx, &ateapi.GetActorEgressPolicyRequest{Actor: oref(e, "a")})
	wantCode(t, "GetActorEgressPolicy after delete", err, codes.NotFound)
	for _, n := range []string{"a", "b", "c"} {
		if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, n)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Client.DeleteActorTemplate(ctx, &ateapi.DeleteActorTemplateRequest{ActorTemplate: oref(e, "tpl")}); err != nil {
		t.Errorf("DeleteActorTemplate unused: %v", err)
	}
}

// TestAuthorityBeforeEffect: no token, a token for another space, a read-only token and a herdlet credential are
// all refused before anything changes.
func TestAuthorityBeforeEffect(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	cases := []struct {
		name  string
		token string
		code  codes.Code
	}{
		{"no token", "", codes.Unauthenticated},
		{"other key", "x.y.z", codes.Unauthenticated},
		{"other space", e.Token(t, map[string]auth.Role{"space-2": auth.RoleWrite}, false), codes.PermissionDenied},
		{"read only", e.Token(t, map[string]auth.Role{e.Space: auth.RoleRead}, false), codes.PermissionDenied},
		{"herdlet credential", e.Token(t, nil, true), codes.PermissionDenied},
	}
	for _, c := range cases {
		cl := conformance.NewClient(e.Addr, c.token)
		_, err := cl.CreateActor(ctx, actorReq(e, "a-"+strings.ReplaceAll(c.name, " ", "-"), "tpl", nil))
		wantCode(t, c.name+": CreateActor", err, c.code)
		cl.Close()
	}
	list, err := e.Client.ListActors(ctx, &ateapi.ListActorsRequest{Atespace: e.Space})
	if err != nil || len(list.GetActors()) != 0 {
		t.Errorf("refused creates left actors: %v, %v", list, err)
	}
	ro := conformance.NewClient(e.Addr, e.Token(t, map[string]auth.Role{e.Space: auth.RoleRead}, false))
	defer ro.Close()
	if _, err := ro.GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, "tpl")}); err != nil {
		t.Errorf("read-only GetActorTemplate: %v", err)
	}
}
