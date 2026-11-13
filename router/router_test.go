package router_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/guest/agent"
	"github.com/everdict/herd/guest/guestclient"
	"github.com/everdict/herd/router"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func ctxT(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func wantCode(t *testing.T, what string, err error, c codes.Code) {
	t.Helper()
	if got := status.Code(err); got != c {
		t.Errorf("%s: code %v (%v), want %v", what, got, err, c)
	}
}

// forge is tok with one byte in the middle of its signature flipped: a signature that decodes to other bytes. Changing
// the last base64url characters is not one, since their low bits are padding a decoder may ignore.
func forge(t *testing.T, tok string) string {
	t.Helper()
	i := strings.LastIndex(tok, ".")
	sig, err := base64.RawURLEncoding.DecodeString(tok[i+1:])
	if err != nil || len(sig) == 0 {
		t.Fatalf("token signature: %v", err)
	}
	sig[len(sig)/2] ^= 0xff
	return tok[:i+1] + base64.RawURLEncoding.EncodeToString(sig)
}

func listDir(ctx context.Context, c *client) (string, error) {
	return c.invoke(ctx, agent.MethodListDirectory, &herdext.ListDirectoryRequest{Path: "/workspace"}, new(herdext.ListDirectoryResponse))
}

// TestRouteByName (route-by-name): a client names "<atespace>/<actor>" in ate-target-actor; the router admits it only
// with a token that may write in that atespace, and reaches the actor only through the owning herdlet's tunnel.
func TestRouteByName(t *testing.T) {
	s := startStack(t, stackOpts{budget: time.Second})
	ctx := ctxT(t, 30*time.Second)
	s.createActor(space1, "a1")
	s.createActor(space2, "b1")
	tok := s.clientToken()

	before := s.tun.Opened()
	c := s.client(space1+"/a1", tok)
	p, _, err := c.start(ctx, "echo routed", false)
	if err != nil {
		t.Fatalf("StartProcess through the router: %v", err)
	}
	res := c.follow(ctx, p.GetProcessId(), 0, nil)
	if res.err != nil || res.stdout != "routed\n" || res.exit.GetExitCode() != 0 {
		t.Fatalf("follow = %+v", res)
	}
	if s.tun.Opened() <= before {
		t.Errorf("the request did not travel through the herdlet's tunnel (opened %d → %d)", before, s.tun.Opened())
	}

	// The same token naming an actor of another atespace is refused before anything reaches the node.
	before, dials := s.tun.Opened(), s.dials.Load()
	other := s.client(space2+"/b1", tok)
	reason, err := listDir(ctx, other)
	wantCode(t, "a space-1 token reaching space-2/b1", err, codes.PermissionDenied)
	if reason != string(router.ReasonPermissionDenied) {
		t.Errorf("herd-reason = %q, want %q", reason, router.ReasonPermissionDenied)
	}
	if s.tun.Opened() != before || s.dials.Load() != dials {
		t.Errorf("a refused request reached the tunnel (opened %d → %d, dials %d → %d)", before, s.tun.Opened(), dials, s.dials.Load())
	}

	for _, tc := range []struct {
		name, target, token string
		code                codes.Code
		reason              router.Reason
	}{
		{"no token", space1 + "/a1", "", codes.Unauthenticated, router.ReasonUnauthenticated},
		{"forged token", space1 + "/a1", forge(t, tok), codes.Unauthenticated, router.ReasonUnauthenticated},
		{"read-only token", space1 + "/a1", s.token(map[string]auth.Role{space1: auth.RoleRead}, false, "ro"), codes.PermissionDenied, router.ReasonPermissionDenied},
		{"herdlet credential", space1 + "/a1", s.token(map[string]auth.Role{auth.AllSpaces: auth.RoleWrite}, true, "h1"), codes.PermissionDenied, router.ReasonPermissionDenied},
		{"no atespace", "a1", tok, codes.InvalidArgument, router.ReasonBadTarget},
		{"unknown actor", space1 + "/nope", tok, codes.NotFound, router.ReasonActorNotFound},
	} {
		reason, err := listDir(ctx, s.client(tc.target, tc.token))
		wantCode(t, tc.name, err, tc.code)
		if reason != string(tc.reason) {
			t.Errorf("%s: herd-reason %q, want %q", tc.name, reason, tc.reason)
		}
	}
	if s.tun.Opened() != before {
		t.Errorf("refused requests reached the tunnel (opened %d → %d)", before, s.tun.Opened())
	}

	// CONNECT reaches another port named in the authority: here herd-guest's port, spoken to as gRPC inside it.
	conn := connect(t, s, space1+"/a1", tok, fmt.Sprintf("a1.%s:%d", space1, 8080))
	gc, err := guestclient.New(func(context.Context) (net.Conn, error) { return conn, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer gc.Close()
	if err := gc.Invoke(ctx, agent.MethodListDirectory, &herdext.ListDirectoryRequest{Path: "/workspace"}, new(herdext.ListDirectoryResponse), nil); err != nil {
		t.Errorf("gRPC inside CONNECT: %v", err)
	}
	// CONNECT to another space's actor is refused like any request.
	if code := connectStatus(t, s, space2+"/b1", tok, "b1:8080"); code != http.StatusForbidden {
		t.Errorf("CONNECT to space-2/b1 with a space-1 token: HTTP %d, want 403", code)
	}
}

// connect opens an HTTP/1.1 CONNECT through the router and returns the tunnelled connection.
func connect(t *testing.T, s *stack, target, tok, authority string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", s.routerAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s: %s\r\nAuthorization: Bearer %s\r\n\r\n", authority, authority, router.TargetActorHeader, target, tok)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("CONNECT %s: %s %s", authority, resp.Status, b)
	}
	// The tunnelled server may speak first (an HTTP/2 server sends its SETTINGS at once), and those bytes can arrive in
	// the same read as the CONNECT response: they belong to the tunnel, so hand them on instead of dropping them.
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: c, r: br}
	}
	return c
}

