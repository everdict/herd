package deploy_test

import (
	"context"
	"errors"
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
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/deploy"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/join"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
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
	return deploy.Herdlet{Worker: worker, StateDir: state, Slots: 1, Kernel: "vmlinux",
		VCPUs: 1, MemoryMiB: 512, VMM: fake.New(chCaps(), store), Retry: 50 * time.Millisecond,
		Images: &image.Cache{Dir: filepath.Join(state, "images"), Source: registry(), Builder: &imagetest.Builder{},
			Platform: image.Platform{OS: "linux", Architecture: "amd64"}}}
}

func writeToken(t *testing.T, key []byte) string {
	tok, err := auth.Sign(key, auth.Claims{Subject: "everdict", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		Spaces: map[string]auth.Role{space: auth.RoleWrite}})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// workers lists the workers herd-api has registrations of.
func workers(ctx context.Context, t *testing.T, addr, tok string) []string {
	cc, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(auth.Bearer(tok)))
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
	c := conformance.NewClient(addr, tok)
	defer c.Close()
	ctx := ctxT(t)
	if _, err := c.CreateActorTemplate(ctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata:   &ateapi.ResourceMetadata{Atespace: space, Name: "agent"},
		Containers: []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
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
	key, err := deploy.ReadKey(filepath.Join(data, "jwt.key"))
	if err != nil {
		t.Fatal(err)
	}
	tok := writeToken(t, key)
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

func startAPI(t *testing.T) api {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "jwt.key")
	key, err := deploy.EnsureKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &deploy.API{KeyFile: keyFile, Store: "bolt:" + filepath.Join(dir, "herd.db"), Snapshots: filepath.Join(dir, "snapshots"),
		CallTimeout: 5 * time.Second, Reconcile: time.Second}
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

// TestTwoHerdletsJoinOneToken: two hosts join one herd-api with one join token minted by `herd-api join-token`,
// each becomes a worker, and actors are placed on both.
func TestTwoHerdletsJoinOneToken(t *testing.T) {
	a := startAPI(t)
	jt := &deploy.JoinToken{KeyFile: filepath.Join(a.dir, "jwt.key"), TTL: time.Hour}
	tok, err := jt.Mint()
	if err != nil {
		t.Fatal(err)
	}
	shared := fake.NewStore()
	for _, w := range []string{"node-1", "node-2"} {
		h := herdletOn(t, w, shared)
		h.API, h.Token, h.Snapshots = a.addr, tok, filepath.Join(a.dir, "snapshots")
		runHerdlet(t, h)
	}
	ctl := writeToken(t, a.key)
	waitWorkers(t, a.addr, ctl, 2)
	got := pool(t, a.addr, ctl, 2)
	if !((got[0] == "node-1" && got[1] == "node-2") || (got[0] == "node-2" && got[1] == "node-1")) {
		t.Errorf("actors placed on %v, want one on each joined node", got)
	}
}

// TestJoinRefusesAnInvalidToken: a join with a token herd-api did not sign, an expired join token, or a credential
// that is not a join token gets no grant and no worker; and a join token cannot itself open a herdlet link.
func TestJoinRefusesAnInvalidToken(t *testing.T) {
	a := startAPI(t)
	other := make([]byte, 32)
	forged, _ := join.MintToken(other, time.Hour)
	expired, _ := auth.Sign(a.key, auth.Claims{Subject: "join-old", ExpiresAt: time.Now().Add(-time.Minute).Unix(), Join: true})
	herdletCred, _ := auth.Sign(a.key, auth.Claims{Subject: "node-x", ExpiresAt: time.Now().Add(time.Hour).Unix(), Herdlet: true})
	control := writeToken(t, a.key)
	for _, c := range []struct {
		name, tok string
		code      codes.Code
	}{
		{"forged", forged, codes.Unauthenticated},
		{"expired", expired, codes.Unauthenticated},
		{"herdlet-credential", herdletCred, codes.PermissionDenied},
		{"control-token", control, codes.PermissionDenied},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := herdletOn(t, "node-x", nil)
			h.API, h.Token = a.addr, c.tok
			var err error
			select {
			case err = <-runHerdlet(t, h):
			case <-time.After(5 * time.Second):
				t.Fatalf("join with a %s token was accepted: the herdlet is still running", c.name)
			}
			if err == nil {
				t.Fatalf("join with a %s token succeeded", c.name)
			}
			var se interface{ GRPCStatus() *status.Status }
			if !errors.As(err, &se) || se.GRPCStatus().Code() != c.code {
				t.Errorf("join with a %s token: %v, want %v", c.name, err, c.code)
			}
			if _, err := os.Stat(deploy.StatePath(h.StateDir)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused join left a grant: %v", err)
			}
		})
	}
	if ws := workers(ctxT(t), t, a.addr, control); len(ws) != 0 {
		t.Errorf("workers after refused joins: %v", ws)
	}
	// A join token is not a herdlet credential.
	jt, _ := join.MintToken(a.key, time.Hour)
	cc, err := grpc.NewClient("passthrough:///"+a.addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(auth.Bearer(jt)))
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
