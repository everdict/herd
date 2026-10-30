package conformance

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/everdict/herd/apiserver"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/herdlet"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/snapstore/disk"
	"github.com/everdict/herd/store"
	"github.com/everdict/herd/store/memstore"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// HerdletSpec describes one fake herdlet.
type HerdletSpec struct {
	Name   string
	Slots  int
	Labels map[string]string
	Caps   vmm.Capabilities
	// Configure, if set, finishes the herdlet before it dials (e.g. gives it Machines over its VMM).
	Configure func(*herdlet.Agent)
}

// Herdlet is a running fake herdlet.
type Herdlet struct {
	Spec HerdletSpec
	// VMM is its hypervisor: Hang, Unreachable, LeaveOnDelete, Clear.
	VMM *fake.Fake

	mu      sync.Mutex
	rewrite func(link.Work, link.Outcome) (link.Outcome, bool)
	cancel  context.CancelFunc
	done    chan struct{}
}

// Rewrite makes the herdlet replace its answers (return false to stay silent); nil restores honest answers.
func (h *Herdlet) Rewrite(f func(link.Work, link.Outcome) (link.Outcome, bool)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rewrite = f
}

func (h *Herdlet) answer(w link.Work, o link.Outcome) (link.Outcome, bool) {
	h.mu.Lock()
	f := h.rewrite
	h.mu.Unlock()
	if f == nil {
		return o, true
	}
	return f(w, o)
}

// Disconnect ends the herdlet's link and waits for it to stop.
func (h *Herdlet) Disconnect() {
	h.cancel()
	<-h.done
}

// Env is herd-api with fake herdlets, reachable over gRPC on a loopback port.
type Env struct {
	Addr      string
	Space     string
	Server    *apiserver.Server
	Hub       *link.Hub
	Store     store.Store
	Snapshots snapstore.Store
	Key       []byte
	// Client may write in Space.
	Client   *Client
	Herdlets map[string]*Herdlet

	vms *fake.Store
	ctx context.Context
}

// NewStore makes the record store each Env runs on; tests may swap it (Envs are not started in parallel).
var NewStore = func(t testing.TB) store.Store { return memstore.New() }

// CallTimeout is how long herd-api waits for a herdlet in the harness.
const CallTimeout = 400 * time.Millisecond

// Start runs herd-api and the given herdlets; everything stops when the test ends.
func Start(t testing.TB, herdlets ...HerdletSpec) *Env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	snaps, err := disk.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &Env{Space: "space-1", Hub: link.NewHub(), Store: NewStore(t), Snapshots: snaps, Key: key,
		Herdlets: map[string]*Herdlet{}, vms: fake.NewStore(), ctx: ctx}
	e.Server, err = apiserver.New(apiserver.Config{
		Store: e.Store, Snapshots: snaps, Hub: e.Hub, Authn: auth.StaticKey{Key: key}, Authz: auth.SpaceRoles{},
		CallTimeout: CallTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	e.Server.Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.Addr = ln.Addr().String()
	served := make(chan struct{})
	go func() { defer close(served); _ = g.Serve(ln) }()
	e.Client = NewClient(e.Addr, e.Token(t, map[string]auth.Role{e.Space: auth.RoleWrite}, false))
	t.Cleanup(func() {
		for _, h := range e.Herdlets {
			h.Disconnect()
		}
		e.Server.Wait()
		e.Client.Close()
		cancel()
		g.Stop()
		<-served
		_ = e.Store.Close()
	})
	for _, s := range herdlets {
		e.AddHerdlet(t, s)
	}
	return e
}

// Token mints a bearer token for the given spaces.
func (e *Env) Token(t testing.TB, spaces map[string]auth.Role, herdletCred bool) string {
	t.Helper()
	return e.TokenFor(t, fmt.Sprintf("test-%d", len(spaces)), spaces, herdletCred)
}

// TokenFor mints a bearer token for subject (a herdlet credential names its worker).
func (e *Env) TokenFor(t testing.TB, subject string, spaces map[string]auth.Role, herdletCred bool) string {
	t.Helper()
	tok, err := auth.Sign(e.Key, auth.Claims{Subject: subject, ExpiresAt: time.Now().Add(time.Hour).Unix(), Spaces: spaces, Herdlet: herdletCred})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// AddHerdlet starts a fake herdlet that dials herd-api, and waits until herd-api has its registration.
func (e *Env) AddHerdlet(t testing.TB, s HerdletSpec) *Herdlet {
	t.Helper()
	if s.Caps.Platform == "" {
		s.Caps = vmm.Capabilities{Platform: vmm.PlatformCloudHypervisor, Arch: "amd64", SuspendMaxFidelity: vmm.FidelityMemory, RestoreFamily: "microvm-ch-amd64-tigerlake"}
	}
	ctx, cancel := context.WithCancel(e.ctx)
	h := &Herdlet{Spec: s, VMM: fake.New(s.Caps, e.vms), cancel: cancel, done: make(chan struct{})}
	a := &herdlet.Agent{Worker: s.Name, Class: apiserver.SandboxClass, Labels: s.Labels, Slots: s.Slots, VMM: h.VMM,
		Snapshots: e.Snapshots, Rewrite: h.answer}
	if s.Configure != nil {
		s.Configure(a)
	}
	c, err := grpc.NewClient("passthrough:///"+e.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(auth.Bearer(e.TokenFor(t, s.Name, nil, true))))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(h.done)
		defer c.Close()
		_ = a.Run(ctx, c)
	}()
	e.Herdlets[s.Name] = h
	deadline := time.Now().Add(5 * time.Second)
	for !e.connected(s.Name) {
		if time.Now().After(deadline) {
			t.Fatalf("herdlet %s did not register", s.Name)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return h
}

func (e *Env) connected(name string) bool {
	for _, w := range e.Hub.Workers() {
		if w.Worker == name {
			return true
		}
	}
	return false
}
