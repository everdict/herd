package deploy_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/deploy"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/join"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const space = "team-a"

func chCaps() vmm.Capabilities {
	return vmm.Capabilities{Platform: vmm.PlatformCloudHypervisor, Arch: "amd64", SuspendMaxFidelity: vmm.FidelityMemory, RestoreFamily: "microvm-ch-amd64-test"}
}

// registry serves the test template's image for both architectures, as a real registry would.
func registry() *imagetest.Registry {
	reg := imagetest.NewRegistry()
	var ms []imagetest.Manifest
	for _, arch := range []string{"amd64", "arm64"} {
		ms = append(ms, imagetest.Manifest{Platform: image.Platform{OS: "linux", Architecture: arch},
			Config: image.Config{Cmd: []string{"/bin/run"}},
			Layers: [][]byte{imagetest.Layer(imagetest.File{Name: "bin/run", Body: arch, Mode: 0o755})}})
	}
	reg.Push("everdict/agent", "1", ms...)
	return reg
}

// herdletOn is a herdlet configured as its flags' defaults would, on the fake hypervisor, with the image cache over a
// test registry: templates become VMs through the same machines a real host has.
func herdletOn(t *testing.T, worker string, store *fake.Store) deploy.Herdlet {
	state := filepath.Join(t.TempDir(), "state")
	return deploy.Herdlet{Worker: worker, Identity: nodeIdentity(t, worker), StateDir: state, Slots: 1, Kernel: "vmlinux",
		VCPUs: 1, MemoryMiB: 512, VMM: fake.New(chCaps(), store), Retry: 50 * time.Millisecond,
		Images: &image.Cache{Dir: filepath.Join(state, "images"), Source: registry(), Builder: &imagetest.Builder{},
			Platform: image.Platform{OS: "linux", Architecture: "amd64"}}}
}

// coldBoot is the golden snapshot config of these tests' templates: they count placements, which a template's golden
// actor would take part in.
var coldBoot = &ateapi.GoldenSnapshotConfig{Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// workers lists the workers herd-api has registrations of.
func workers(ctx context.Context, t *testing.T, addr, tok string) []string {
	opts := []grpc.DialOption{transportFor(t, addr)}
	if tok != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(auth.Bearer(tok)))
	}
	cc, err := grpc.NewClient("passthrough:///"+addr, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	var resp herdext.ListWorkerCapabilitiesResponse
	if err := cc.Invoke(ctx, "/herd.ext.v1.Capabilities/ListWorkerCapabilities", &herdext.ListWorkerCapabilitiesRequest{}, &resp); err != nil {
		return nil
	}
	var out []string
	for _, w := range resp.GetWorkers() {
		out = append(out, w.GetWorker())
	}
	return out
}

func waitWorkers(t *testing.T, addr, tok string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ws := workers(ctxT(t), t, addr, tok)
		if len(ws) >= n {
			return ws
		}
		if time.Now().After(deadline) {
			t.Fatalf("herd-api has workers %v, want %d", ws, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pool creates a template and n actors through the Control API and returns the workers they were placed on.
func pool(t *testing.T, addr, tok string, n int) []string {
	t.Helper()
	c := client(t, addr, tok)
	defer c.Close()
	ctx := ctxT(t)
	if _, err := c.CreateActorTemplate(ctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata:       &ateapi.ResourceMetadata{Atespace: space, Name: "agent"},
		Containers:     []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
		SnapshotConfig: &ateapi.SnapshotConfig{GoldenSnapshotConfig: coldBoot},
	}}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	var placed []string
	for i := 0; i < n; i++ {
		a, err := c.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
			Metadata:      &ateapi.ResourceMetadata{Atespace: space, Name: "a" + string(rune('0'+i))},
			ActorTemplate: &ateapi.ObjectRef{Atespace: space, Name: "agent"},
		}})
		if err != nil {
			t.Fatalf("CreateActor %d: %v", i, err)
		}
		if a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
			t.Fatalf("actor %d: state %v", i, a.GetStatus().GetState())
		}
		placed = append(placed, a.GetStatus().GetAssignedNode())
	}
	return placed
}