// bufferedConn is a connection whose first bytes were already read into r.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func connectStatus(t *testing.T, s *stack, target, tok, authority string) int {
	t.Helper()
	c, err := net.Dial("tcp", s.routerAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s: %s\r\nAuthorization: Bearer %s\r\n\r\n", authority, authority, router.TargetActorHeader, target, tok)
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

// TestRouterNeverDialsNodes (route-by-name): the router's packages hold no dialer — every connection to a VM is one
// the herdlet opened — and with the herdlet's tunnel down a running actor is unreachable even though its guest
// still listens on the node. herd-router's main is deploy.Router (deploy/router.go).
func TestRouterNeverDialsNodes(t *testing.T) {
	fset := token.NewFileSet()
	newClients := 0
	for _, dir := range []string{"../router", "../tunnel", "../cmd/herd-router", "../deploy/router.go"} {
		pkgs, err := parseSources(fset, dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pkgs {
			for name, f := range p.Files {
				ast.Inspect(f, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok && strings.HasSuffix(dir, "deploy/router.go") {
						// deploy's dial: a gRPC client over TLS as herd-router (ADR 0001 A-60).
						if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "dial" {
							newClients++ // the connection to herd-api
						}
					}
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					x, ok := sel.X.(*ast.Ident)
					if !ok {
						return true
					}
					call := x.Name + "." + sel.Sel.Name
					switch {
					case call == "grpc.NewClient" && strings.HasSuffix(dir, "deploy/router.go"):
						newClients++ // the connection to herd-api
					case strings.HasPrefix(sel.Sel.Name, "Dial") && x.Name != "tunnel", call == "net.Dialer", call == "grpc.NewClient",
						call == "http.Client", call == "http.Get", call == "http.Post", call == "http.DefaultTransport":
						t.Errorf("%s: %s at %v — the router must not dial a node", filepath.Base(name), call, fset.Position(sel.Pos()))
					}
					return true
				})
			}
		}
	}
	if newClients != 1 {
		t.Errorf("herd-router (deploy/router.go) makes %d gRPC clients, want exactly one (herd-api)", newClients)
	}

	s := startStack(t, stackOpts{budget: 500 * time.Millisecond})
	ctx := ctxT(t, 30*time.Second)
	a := s.createActor(space1, "a1")
	c := s.client(space1+"/a1", s.clientToken())
	if _, err := listDir(ctx, c); err != nil {
		t.Fatalf("with the tunnel up: %v", err)
	}
	s.stopTunnel()
	<-s.tunnelDone
	if g, err := s.vms.DialGuest(ctx, a.GetMetadata().GetUid(), 8080); err != nil {
		t.Fatalf("the guest should still listen on the node: %v", err)
	} else {
		g.Close()
	}
	reason, err := listDir(ctx, c)
	wantCode(t, "with the herdlet's tunnel down", err, codes.Unavailable)
	if reason != string(router.ReasonNodeUnreachable) {
		t.Errorf("herd-reason %q (%q), want %s", reason, status.Convert(err).Message(), router.ReasonNodeUnreachable)
	}
}

// TestNodeUnreachableWhenHerdAPIIsSlow (route-by-name): the reason of a budget-exceeded 503 does not depend on
// herd-api answering inside the budget. With the actor's worker holding no tunnel and herd-api slower than the
// budget, the answer is node-unreachable, with herd-api's error as detail.
func TestNodeUnreachableWhenHerdAPIIsSlow(t *testing.T) {
	const budget = 300 * time.Millisecond
	s := startStack(t, stackOpts{budget: budget})
	ctx := ctxT(t, 30*time.Second)
	s.createActor(space1, "a1")
	c := s.client(space1+"/a1", s.clientToken())
	if _, err := listDir(ctx, c); err != nil {
		t.Fatalf("with the tunnel up: %v", err)
	}
	s.stopTunnel()
	<-s.tunnelDone
	s.apiDelay.Store(int64(3 * budget))
	for i := 0; i < 3; i++ {
		start := time.Now()
		reason, err := listDir(ctx, c)
		took := time.Since(start)
		wantCode(t, "herd-api slower than the budget, tunnel down", err, codes.Unavailable)
		msg := status.Convert(err).Message()
		if reason != string(router.ReasonNodeUnreachable) || !strings.Contains(msg, "h1") || !strings.Contains(msg, "herd-api") {
			t.Errorf("try %d: herd-reason %q, message %q: want %s naming worker h1 and herd-api's error", i, reason, msg, router.ReasonNodeUnreachable)
		}
		if took > budget+time.Second {
			t.Errorf("try %d: answered after %v, budget %v", i, took, budget)
		}
	}
	s.apiDelay.Store(0)
}

// TestResumeOnRequest (resume-on-request): requests to a SUSPENDED actor are parked, one resume is asked of
// herd-api, and every parked request is delivered once the actor runs — within the parking budget. A resume that
// cannot finish in the budget is answered with a typed 503, neither at once nor never.
func TestResumeOnRequest(t *testing.T) {
	const budget = 3 * time.Second
	s := startStack(t, stackOpts{budget: budget})
	ctx := ctxT(t, 60*time.Second)
	s.createActor(space1, "a1")
	if err := s.suspend(space1, "a1"); err != nil {
		t.Fatal(err)
	}
	if st := s.state(space1, "a1"); st != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("after suspend: %v", st)
	}
	c := s.client(space1+"/a1", s.clientToken())
	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = listDir(ctx, c)
		}()
	}
	wg.Wait()
	took := time.Since(start)
	for i, err := range errs {
		if err != nil {
			t.Errorf("parked request %d: %v", i, err)
		}
	}
	if took > budget {
		t.Errorf("delivery took %v, past the parking budget %v", took, budget)
	}
	if st := s.state(space1, "a1"); st != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("after the request: %v, want RUNNING", st)
	}
	if n := s.resumes.Load(); n != 1 {
		t.Errorf("the router asked herd-api for %d resumes, want 1 shared by the parked requests", n)
	}
	t.Logf("5 parked requests delivered in %v (budget %v)", took, budget)

	// A resume that cannot finish: the restore hangs on the node.
	if err := s.suspend(space1, "a1"); err != nil {
		t.Fatal(err)
	}
	s.vms.Hang(vmm.OpRestore)
	start = time.Now()
	reason, err := listDir(ctx, c)
	took = time.Since(start)
	wantCode(t, "a request past the parking budget", err, codes.Unavailable)
	if reason != string(router.ReasonParkingBudget) {
		t.Errorf("herd-reason %q, want %q", reason, router.ReasonParkingBudget)
	}
	if took < budget || took > budget+2*time.Second {
		t.Errorf("answered after %v, want the parking budget %v", took, budget)
	}
	// A plain HTTP client gets the same as an HTTP 503 with the reason header.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.routerAddr+"/", nil)
	req.Header.Set(router.TargetActorHeader, space1+"/a1")
	req.Header.Set("Authorization", "Bearer "+s.clientToken())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get(router.ReasonHeader) != string(router.ReasonParkingBudget) {
		t.Errorf("plain HTTP: %d %s %q", resp.StatusCode, resp.Header.Get(router.ReasonHeader), body)
	}
	s.vms.Clear()
}

