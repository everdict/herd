package conformance_test

import (
	"strings"
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/conformance"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// listTags pages ListTags in space to the end, page_size at a time, and returns every page.
func listTags(t *testing.T, c *conformance.Client, space string, size int32) [][]*ateapi.Tag {
	t.Helper()
	var pages [][]*ateapi.Tag
	token := ""
	for {
		if len(pages) == 50 {
			t.Fatalf("ListTags: still a next_page_token after 50 pages")
		}
		resp, err := c.ListTags(ctxT(t), &ateapi.ListTagsRequest{Atespace: space, Options: &ateapi.ListOptions{PageSize: size, PageToken: token}})
		if err != nil {
			t.Fatalf("ListTags %s page %d: %v", space, len(pages)+1, err)
		}
		pages = append(pages, resp.GetTags())
		if token = resp.GetNextPageToken(); token == "" {
			return pages
		}
	}
}

func tagOf(t *testing.T, e *conformance.Env, c *conformance.Client, space, name, actor string) *ateapi.Tag {
	t.Helper()
	tag, err := c.CreateTag(ctxT(t), &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: name}, SourceActor: &ateapi.ObjectRef{Atespace: space, Name: actor},
	}})
	if err != nil {
		t.Fatalf("CreateTag %s/%s: %v", space, name, err)
	}
	return tag
}

// TestListedTagCarriesScopeSourceActorAndTemplateUID (DEFAUL-1613): ListTags answers each tag of the atespace as
// GetTag does: its metadata (uid, create time), its scope, its source actor and status.actor_template_uid, the uid
// everdict-herder's template sweep keeps a template by. A golden tag is listed PUBLISHED under its template's uid.
func TestListedTagCarriesScopeSourceActorAndTemplateUID(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	tpl := mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "src", "tpl", nil)
	made := tagOf(t, e, e.Client, e.Space, "keep", "src")
	golden := goldenTemplate(t, e, "gold", nil)
	gs := waitGolden(t, e, "gold")
	if gs.GetGoldenTag() == nil {
		t.Fatalf("golden_snapshot_status = %v", gs)
	}
	pages := listTags(t, e.Client, e.Space, 0)
	if len(pages) != 1 || len(pages[0]) != 2 {
		t.Fatalf("ListTags = %v, want one page of 2 tags", pages)
	}
	byName := map[string]*ateapi.Tag{}
	for _, tag := range pages[0] {
		byName[tag.GetMetadata().GetName()] = tag
		want, err := e.Client.GetTag(ctxT(t), &ateapi.GetTagRequest{Tag: oref(e, tag.GetMetadata().GetName())})
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(tag, want) {
			t.Errorf("%s: listed\n%v\nGetTag\n%v", tag.GetMetadata().GetName(), tag, want)
		}
		if md := tag.GetMetadata(); md.GetUid() == "" || md.GetCreateTime() == nil || md.GetCreateTime().AsTime().IsZero() {
			t.Errorf("%s: listed metadata %v, want a uid and a create_time", md.GetName(), md)
		}
	}
	keep := byName["keep"]
	if keep.GetMetadata().GetUid() != made.GetMetadata().GetUid() || keep.GetScope() != ateapi.TagScope_TAG_SCOPE_ATESPACE ||
		keep.GetSourceActor().GetName() != "src" || keep.GetSourceActor().GetAtespace() != e.Space ||
		keep.GetStatus().GetActorTemplateUid() != tpl.GetMetadata().GetUid() {
		t.Errorf("keep listed as %v, want uid %s, ATESPACE, source actor %s/src, template uid %s", keep, made.GetMetadata().GetUid(), e.Space, tpl.GetMetadata().GetUid())
	}
	g := byName[gs.GetGoldenTag().GetName()]
	if g.GetScope() != ateapi.TagScope_TAG_SCOPE_PUBLISHED || g.GetStatus().GetActorTemplateUid() != golden.GetMetadata().GetUid() {
		t.Errorf("golden tag listed as %v, want PUBLISHED under template uid %s", g, golden.GetMetadata().GetUid())
	}
}

// TestListTagsPagesEachOnce (DEFAUL-1613): paged by page_size and page_token as ListActors is, every tag of the
// atespace comes back exactly once, in name order, and none of another atespace's; a read token lists, a token of
// another atespace and no atespace are refused.
func TestListTagsPagesEachOnce(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	both := conformance.NewClient(e.Addr, e.Token(t, map[string]auth.Role{e.Space: auth.RoleWrite, "space-2": auth.RoleWrite}, false))
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "src", "tpl", nil)
	for _, n := range []string{"t3", "t1", "t5", "t2", "t4"} {
		tagOf(t, e, e.Client, e.Space, n, "src")
	}
	if _, err := both.CreateActorTemplate(ctxT(t), &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata:       &ateapi.ResourceMetadata{Atespace: "space-2", Name: "tpl"},
		Containers:     []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
		SnapshotConfig: &ateapi.SnapshotConfig{GoldenSnapshotConfig: coldBoot},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := both.CreateActor(ctxT(t), &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{Atespace: "space-2", Name: "src"}, ActorTemplate: &ateapi.ObjectRef{Atespace: "space-2", Name: "tpl"},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"o1", "t2"} {
		tagOf(t, e, both, "space-2", n, "src")
	}
	want := []string{"t1", "t2", "t3", "t4", "t5"}
	for _, size := range []int32{1, 2, 5, 100} {
		pages := listTags(t, e.Client, e.Space, size)
		var got []string
		for _, p := range pages {
			if len(p) > int(size) {
				t.Errorf("page_size %d: a page of %d", size, len(p))
			}
			for _, tag := range p {
				if tag.GetMetadata().GetAtespace() != e.Space {
					t.Errorf("page_size %d: %s/%s listed in %s", size, tag.GetMetadata().GetAtespace(), tag.GetMetadata().GetName(), e.Space)
				}
				got = append(got, tag.GetMetadata().GetName())
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("page_size %d: listed %v over %d pages, want each of %v once", size, got, len(pages), want)
		}
	}
	other := listTags(t, both, "space-2", 1)
	if len(other) != 2 || other[0][0].GetMetadata().GetName() != "o1" || other[1][0].GetMetadata().GetName() != "t2" {
		t.Errorf("space-2 = %v, want o1 then t2", other)
	}
	ro := conformance.NewClient(e.Addr, e.Token(t, map[string]auth.Role{e.Space: auth.RoleRead}, false))
	if pages := listTags(t, ro, e.Space, 0); len(pages[0]) != 5 {
		t.Errorf("a read token lists %d tags, want 5", len(pages[0]))
	}
	_, err := e.Client.ListTags(ctxT(t), &ateapi.ListTagsRequest{Atespace: "space-2"})
	wantCode(t, "ListTags in another atespace", err, codes.PermissionDenied)
	_, err = e.Client.ListTags(ctxT(t), &ateapi.ListTagsRequest{})
	wantCode(t, "ListTags with no atespace", err, codes.InvalidArgument)
}
