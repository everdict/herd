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
)

// workersServer answers ListWorkers with pages, in order; with nil pages it serves no Control method at all.
func workersServer(t *testing.T, pages []*ateapi.ListWorkersResponse) *Control {
	t.Helper()
	g := grpc.NewServer()
	if pages != nil {
		g.RegisterService(&grpc.ServiceDesc{ServiceName: "substrate.ateapi.v1.Control", HandlerType: (*any)(nil),
			Methods: []grpc.MethodDesc{{MethodName: "ListWorkers", Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				var req ateapi.ListWorkersRequest
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

func worker(name, class string) *ateapi.Worker {
	w := &ateapi.Worker{Metadata: &ateapi.ResourceMetadata{Name: name}, Status: &ateapi.WorkerStatus{}}
	if class != "" {
		w.Status.SandboxClasses = []*ateapi.WorkerSandboxClass{{SandboxClass: class}}
	}
	return w
}

// TestListWorkersCase (PLT-219): the list-workers case passes on a target that lists its workers across pages,
// skips with the server's reason on one that does not serve ListWorkers (herd before PLT-219 answered
// "unknown method ListWorkers"), and fails on a listing that names no class or leaves out the resumed actor's worker.
func TestListWorkersCase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r := &run{c: workersServer(t, []*ateapi.ListWorkersResponse{
		{Workers: []*ateapi.Worker{worker("w1", "microvm")}, NextPageToken: "w1"},
		{Workers: []*ateapi.Worker{worker("w2", "gvisor")}},
	}), ranOn: "w2"}
	if detail, err := r.listWorkers(ctx); err != nil || !strings.Contains(detail, "2 workers (w1, w2)") || !strings.Contains(detail, "gvisor, microvm") {
		t.Errorf("two pages: %q, %v", detail, err)
	}

	r = &run{c: workersServer(t, nil)}
	var d declared
	if _, err := r.listWorkers(ctx); !errors.As(err, &d) || !strings.Contains(d.Error(), "ListWorkers") {
		t.Errorf("a target that does not serve ListWorkers: %v, want a declared skip", err)
	}

	for _, c := range []struct {
		what  string
		pages []*ateapi.ListWorkersResponse
		ranOn string
	}{
		{"no worker", []*ateapi.ListWorkersResponse{{}}, ""},
		{"no sandbox class", []*ateapi.ListWorkersResponse{{Workers: []*ateapi.Worker{worker("w1", "")}}}, ""},
		{"an atespaced worker", []*ateapi.ListWorkersResponse{{Workers: []*ateapi.Worker{{Metadata: &ateapi.ResourceMetadata{Atespace: "s", Name: "w1"},
			Status: &ateapi.WorkerStatus{SandboxClasses: []*ateapi.WorkerSandboxClass{{SandboxClass: "microvm"}}}}}}}, ""},
		{"the actor's worker missing", []*ateapi.ListWorkersResponse{{Workers: []*ateapi.Worker{worker("w1", "microvm")}}}, "w9"},
	} {
		r := &run{c: workersServer(t, c.pages), ranOn: c.ranOn}
		if _, err := r.listWorkers(ctx); err == nil || errors.As(err, &d) {
			t.Errorf("%s: %v, want a failure", c.what, err)
		}
	}
}