// TestLongStreams (long-streams): a followed exec and a terminal stay open far past every timeout the router has; a
// suspend cuts them with a typed reason, and the client re-attaches by exec id from its offset after the resume.
func TestLongStreams(t *testing.T) {
	const budget = 300 * time.Millisecond
	s := startStack(t, stackOpts{budget: budget})
	ctx := ctxT(t, 60*time.Second)
	s.createActor(space1, "a1")
	c := s.client(space1+"/a1", s.clientToken())

	// A followed exec silent for five parking budgets.
	p, _, err := c.start(ctx, "sleep 1.5; echo late", false)
	if err != nil {
		t.Fatal(err)
	}
	res := c.follow(ctx, p.GetProcessId(), 0, nil)
	if res.err != nil || res.stdout != "late\n" || res.exit == nil {
		t.Errorf("a silent followed exec: %+v", res)
	}

	// A terminal: stdin and output streams open together, idle in between.
	p, _, err = c.start(ctx, "cat", true)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 16)
	followed := make(chan followResult, 1)
	go func() { followed <- c.follow(ctx, p.GetProcessId(), 0, func(s string) { got <- s }) }()
	in, err := c.conn.NewStream(c.ctx(ctx), agent.MethodWriteProcessInput, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range []string{"hello\n", "again\n"} {
		time.Sleep(4 * budget)
		req := &ateenv.WriteProcessInputRequest{Data: []byte(line)}
		if i == 0 {
			req.ProcessId = p.GetProcessId()
		}
		if err := in.Send(req); err != nil {
			t.Fatalf("stdin after %v idle: %v", 4*budget, err)
		}
		select {
		case out := <-got:
			if out != line {
				t.Errorf("terminal echoed %q, want %q", out, line)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("terminal: no echo of %q", line)
		}
	}
	if err := in.Send(&ateenv.WriteProcessInputRequest{Close: true}); err != nil {
		t.Fatal(err)
	}
	_ = in.CloseSend()
	if err := in.Recv(new(ateenv.WriteProcessInputResponse)); err != nil {
		t.Errorf("WriteProcessInput: %v", err)
	}
	if r := <-followed; r.err != nil || r.exit == nil {
		t.Errorf("terminal output stream: %+v", r)
	}

	// A suspend cuts a followed exec with a typed reason; the client re-attaches from its offset.
	p, _, err = c.start(ctx, "echo one; sleep 1.5; echo two", false)
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan struct{})
	var once sync.Once
	go func() {
		<-first
		if err := s.suspend(space1, "a1"); err != nil {
			t.Errorf("suspend: %v", err)
		}
	}()
	start := time.Now()
	cut := c.follow(ctx, p.GetProcessId(), 0, func(string) { once.Do(func() { close(first) }) })
	if status.Code(cut.err) != codes.Unavailable || cut.reason != string(router.ReasonSuspended) || cut.stdout != "one\n" {
		t.Fatalf("stream cut by suspend: stdout %q, code %v (%v), herd-reason %q; want %q, Unavailable, %q",
			cut.stdout, status.Code(cut.err), cut.err, cut.reason, "one\n", router.ReasonSuspended)
	}
	if took := time.Since(start); took > 1400*time.Millisecond {
		t.Errorf("the cut took %v: the client was left hanging", took)
	}
	t.Logf("cut: %v", cut.err)
	for s.state(space1, "a1") != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		time.Sleep(10 * time.Millisecond)
	}
	// Re-attach by exec id: the request resumes the actor (budget 300 ms) and the guest follows from the offset.
	again := c.follow(ctx, p.GetProcessId(), int64(len(cut.stdout)), nil)
	if again.err != nil || again.stdout != "two\n" || again.exit.GetExitCode() != 0 {
		t.Errorf("re-attach: %+v", again)
	}
}

