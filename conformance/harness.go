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
	// Backend, if set, is the herdlet's hypervisor instead of the fake (Herdlet.VMM is then nil); Caps is unused.
	Backend vmm.VMM
	// Guest is the guest build the herdlet declares (herdlet.Agent.Guest).
	Guest string
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
	agent   *herdlet.Agent
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
	// SnapshotDir is the directory the snapshot store keeps its objects in.
	SnapshotDir string
	Key         []byte
	// Client may write in Space.
	Client   *Client
	Herdlets map[string]*Herdlet

	vms  *fake.Store
	ctx  context.Context
	hang *hangStore
	api  *apiProcess
}

// NewStore makes the record store each Env runs on; tests may swap it (Envs are not started in parallel).
var NewStore = func(t testing.TB) store.Store { return memstore.New() }

// GoldenWarmup is herd-api's apiserver.Config.GoldenWarmup in the harness (tests may set it; Envs are not started in
// parallel).
var GoldenWarmup time.Duration

// GoldenRetryBackoff is herd-api's apiserver.Config.GoldenRetryBackoff in the harness (tests may set it).
var GoldenRetryBackoff time.Duration

// GoldenRebuilds is herd-api's apiserver.Config.GoldenRebuilds in the harness (tests may set it).
var GoldenRebuilds int

// CallTimeout is how long herd-api waits for a herdlet in the harness.
const CallTimeout = 400 * time.Millisecond

// ReconcileCallTimeout is how long Reconcile waits for a herdlet in the harness: a bound that a loaded host's herdlet
// (fsyncs of a snapshot, a starved scheduler) stays well inside, so one Reconcile converges what the herdlet answers.
const ReconcileCallTimeout = 5 * time.Second

// Start runs herd-api and the given herdlets; everything stops when the test ends.
func Start(t testing.TB, herdlets ...HerdletSpec) *Env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	snapDir := t.TempDir()
	snaps, err := disk.New(snapDir)
	if err != nil {
		t.Fatal(err)
	}
	// The herdlet's snapshot call is bounded by the harness's call timeout; the store's fsyncs would make it depend
	// on the host disk (p99 near 600 ms on a loaded box, past the conformance harness's 400 ms).
	snaps.NoSync = true
	hang := &hangStore{Store: snaps}
	e := &Env{Space: "space-1", Store: NewStore(t), Snapshots: hang, SnapshotDir: snapDir, Key: key,
		Herdlets: map[string]*Herdlet{}, vms: fake.NewStore(), ctx: ctx, hang: hang}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.Addr = ln.Addr().String()
	e.serve(t, ln)
	e.Client = NewClient(e.Addr, e.Token(t, map[string]auth.Role{e.Space: auth.RoleWrite}, false))
	t.Cleanup(func() {
		e.HangSnapshots(false)
		for _, h := range e.Herdlets {
			h.Disconnect()
		}
		e.Server.Close()
		e.Server.Wait()
		e.Client.Close()
		cancel()
		e.api.stop()
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
	h := &Herdlet{Spec: s, cancel: cancel, done: make(chan struct{})}
	var backend vmm.VMM = s.Backend
	if backend == nil {
		h.VMM = fake.New(s.Caps, e.vms)
		backend = h.VMM
	}
	h.agent = &herdlet.Agent{Worker: s.Name, Class: apiserver.SandboxClass, Labels: s.Labels, Slots: s.Slots, VMM: backend,
		Snapshots: e.Snapshots, Rewrite: h.answer, Guest: s.Guest}
	if s.Configure != nil {
		s.Configure(h.agent)
	}
	e.Herdlets[s.Name] = h
	e.run(t, ctx, h)
	return h
}

// run dials herd-api with h's agent and waits until herd-api has its registration.
func (e *Env) run(t testing.TB, ctx context.Context, h *Herdlet) {
	t.Helper()
	c, err := grpc.NewClient("passthrough:///"+e.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(auth.Bearer(e.TokenFor(t, h.Spec.Name, nil, true))))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(h.done)
		defer c.Close()
		_ = h.agent.Run(ctx, c)
	}()
	e.await(t, h.Spec.Name, true)
}

// await waits until the worker is (or is no longer) connected to herd-api.
func (e *Env) await(t testing.TB, name string, connected bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for e.connected(name) != connected {
		if time.Now().After(deadline) {
			t.Fatalf("herdlet %s: connected is not %v after 5s", name, connected)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Reconnect ends the herdlet's link and dials again from the same herdlet process (a link blip): its VMs and what it
// knows of them stay.
func (e *Env) Reconnect(t testing.TB, name string) *Herdlet {
	t.Helper()
	h := e.Herdlets[name]
	h.Disconnect()
	e.await(t, name, false)
	ctx, cancel := context.WithCancel(e.ctx)
	h.cancel, h.done = cancel, make(chan struct{})
	e.run(t, ctx, h)
	return h
}

// Restart ends the herdlet's process and starts a new one under the same worker name and spec. The VMs end with the
// old process (ADR A-19: a restarted herdlet does not adopt the VMs it left), so the new fake hypervisor holds none of
// them; it shares the old one's snapshot records, as a host keeps its snapshot directory. A herdlet given a Backend
// gets the same backend back, with whatever VMs it still holds.
func (e *Env) Restart(t testing.TB, name string) *Herdlet {
	t.Helper()
	old := e.Herdlets[name]
	old.Disconnect()
	e.await(t, name, false)
	s := old.Spec
	return e.AddHerdlet(t, s)
}

func (e *Env) connected(name string) bool {
	for _, w := range e.Hub.Workers() {
		if w.Worker == name {
			return true
		}
	}
	return false
}
