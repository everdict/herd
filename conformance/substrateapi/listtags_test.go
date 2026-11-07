package substrateapi

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// tagsServer answers ListTags with pages, in order, and GetTag from tags; with nil pages it serves no Control method.
func tagsServer(t *testing.T, pages []*ateapi.ListTagsResponse, tags map[string]*ateapi.Tag) *Control {
	t.Helper()
	g := grpc.NewServer()
	if pages != nil {
		g.RegisterService(&grpc.ServiceDesc{ServiceName: "substrate.ateapi.v1.Control", HandlerType: (*any)(nil),
			Methods: []grpc.MethodDesc{
				{MethodName: "ListTags", Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
					var req ateapi.ListTagsRequest
					if err := dec(&req); err != nil {
						return nil, err
					}
					for i, p := range pages {
						if (i == 0 && req.GetOptions().GetPageToken() == "") || (i > 0 && pages[i-1].GetNextPageToken() == req.GetOptions().GetPageToken()) {
							return p, nil
						}
					}
					return nil, errors.New("unknown page token")
				}},
				{MethodName: "GetTag", Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
					var req ateapi.GetTagRequest
					if err := dec(&req); err != nil {
						return nil, err
					}
					if tg := tags[req.GetTag().GetName()]; tg != nil {
						return tg, nil
					}
					return nil, status.Error(codes.NotFound, "no tag")
				}},
			}}, struct{}{})
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	c, err := DialControl(Endpoint{Addr: ln.Addr().String()}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func tagNamed(name, source, tplUID string) *ateapi.Tag {
	return &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: "s", Name: name, Uid: "u-" + name, CreateTime: timestamppb.New(time.Unix(1794000000, 0))},
		Scope:    ateapi.TagScope_TAG_SCOPE_ATESPACE, SourceActor: &ateapi.ObjectRef{Atespace: "s", Name: source},
		Status: &ateapi.TagStatus{ActorTemplateUid: tplUID},
	}
}

// TestListTagsCase (DEFAUL-1613): the list-tags case passes on a target that lists the atespace's tags across pages
// as GetTag answers them, skips with the server's reason on one that does not serve ListTags (herd before DEFAUL-1613
// answered "unknown method ListTags"), and fails on a listing that repeats a tag, leaves out its template uid or scope,
// lists another source actor than GetTag, or leaves out a tag the suite created.
func TestListTagsCase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, b := tagNamed("a", "x", "tpl-1"), tagNamed("b", "y", "tpl-2")
	byName := map[string]*ateapi.Tag{"a": a, "b": b}
	newRun := func(c *Control) *run { return &run{c: c, cfg: Config{Atespace: "s"}, tags: []string{"b"}} }

	r := newRun(tagsServer(t, []*ateapi.ListTagsResponse{{Tags: []*ateapi.Tag{a}, NextPageToken: "a"}, {Tags: []*ateapi.Tag{b}}}, byName))
	if detail, err := r.listTags(ctx); err != nil || !strings.Contains(detail, "2 tags over 2 pages") || !strings.Contains(detail, "2 as GetTag answers them") {
		t.Errorf("two pages: %q, %v", detail, err)
	}

	var d declared
	if _, err := newRun(tagsServer(t, nil, nil)).listTags(ctx); !errors.As(err, &d) || !strings.Contains(d.Error(), "ListTags") {
		t.Errorf("a target that does not serve ListTags: %v, want a declared skip", err)
	}

	noUID := proto.CloneOf(b)
	noUID.Status = nil
	noScope := proto.CloneOf(b)
	noScope.Scope = ateapi.TagScope_TAG_SCOPE_UNSPECIFIED
	moved := proto.CloneOf(b)
	moved.SourceActor.Name = "other"
	for _, c := range []struct {
		what string
		tags []*ateapi.Tag
		want string
	}{
		{"listed twice", []*ateapi.Tag{b, b}, "listed twice"},
		{"no template uid", []*ateapi.Tag{a, noUID}, "actor_template_uid"},
		{"no scope", []*ateapi.Tag{a, noScope}, "no scope"},
		{"another source actor", []*ateapi.Tag{a, moved}, "GetTag answers"},
		{"created one missing", []*ateapi.Tag{a}, "which the suite created"},
	} {
		if _, err := newRun(tagsServer(t, []*ateapi.ListTagsResponse{{Tags: c.tags}}, byName)).listTags(ctx); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a failure saying %q", c.what, err, c.want)
		}
	}
}
