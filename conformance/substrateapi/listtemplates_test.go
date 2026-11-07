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
	"google.golang.org/protobuf/types/known/timestamppb"
)

// templatesServer answers ListActorTemplates with pages, in order; with nil pages it serves no Control method at all.
func templatesServer(t *testing.T, pages []*ateapi.ListActorTemplatesResponse) *Control {
	t.Helper()
	g := grpc.NewServer()
	if pages != nil {
		g.RegisterService(&grpc.ServiceDesc{ServiceName: "substrate.ateapi.v1.Control", HandlerType: (*any)(nil),
			Methods: []grpc.MethodDesc{{MethodName: "ListActorTemplates", Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				var req ateapi.ListActorTemplatesRequest
				if err := dec(&req); err != nil {
					return nil, err
				}
				for i, p := range pages {
					if (i == 0 && req.GetOptions().GetPageToken() == "") || (i > 0 && pages[i-1].GetNextPageToken() == req.GetOptions().GetPageToken()) {
						return p, nil
					}
				}
				return nil, errors.New("unknown page token")
			}}}}, struct{}{})
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

func listed(name, uid string) *ateapi.ActorTemplate {
	md := &ateapi.ResourceMetadata{Atespace: "s", Name: name, Uid: uid}
	if uid != "" {
		md.CreateTime = timestamppb.New(time.Unix(1794000000, 0))
	}
	return &ateapi.ActorTemplate{Metadata: md}
}

// TestListTemplatesCase (DEFAUL-1613): the list-templates case passes on a target that lists the atespace's templates
// across pages, skips with the server's reason on one that does not serve ListActorTemplates (herd v0.1.10 answered
// "unknown method ListActorTemplates"), and fails on a listing that repeats a template, leaves out its uid or create
// time, or leaves out the suite's template or one the suite created.
func TestListTemplatesCase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	newRun := func(c *Control) *run {
		return &run{c: c, cfg: Config{Atespace: "s", Template: "agent"}, tpl: listed("agent", "u-agent"), templates: []string{"cf-golden"}}
	}

	r := newRun(templatesServer(t, []*ateapi.ListActorTemplatesResponse{
		{ActorTemplates: []*ateapi.ActorTemplate{listed("agent", "u-agent"), listed("cf-golden", "u-g")}, NextPageToken: "cf-golden"},
		{ActorTemplates: []*ateapi.ActorTemplate{listed("pair", "u-pair")}},
	}))
	if detail, err := r.listTemplates(ctx); err != nil || !strings.Contains(detail, "3 templates over 2 pages") {
		t.Errorf("two pages: %q, %v", detail, err)
	}

	var d declared
	if _, err := newRun(templatesServer(t, nil)).listTemplates(ctx); !errors.As(err, &d) || !strings.Contains(d.Error(), "ListActorTemplates") {
		t.Errorf("a target that does not serve ListActorTemplates: %v, want a declared skip", err)
	}

	noTime := listed("pair", "u-pair")
	noTime.Metadata.CreateTime = nil
	for _, c := range []struct {
		what  string
		pages []*ateapi.ListActorTemplatesResponse
		want  string
	}{
		{"listed twice", []*ateapi.ListActorTemplatesResponse{
			{ActorTemplates: []*ateapi.ActorTemplate{listed("agent", "u-agent"), listed("cf-golden", "u-g")}, NextPageToken: "cf-golden"},
			{ActorTemplates: []*ateapi.ActorTemplate{listed("cf-golden", "u-g")}},
		}, "listed twice"},
		{"no uid", []*ateapi.ListActorTemplatesResponse{{ActorTemplates: []*ateapi.ActorTemplate{listed("agent", "u-agent"), listed("cf-golden", "")}}}, "no uid"},
		{"no create time", []*ateapi.ListActorTemplatesResponse{{ActorTemplates: []*ateapi.ActorTemplate{listed("agent", "u-agent"), noTime}}}, "no create_time"},
		{"another uid", []*ateapi.ListActorTemplatesResponse{{ActorTemplates: []*ateapi.ActorTemplate{listed("agent", "u-other"), listed("cf-golden", "u-g")}}}, "suite's template"},
		{"created one missing", []*ateapi.ListActorTemplatesResponse{{ActorTemplates: []*ateapi.ActorTemplate{listed("agent", "u-agent")}}}, "cf-golden"},
	} {
		if _, err := newRun(templatesServer(t, c.pages)).listTemplates(ctx); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a failure saying %q", c.what, err, c.want)
		}
	}
}