// TestAllInOneJoinsItsOwnHerdlet: `herd` runs herd-api on bbolt, herd-router, herd-egress and a herdlet in one
// process, and its herdlet becomes a worker through the same join a remote host makes — not a credential the
// all-in-one hands it on the side. An actor then runs on it.
func TestAllInOneJoinsItsOwnHerdlet(t *testing.T) {
	data := t.TempDir()
	var mu sync.Mutex
	var grants []join.Grant
	a := &deploy.AllInOne{Data: data, Listen: "127.0.0.1:0", RouterListen: "127.0.0.1:0", TunnelListen: "127.0.0.1:0",
		EgressListen: "127.0.0.1:0", DNSListen: "127.0.0.1:0", Egress: true, Herdlet: herdletOn(t, "aio", nil),
		OnJoin: func(g join.Grant) { mu.Lock(); grants = append(grants, g); mu.Unlock() }}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan deploy.Addrs, 1)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, func(ad deploy.Addrs) { ready <- ad }) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("all-in-one: %v", err)
		}
	})
	var addrs deploy.Addrs
	select {
	case addrs = <-ready:
	case err := <-done:
		t.Fatalf("all-in-one stopped: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("all-in-one not ready")
	}
	localOperator(t, data, addrs.API)
	tok := ""
	waitWorkers(t, addrs.API, tok, 1)
	if got := pool(t, addrs.API, tok, 1); got[0] != "aio" {
		t.Errorf("actor placed on %q, want aio", got[0])
	}
	// The actor is a VM the herdlet built from the template: its share exists under the herdlet's state.
	if shares, err := os.ReadDir(filepath.Join(data, "herdlet", "shares")); err != nil || len(shares) != 1 {
		t.Errorf("herdlet shares: %v %v, want one actor VM's share", shares, err)
	}
	if _, err := os.Stat(filepath.Join(data, "herd.db")); err != nil {
		t.Errorf("no bbolt store: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(grants) != 1 || grants[0].Worker != "aio" || grants[0].Tunnel != addrs.Tunnel {
		t.Errorf("herd-api granted %+v; want one join by worker aio advertising tunnel %s", grants, addrs.Tunnel)
	}
	st, err := os.ReadFile(deploy.StatePath(filepath.Join(data, "herdlet")))
	if err != nil || !strings.Contains(string(st), `"worker": "aio"`) {
		t.Errorf("herdlet join state: %v %s", err, st)
	}
}

type api struct {
	addr string
	key  []byte
	dir  string
}

// startAPI runs herd-api on the tests' CA's SVID with a policy of no rules (its callers are nodes and operators).
func startAPI(t *testing.T) api { return startAPIWith(t, nil) }

// startAPIWith is startAPI with the HS256 break-glass on, until tomorrow, when breakGlass: its key is api.key.
func startAPIWith(t *testing.T, configure func(*deploy.API)) api {
	t.Helper()
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &deploy.API{Identity: apiIdentity(t), Policy: noRules(t), Store: "bolt:" + filepath.Join(dir, "herd.db"),
		Snapshots: filepath.Join(dir, "snapshots"), CallTimeout: 5 * time.Second, Reconcile: time.Second}
	if configure != nil {
		configure(a)
	}
	key := a.BreakGlass.Key
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("herd-api: %v", err)
		}
	})
	return api{addr: ln.Addr().String(), key: key, dir: dir}
}

func runHerdlet(t *testing.T, h deploy.Herdlet) <-chan error {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	fin := make(chan struct{})
	go func() { done <- h.Run(ctx); close(fin) }()
	t.Cleanup(func() {
		cancel()
		<-fin
	})
	return done
}

