package router_test

import (
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	"github.com/everdict/herd/guest/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// hops is what each hop of a request counted: router → tunnel dials, herdlet tunnel streams, herdlet → guest dials.
type hops struct{ dials, opened, guest int64 }

func (s *stack) hops() hops { return hops{s.dials.Load(), s.tun.Opened(), s.guestDials.Load()} }

// TestRouterE2E (router-e2e): herd-api, herd-router and a herdlet whose fake VMs run the real herd-guest on unix
// sockets, each on its own connection. Create an actor, stream an exec through the router, suspend it (the stream is
// cut with a reason), send a request again (it resumes the actor), and re-attach to the exec by id. Every request
// crosses every hop: the router's tunnel dial, the herdlet's tunnel stream and the herdlet's guest dial.
func TestRouterE2E(t *testing.T) {
	s := startStack(t, stackOpts{budget: 5 * time.Second})
	ctx := ctxT(t, 60*time.Second)
	a := s.createActor(space1, "e2e")
	if a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("created actor is %v", a.GetStatus().GetState())
	}
	c := s.client(space1+"/e2e", s.clientToken())
	requests := int64(0)
	check := func(step string, before hops) {
		t.Helper()
		requests++
		after := s.hops()
		if after.dials != before.dials+1 || after.opened != before.opened+1 || after.guest != before.guest+1 {
			t.Errorf("%s: hops %+v → %+v: a request must cross router dial, herdlet tunnel and guest dial once each", step, before, after)
		}
	}

	h := s.hops()
	p, _, err := c.start(ctx, "echo started; sleep 1; echo middle; sleep 1; echo done", false)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	check("start", h)

	h = s.hops()
	suspended := make(chan error, 1)
	var asked bool
	cut := c.follow(ctx, p.GetProcessId(), 0, func(out string) {
		if !asked && out == "started\n" {
			asked = true
			go func() { suspended <- s.suspend(space1, "e2e") }()
		}
	})
	check("follow", h)
	if err := <-suspended; err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	if status.Code(cut.err) != codes.Unavailable || cut.reason != "actor-suspended" {
		t.Fatalf("stream during suspend ended with %v, herd-reason %q", cut.err, cut.reason)
	}
	if st := s.state(space1, "e2e"); st != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("after suspend: %v", st)
	}

	// A request to the suspended actor resumes it.
	h = s.hops()
	resumesBefore := s.resumes.Load()
	if _, err := listDir(ctx, c); err != nil {
		t.Fatalf("request to the suspended actor: %v", err)
	}
	check("auto-resume", h)
	if st := s.state(space1, "e2e"); st != ateapi.ActorState_ACTOR_STATE_RUNNING || s.resumes.Load() != resumesBefore+1 {
		t.Fatalf("after the request: %v, resumes %d → %d", st, resumesBefore, s.resumes.Load())
	}

	// Re-attach to the exec by id from the offset the client had.
	h = s.hops()
	rest := c.follow(ctx, p.GetProcessId(), int64(len(cut.stdout)), nil)
	check("re-attach", h)
	if rest.err != nil || cut.stdout+rest.stdout != "started\nmiddle\ndone\n" || rest.exit.GetExitCode() != 0 {
		t.Errorf("re-attached: %q + %q, exit %v, err %v", cut.stdout, rest.stdout, rest.exit, rest.err)
	}
	t.Logf("%d requests, hops %+v", requests, s.hops())
}

// TestGuestRefusalPassesThrough: a guest's own typed refusal reaches the client as the guest answered it, on a
// unary and on a streaming call: a trailers-only answer stays trailers-only through the router.
func TestGuestRefusalPassesThrough(t *testing.T) {
	s := startStack(t, stackOpts{budget: 5 * time.Second})
	ctx := ctxT(t, 30*time.Second)
	s.createActor(space1, "r1")
	c := s.client(space1+"/r1", s.clientToken())
	_, err := c.invoke(ctx, agent.MethodGetProcess, &ateenv.GetProcessRequest{ProcessId: "no-such-exec"}, new(ateenv.Process))
	wantCode(t, "unary GetProcess of an unknown exec", err, codes.NotFound)
	cs, err := c.conn.ClientConn().NewStream(c.ctx(ctx), &grpc.StreamDesc{ServerStreams: true}, agent.MethodReadFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.SendMsg(&ateenv.ReadFileRequest{Path: "/no-such-file"}); err != nil {
		t.Fatal(err)
	}
	_ = cs.CloseSend()
	err = cs.RecvMsg(new(ateenv.ReadFileResponse))
	wantCode(t, "streaming ReadFile of a missing file", err, codes.NotFound)
}