// parseSources parses the non-test Go files of a directory, or one Go file.
func parseSources(fset *token.FileSet, path string) (map[string]*ast.Package, error) {
	if strings.HasSuffix(path, ".go") {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, err
		}
		return map[string]*ast.Package{f.Name.Name: {Name: f.Name.Name, Files: map[string]*ast.File{path: f}}}, nil
	}
	return parser.ParseDir(fset, path, func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
}

// TestCrashedActorIsFailedPrecondition (PLT-230): a request to a CRASHED actor is refused at once with
// FAILED_PRECONDITION and the actor's crash message (what ended: the VM, or a container with its status and last
// output), not parked until the budget ends in UNAVAILABLE "actor is CRASHED".
func TestCrashedActorIsFailedPrecondition(t *testing.T) {
	const budget = 3 * time.Second
	s := startStack(t, stackOpts{budget: budget})
	ctx := ctxT(t, 30*time.Second)
	a := s.createActor(space1, "a1")
	c := s.client(space1+"/a1", s.clientToken())
	if _, err := listDir(ctx, c); err != nil {
		t.Fatalf("while it runs: %v", err)
	}
	s.exited.Store(a.GetMetadata().GetUid(), true)
	deadline := time.Now().Add(10 * time.Second)
	for s.state(space1, "a1") != ateapi.ActorState_ACTOR_STATE_CRASHED {
		if time.Now().After(deadline) {
			t.Fatalf("actor is %v, want CRASHED", s.state(space1, "a1"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	reason, err := listDir(ctx, c)
	wantCode(t, "a request to a CRASHED actor", err, codes.FailedPrecondition)
	if msg := status.Convert(err).Message(); reason != string(router.ReasonActorCrashed) || !strings.Contains(msg, "CRASHED") ||
		!strings.Contains(msg, "powered off") {
		t.Errorf("herd-reason %q, message %q: want %s with the crash message", reason, msg, router.ReasonActorCrashed)
	}
	if took := time.Since(start); took > budget/2 {
		t.Errorf("answered after %v: a CRASHED actor was parked", took)
	}
}