// TestTwoHerdletsJoinBySVID (ADR 0001 A-60): two hosts join one herd-api, each by its node SVID with no token, each
// becomes a worker, and actors are placed on both.
func TestTwoHerdletsJoinBySVID(t *testing.T) {
	a := startAPI(t)
	shared := fake.NewStore()
	for _, w := range []string{"node-1", "node-2"} {
		h := herdletOn(t, w, shared)
		h.API, h.Snapshots = a.addr, filepath.Join(a.dir, "snapshots")
		runHerdlet(t, h)
	}
	waitWorkers(t, a.addr, "", 2)
	got := pool(t, a.addr, "", 2)
	if !((got[0] == "node-1" && got[1] == "node-2") || (got[0] == "node-2" && got[1] == "node-1")) {
		t.Errorf("actors placed on %v, want one on each joined node", got)
	}
}

// joinAs asks herd-api at addr for worker's grant, dialing as src with tok as its bearer if set.
func joinAs(t *testing.T, addr string, src auth.Source, tok, worker string) error {
	t.Helper()
	cfg, err := auth.ClientTLS(src, auth.HerdID(spiffeid.RequireTrustDomainFromString(testTD), "api"))
	if err != nil {
		t.Fatal(err)
	}
	cc, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	_, err = join.Call(ctxT(t), cc, tok, worker)
	return err
}

// TestJoinRefusesWhatIsNotItsNode (A-60): a join is granted to the node SVID of the worker it names. Another node's
// SVID, an operator's, and a join token while the HS256 break-glass is off get no grant; under the break-glass a join
// token is still refused when forged or expired, and a join token cannot itself open a herdlet link.
func TestJoinRefusesWhatIsNotItsNode(t *testing.T) {
	a := startAPI(t)
	node1 := source(t, "spiffe://"+testTD+"/herd/node/node-1")
	if err := joinAs(t, a.addr, node1, "", "node-1"); err != nil {
		t.Fatalf("node-1's SVID joining as node-1: %v", err)
	}
	key := make([]byte, 32)
	jt, _ := join.MintToken(key, time.Hour)
	for _, c := range []struct {
		name   string
		src    auth.Source
		tok    string
		code   codes.Code
		reason string
	}{
		{"another node's SVID", node1, "", codes.PermissionDenied, `worker "node-1"'s`},
		{"an operator's SVID", source(t, operatorID), "", codes.PermissionDenied, "neither a node SVID nor a join token"},
		{"a join token without the break-glass", node1, jt, codes.Unauthenticated, "no break-glass allowance"},
	} {
		err := joinAs(t, a.addr, c.src, c.tok, "node-x")
		if status.Code(err) != c.code || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("join of node-x with %s: %v, want %v naming %q", c.name, err, c.code, c.reason)
		}
	}

	b := startAPIWith(t, func(a *deploy.API) {
		a.BreakGlass = deploy.BreakGlass{Until: time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02"), Key: key}
	})
	good, _ := join.MintToken(key, time.Hour)
	if err := joinAs(t, b.addr, node1, good, "node-x"); err != nil {
		t.Errorf("a join token under the break-glass: %v", err)
	}
	forged, _ := join.MintToken([]byte("ffffffffffffffffffffffffffffffff"), time.Hour)
	expired, _ := auth.Sign(key, auth.Claims{Subject: "join-old", ExpiresAt: time.Now().Add(-time.Minute).Unix(), Join: true})
	for name, tok := range map[string]string{"forged": forged, "expired": expired} {
		if err := joinAs(t, b.addr, node1, tok, "node-y"); status.Code(err) != codes.Unauthenticated {
			t.Errorf("a %s join token under the break-glass: %v, want Unauthenticated", name, err)
		}
	}
	// A join token is not a herdlet credential.
	cfg, err := auth.ClientTLS(source(t, operatorID), auth.HerdID(spiffeid.RequireTrustDomainFromString(testTD), "api"))
	if err != nil {
		t.Fatal(err)
	}
	cc, err := grpc.NewClient("passthrough:///"+b.addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)),
		grpc.WithPerRPCCredentials(auth.Bearer(good)))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	cs, err := cc.NewStream(ctxT(t), &link.StreamDesc, link.LinkMethod, grpc.CallContentSubtype(link.CodecName))
	if err == nil {
		_ = cs.SendMsg(map[string]any{"register": map[string]any{"worker": "node-x"}})
		var m map[string]any
		err = cs.RecvMsg(&m)
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("link opened with a join token: %v", err)
	}
}
