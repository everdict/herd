package conformance_test

import (
	"strings"
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/conformance"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// listTemplates pages ListActorTemplates in space to the end, page_size at a time, and returns every page.
func listTemplates(t *testing.T, c *conformance.Client, space string, size int32) [][]*ateapi.ActorTemplate {
	t.Helper()
	var pages [][]*ateapi.ActorTemplate
	token := ""
	for {
		if len(pages) == 50 {
			t.Fatalf("ListActorTemplates: still a next_page_token after 50 pages")
		}
		resp, err := c.ListActorTemplates(ctxT(t), &ateapi.ListActorTemplatesRequest{Atespace: space, Options: &ateapi.ListOptions{PageSize: size, PageToken: token}})
		if err != nil {
			t.Fatalf("ListActorTemplates %s page %d: %v", space, len(pages)+1, err)
		}
		pages = append(pages, resp.GetActorTemplates())
		if token = resp.GetNextPageToken(); token == "" {
			return pages
		}
	}
}

// TestListedTemplateCarriesUIDAndCreateTime (DEFAUL-1613): ListActorTemplates answers each template of the atespace
// as GetActorTemplate does: its metadata (atespace, name, uid, version, create time) and its status, here a golden
// tag.
func TestListedTemplateCarriesUIDAndCreateTime(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	cold := mkTemplate(t, e, "cold", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	golden := goldenTemplate(t, e, "golden", nil)
	if gs := waitGolden(t, e, "golden"); gs.GetGoldenTag() == nil {
		t.Fatalf("golden_snapshot_status = %v", gs)
	}
	pages := listTemplates(t, e.Client, e.Space, 0)
	if len(pages) != 1 || len(pages[0]) != 2 {
		t.Fatalf("ListActorTemplates = %v, want one page of 2 templates", pages)
	}
	for i, made := range []*ateapi.ActorTemplate{cold, golden} {
		got := pages[0][i]
		md := got.GetMetadata()
		if md.GetUid() == "" || md.GetUid() != made.GetMetadata().GetUid() {
			t.Errorf("%s: listed uid %q, created %q", made.GetMetadata().GetName(), md.GetUid(), made.GetMetadata().GetUid())
		}
		if md.GetCreateTime() == nil || md.GetCreateTime().AsTime().IsZero() || !md.GetCreateTime().AsTime().Equal(made.GetMetadata().GetCreateTime().AsTime()) {
			t.Errorf("%s: listed create_time %v, created %v", made.GetMetadata().GetName(), md.GetCreateTime(), made.GetMetadata().GetCreateTime())
		}
		want, err := e.Client.GetActorTemplate(ctxT(t), &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, made.GetMetadata().GetName())})
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(got, want) {
			t.Errorf("%s: listed\n%v\nGetActorTemplate\n%v", made.GetMetadata().GetName(), got, want)
		}
	}
	if pages[0][1].GetStatus().GetGoldenSnapshotStatus().GetGoldenTag() == nil {
		t.Errorf("golden: listed with status %v, want its golden tag", pages[0][1].GetStatus())
	}
}

