package router_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	"github.com/everdict/herd/apiserver"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/guest/agent"
	"github.com/everdict/herd/guest/execrec"
	"github.com/everdict/herd/guest/guestclient"
	"github.com/everdict/herd/herdlet"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/router"
	"github.com/everdict/herd/snapstore/disk"
	"github.com/everdict/herd/store/memstore"
	"github.com/everdict/herd/tunnel"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
	"github.com/everdict/herd/vmm/fakeguest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// The test binary doubles as herd-guest's in-container shim.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "shim" {
		if err := execrec.Shim(os.Args[2]); err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const (
	space1 = "space-1"
	space2 = "space-2"
)

// stack is herd-api + herd-router + one herdlet whose fake VMs run the real herd-guest, each on its own loopback
// port or socket: the client knows only the router, the router dials only herd-api, the herdlet dials both.
type stack struct {
	t          *testing.T
	key        []byte
	api        *conformance.Client // writes in both spaces
	apiAddr    string
	routerAddr string
	tunnelAddr string
	rt         *router.Router
	tunnels    *tunnel.Server
	vms        *fakeguest.VMM
	tun        *herdlet.Tunnel
	dials      atomic.Int64 // tunnel dials the router made
	resumes    atomic.Int64 // ResumeActor calls the router made
	guestDials atomic.Int64 // guest connections the herdlet made
	stopTunnel context.CancelFunc
	tunnelDone chan struct{}
	ctx        context.Context
}

type stackOpts struct {
	budget time.Duration
}

// countingTunnels counts the router's tunnel dials.
type countingTunnels struct {
	s *stack
	t *tunnel.Server
}

func (c countingTunnels) Dial(ctx context.Context, worker, vm string, port uint32) (net.Conn, error) {
	c.s.dials.Add(1)
	return c.t.Dial(ctx, worker, vm, port)
}

// countingGuests counts the herdlet's guest connections.
type countingGuests struct{ s *stack }

func (c countingGuests) DialGuest(ctx context.Context, vm string, port uint32) (net.Conn, error) {
	c.s.guestDials.Add(1)
	return c.s.vms.DialGuest(ctx, vm, port)
}

// countingControl counts the router's resumes.
type countingControl struct {
	s *stack
	c router.Control
}

func (c countingControl) GetActor(ctx context.Context, token, space, name string) (*ateapi.Actor, error) {
	return c.c.GetActor(ctx, token, space, name)
}