// TestListTemplatesPagesEachOnce (DEFAUL-1613): paged by page_size and page_token as ListActors is, every template of
// the atespace comes back exactly once, in name order, and none of another atespace's; a read token lists, a token
// of another atespace and no atespace are refused.
func TestListTemplatesPagesEachOnce(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	both := conformance.NewClient(e.Addr, e.Token(t, map[string]auth.Role{e.Space: auth.RoleWrite, "space-2": auth.RoleWrite}, false))
	for _, n := range []string{"t3", "t1", "t5", "t2", "t4"} {
		mkTemplate(t, e, n, ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	}
	for _, n := range []string{"o1", "t2"} {
		if _, err := both.CreateActorTemplate(ctxT(t), &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
			Metadata:       &ateapi.ResourceMetadata{Atespace: "space-2", Name: n},
			Containers:     []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
			SnapshotConfig: &ateapi.SnapshotConfig{GoldenSnapshotConfig: coldBoot},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"t1", "t2", "t3", "t4", "t5"}
	for _, size := range []int32{1, 2, 5, 100} {
		pages := listTemplates(t, e.Client, e.Space, size)
		var got []string
		for _, p := range pages {
			if len(p) > int(size) {
				t.Errorf("page_size %d: a page of %d", size, len(p))
			}
			for _, tpl := range p {
				if tpl.GetMetadata().GetAtespace() != e.Space {
					t.Errorf("page_size %d: %s/%s listed in %s", size, tpl.GetMetadata().GetAtespace(), tpl.GetMetadata().GetName(), e.Space)
				}
				got = append(got, tpl.GetMetadata().GetName())
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("page_size %d: listed %v over %d pages, want each of %v once", size, got, len(pages), want)
		}
	}
	other := listTemplates(t, both, "space-2", 1)
	if len(other) != 2 || other[0][0].GetMetadata().GetName() != "o1" || other[1][0].GetMetadata().GetName() != "t2" {
		t.Errorf("space-2 = %v, want o1 then t2", other)
	}
	ro := conformance.NewClient(e.Addr, e.Token(t, map[string]auth.Role{e.Space: auth.RoleRead}, false))
	if pages := listTemplates(t, ro, e.Space, 0); len(pages[0]) != 5 {
		t.Errorf("a read token lists %d templates, want 5", len(pages[0]))
	}
	_, err := e.Client.ListActorTemplates(ctxT(t), &ateapi.ListActorTemplatesRequest{Atespace: "space-2"})
	wantCode(t, "ListActorTemplates in another atespace", err, codes.PermissionDenied)
	_, err = e.Client.ListActorTemplates(ctxT(t), &ateapi.ListActorTemplatesRequest{})
	wantCode(t, "ListActorTemplates with no atespace", err, codes.InvalidArgument)
}

// TestDeleteTemplateRemovesItsGoldenAndObject (DEFAUL-1613): a deleted template takes its golden tag and the tag's
// snapshot object with it, and is no longer listed.
func TestDeleteTemplateRemovesItsGoldenAndObject(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	goldenTemplate(t, e, "tpl", nil)
	gs := waitGolden(t, e, "tpl")
	tag, err := e.Client.GetTag(ctxT(t), &ateapi.GetTagRequest{Tag: gs.GetGoldenTag()})
	if err != nil {
		t.Fatal(err)
	}
	obj := tag.GetStatus().GetSnapshot().GetUuid()
	if _, err := e.Snapshots.Stat(ctxT(t), obj); err != nil {
		t.Fatalf("golden snapshot object %s before the delete: %v", obj, err)
	}
	if _, err := e.Client.DeleteActorTemplate(ctxT(t), &ateapi.DeleteActorTemplateRequest{ActorTemplate: oref(e, "tpl")}); err != nil {
		t.Fatalf("DeleteActorTemplate: %v", err)
	}
	_, err = e.Client.GetTag(ctxT(t), &ateapi.GetTagRequest{Tag: gs.GetGoldenTag()})
	wantCode(t, "GetTag of the golden after the template's delete", err, codes.NotFound)
	if _, err := e.Snapshots.Stat(ctxT(t), obj); err == nil {
		t.Errorf("golden snapshot object %s survived the template", obj)
	}
	_, err = e.Client.GetActorTemplate(ctxT(t), &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, "tpl")})
	wantCode(t, "GetActorTemplate after its delete", err, codes.NotFound)
	if pages := listTemplates(t, e.Client, e.Space, 0); len(pages[0]) != 0 {
		t.Errorf("ListActorTemplates after the delete = %v", pages[0])
	}
}

// TestDeleteTemplateRefusedWhileAnActorRuns (DEFAUL-1613): while an actor of the template exists, DeleteActorTemplate
// is FailedPrecondition and changes nothing: the template, its golden tag and the tag's object stay. Once the actor is
// gone the delete goes through.
func TestDeleteTemplateRefusedWhileAnActorRuns(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	goldenTemplate(t, e, "tpl", nil)
	gs := waitGolden(t, e, "tpl")
	tag, err := e.Client.GetTag(ctxT(t), &ateapi.GetTagRequest{Tag: gs.GetGoldenTag()})
	if err != nil {
		t.Fatal(err)
	}
	obj := tag.GetStatus().GetSnapshot().GetUuid()
	mkActor(t, e, "a1", "tpl", nil)
	_, err = e.Client.DeleteActorTemplate(ctxT(t), &ateapi.DeleteActorTemplateRequest{ActorTemplate: oref(e, "tpl")})
	wantCode(t, "DeleteActorTemplate with actor a1 running from it", err, codes.FailedPrecondition)
	if !strings.Contains(status.Convert(err).Message(), "a1") {
		t.Errorf("the refusal does not name the actor: %v", err)
	}
	if _, err := e.Client.GetActorTemplate(ctxT(t), &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, "tpl")}); err != nil {
		t.Errorf("GetActorTemplate after a refused delete: %v", err)
	}
	if _, err := e.Client.GetTag(ctxT(t), &ateapi.GetTagRequest{Tag: gs.GetGoldenTag()}); err != nil {
		t.Errorf("GetTag of the golden after a refused delete: %v", err)
	}
	if _, err := e.Snapshots.Stat(ctxT(t), obj); err != nil {
		t.Errorf("golden snapshot object %s after a refused delete: %v", obj, err)
	}
	if _, err := e.Client.DeleteActor(ctxT(t), &ateapi.DeleteActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Client.DeleteActorTemplate(ctxT(t), &ateapi.DeleteActorTemplateRequest{ActorTemplate: oref(e, "tpl")}); err != nil {
		t.Errorf("DeleteActorTemplate once a1 is gone: %v", err)
	}
}