func (c countingControl) ResumeActor(ctx context.Context, token, space, name string) error {
	c.s.resumes.Add(1)
	return c.c.ResumeActor(ctx, token, space, name)
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func startStack(t *testing.T, o stackOpts) *stack {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &stack{t: t, key: make([]byte, 32), ctx: ctx}
	if _, err := rand.Read(s.key); err != nil {
		t.Fatal(err)
	}
	authn := auth.StaticKey{Key: s.key}

	// herd-api
	snaps, err := disk.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hub := link.NewHub()
	api, err := apiserver.New(apiserver.Config{Store: memstore.New(), Snapshots: snaps, Hub: hub, Authn: authn, Authz: auth.SpaceRoles{},
		CallTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ag := grpc.NewServer()
	api.Register(ag)
	al := listen(t)
	s.apiAddr = al.Addr().String()
	go func() { _ = ag.Serve(al) }()

	// herd-router: a gRPC server for herdlets' tunnels, an HTTP server for clients, a client of herd-api.
	s.tunnels = &tunnel.Server{Authn: authn, OpenTimeout: 2 * time.Second}
	tg := grpc.NewServer()
	s.tunnels.Register(tg)
	tl := listen(t)
	s.tunnelAddr = tl.Addr().String()
	go func() { _ = tg.Serve(tl) }()
	apiCC, err := grpc.NewClient("passthrough:///"+s.apiAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	if o.budget == 0 {
		o.budget = 5 * time.Second
	}
	s.rt, err = router.New(router.Config{
		Control: countingControl{s, router.GRPCControl{CC: apiCC}}, Tunnels: countingTunnels{s, s.tunnels},
		Authn: authn, Authz: auth.SpaceRoles{}, DefaultPort: fakeguest.GuestPort, ParkingBudget: o.budget, PollInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	hs := router.NewHTTPServer(s.rt)
	rl := listen(t)
	s.routerAddr = rl.Addr().String()
	go func() { _ = hs.Serve(rl) }()

	// herdlet h1: dials herd-api (link) and herd-router (tunnel).
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s.vms = fakeguest.New(fake.New(vmm.Capabilities{Platform: vmm.PlatformCloudHypervisor, Arch: "amd64",
		SuspendMaxFidelity: vmm.FidelityMemory, RestoreFamily: "microvm-ch-amd64-test"}, nil), t.TempDir(), []string{self, "shim"})
	s.tun = &herdlet.Tunnel{Worker: "h1", Guests: countingGuests{s}}
	agent := &herdlet.Agent{Worker: "h1", Class: apiserver.SandboxClass, Slots: 8, VMM: s.vms, Snapshots: snaps, Tunnel: s.tun}
	herdletCred := grpc.WithPerRPCCredentials(auth.Bearer(s.token(nil, true, "h1")))
	linkCC, err := grpc.NewClient("passthrough:///"+s.apiAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), herdletCred)
	if err != nil {
		t.Fatal(err)
	}
	linkDone := make(chan struct{})
	go func() { defer close(linkDone); _ = agent.Run(ctx, linkCC) }()
	s.startTunnel(herdletCred)

	s.api = conformance.NewClient(s.apiAddr, s.token(map[string]auth.Role{space1: auth.RoleWrite, space2: auth.RoleWrite}, false, "admin"))
	t.Cleanup(func() {
		s.api.Close()
		_ = hs.Close()
		s.stopTunnel()
		<-s.tunnelDone
		cancel()
		<-linkDone
		s.rt.Wait()
		api.Wait()
		s.vms.Close()
		tg.Stop()
		ag.Stop()
		linkCC.Close()
		apiCC.Close()
	})
	deadline := time.Now().Add(5 * time.Second)
	for len(hub.Workers()) == 0 || !s.tunnels.Connected("h1") {
		if time.Now().After(deadline) {
			t.Fatal("herdlet did not connect to herd-api and herd-router")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, sp := range []string{space1, space2} {
		if _, err := s.api.CreateActorTemplate(s.ctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
			Metadata:       &ateapi.ResourceMetadata{Atespace: sp, Name: "tpl"},
			Containers:     []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
			SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// startTunnel runs the herdlet's tunnel to the router; stopTunnel ends it.
func (s *stack) startTunnel(cred grpc.DialOption) {
	cc, err := grpc.NewClient("passthrough:///"+s.tunnelAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), cred)
	if err != nil {
		s.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cc.Close()
		_ = s.tun.Run(ctx, cc)
	}()
	s.stopTunnel, s.tunnelDone = cancel, done
}

func (s *stack) token(spaces map[string]auth.Role, herdletCred bool, sub string) string {
	s.t.Helper()
	tok, err := auth.Sign(s.key, auth.Claims{Subject: sub, ExpiresAt: time.Now().Add(time.Hour).Unix(), Spaces: spaces, Herdlet: herdletCred})
	if err != nil {
		s.t.Fatal(err)
	}
	return tok
}

// clientToken may write in space-1 only.
func (s *stack) clientToken() string {
	return s.token(map[string]auth.Role{space1: auth.RoleWrite}, false, "client")
}

func (s *stack) createActor(space, name string) *ateapi.Actor {
	s.t.Helper()
	a, err := s.api.CreateActor(s.ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: name}, ActorTemplate: &ateapi.ObjectRef{Atespace: space, Name: "tpl"}}})
	if err != nil {
		s.t.Fatalf("CreateActor %s/%s: %v", space, name, err)
	}
	return a
}

func (s *stack) state(space, name string) ateapi.ActorState {
	s.t.Helper()
	a, err := s.api.GetActor(s.ctx, &ateapi.GetActorRequest{Actor: &ateapi.ObjectRef{Atespace: space, Name: name}})
	if err != nil {
		s.t.Fatalf("GetActor: %v", err)
	}
	return a.GetStatus().GetState()
}

func (s *stack) suspend(space, name string) error {
	_, err := s.api.SuspendActor(s.ctx, &ateapi.SuspendActorRequest{Actor: &ateapi.ObjectRef{Atespace: space, Name: name}})
	return err
}

// client is a gRPC client of herd-guest's API that knows only the router's address.
type client struct {
	conn  *guestclient.Conn
	md    map[string]string
	stack *stack
}

func (s *stack) client(target, token string) *client {
	s.t.Helper()
	addr := s.routerAddr
	c, err := guestclient.New(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	})
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { c.Close() })
	md := map[string]string{router.TargetActorHeader: target}
	if token != "" {
		md["authorization"] = "Bearer " + token
	}
	return &client{conn: c, md: md, stack: s}
}

func (c *client) ctx(ctx context.Context) context.Context {
	return metadata.NewOutgoingContext(ctx, metadata.New(c.md))
}

// invoke makes a unary call and returns the herd-reason the router put on the answer, if any.
func (c *client) invoke(ctx context.Context, method string, req, resp any) (string, error) {
	var hdr, tr metadata.MD
	err := c.conn.ClientConn().Invoke(c.ctx(ctx), method, req, resp, grpc.Header(&hdr), grpc.Trailer(&tr))
	return reasonOf(hdr, tr), err
}

func reasonOf(mds ...metadata.MD) string {
	for _, md := range mds {
		if v := md.Get(router.ReasonHeader); len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

func (c *client) start(ctx context.Context, cmd string, stdin bool) (*ateenv.Process, string, error) {
	p := new(ateenv.Process)
	r, err := c.invoke(ctx, agent.MethodStartProcess, &ateenv.StartProcessRequest{Command: []string{"/bin/sh", "-c", cmd}, Stdin: stdin}, p)
	return p, r, err
}

// follow streams an exec's output from offsets. onOut sees each stdout chunk. It returns the stdout received, the
// exit (nil if the stream ended without one), the herd-reason trailer and the stream's error.
type followResult struct {
	stdout string
	exit   *ateenv.Process
	reason string
	err    error
}

func (c *client) follow(ctx context.Context, id string, outOff int64, onOut func(string)) followResult {
	ctx, cancel := context.WithCancel(c.ctx(ctx))
	defer cancel()
	cs, err := c.conn.ClientConn().NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, agent.MethodStreamProcessOutput)
	if err != nil {
		return followResult{err: err}
	}
	var res followResult
	if err := cs.SendMsg(&ateenv.StreamProcessOutputRequest{ProcessId: id, StdoutOffset: outOff, Follow: true}); err != nil {
		res.err = err
		return res
	}
	_ = cs.CloseSend()
	var b strings.Builder
	for {
		m := new(ateenv.ProcessOutput)
		err := cs.RecvMsg(m)
		if err != nil {
			hdr, _ := cs.Header()
			res.stdout, res.reason = b.String(), reasonOf(hdr, cs.Trailer())
			if !errors.Is(err, io.EOF) {
				res.err = err
			}
			return res
		}
		if out := m.GetStdout(); len(out) > 0 {
			b.Write(out)
			if onOut != nil {
				onOut(string(out))
			}
		}
		if e := m.GetExit(); e != nil {
			res.exit = e
		}
	}
}