// TestDeleteTemplateRefusedWhileATagHoldsIt (DEFAUL-1613): a tag other than the template's golden that holds the
// template's uid (a tag of one of its actors) refuses DeleteActorTemplate with FailedPrecondition, as an actor does:
// an actor created from that tag needs the template. The tag and the template stay; once the tag is gone the delete
// goes through.
func TestDeleteTemplateRefusedWhileATagHoldsIt(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	tpl := mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "src", "tpl", nil)
	tag, err := e.Client.CreateTag(ctxT(t), &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "keep"}, SourceActor: oref(e, "src"),
	}})
	if err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	if tag.GetStatus().GetActorTemplateUid() != tpl.GetMetadata().GetUid() {
		t.Fatalf("tag holds template uid %q, want %q", tag.GetStatus().GetActorTemplateUid(), tpl.GetMetadata().GetUid())
	}
	if _, err := e.Client.DeleteActor(ctxT(t), &ateapi.DeleteActorRequest{Actor: oref(e, "src")}); err != nil {
		t.Fatal(err)
	}
	_, err = e.Client.DeleteActorTemplate(ctxT(t), &ateapi.DeleteActorTemplateRequest{ActorTemplate: oref(e, "tpl")})
	wantCode(t, "DeleteActorTemplate with tag keep holding it", err, codes.FailedPrecondition)
	if !strings.Contains(status.Convert(err).Message(), "keep") {
		t.Errorf("the refusal does not name the tag: %v", err)
	}
	if _, err := e.Client.GetActorTemplate(ctxT(t), &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, "tpl")}); err != nil {
		t.Errorf("GetActorTemplate after a refused delete: %v", err)
	}
	if _, err := e.Snapshots.Stat(ctxT(t), tag.GetStatus().GetSnapshot().GetUuid()); err != nil {
		t.Errorf("tag keep's object after a refused delete: %v", err)
	}
	if _, err := e.Client.DeleteTag(ctxT(t), &ateapi.DeleteTagRequest{Tag: oref(e, "keep")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Client.DeleteActorTemplate(ctxT(t), &ateapi.DeleteActorTemplateRequest{ActorTemplate: oref(e, "tpl")}); err != nil {
		t.Errorf("DeleteActorTemplate once tag keep is gone: %v", err)
	}
}
