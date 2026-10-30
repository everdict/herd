package substrateapi_test

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/conformance/substrateapi"
	"github.com/everdict/herd/deploy"
	"github.com/everdict/herd/guest/execrec"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/join"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
	"github.com/everdict/herd/vmm/fakeguest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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

const space = "conformance"

var (
	ubuntu = vmm.Capabilities{Platform: vmm.PlatformCloudHypervisor, Arch: "amd64", SuspendMaxFidelity: vmm.FidelityMemory,
		RestoreFamily: "microvm-ch-amd64-tigerlake"}
	mac = vmm.Capabilities{Platform: vmm.PlatformVZ, Arch: "arm64", SuspendMaxFidelity: vmm.FidelityVolumes}
)

// registry serves the template's image for both architectures.
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

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "cf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// herdlet is a herdlet on the fake hypervisor whose VMs run the real herd-guest, each guest's container being the
// VM's share as the herdlet's machines laid it out: what the guest writes in a volume is what a suspend carries.
// state is the herdlet's state directory as it will run (all-in-one puts it under its data directory).
func herdlet(t *testing.T, state, worker, labels string, caps vmm.Capabilities, vms *fake.Store) deploy.Herdlet {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	g := fakeguest.New(fake.New(caps, vms), shortDir(t), []string{self, "shim"})
	g.RootOf = func(vm string) string { return filepath.Join(state, "shares", vm) }
	t.Cleanup(g.Close)
	return deploy.Herdlet{Worker: worker, Labels: labels, StateDir: state, Slots: 4, Kernel: "vmlinux", VCPUs: 1, MemoryMiB: 512,
		GuestPort: fakeguest.GuestPort, VMM: g, Guests: g, Retry: 50 * time.Millisecond,
		Images: &image.Cache{Dir: filepath.Join(state, "images"), Source: registry(), Builder: &imagetest.Builder{},
			Platform: image.Platform{OS: "linux", Architecture: caps.Arch}}}
}

func token(t *testing.T, key []byte) string {
	t.Helper()
	tok, err := auth.Sign(key, auth.Claims{Subject: "conformance", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		Spaces: map[string]auth.Role{space: auth.RoleWrite}})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// template creates the suite's template through the Control API, as an operator would before a run.
func template(t *testing.T, addr, tok string, f ateapi.SnapshotFidelity) {
	t.Helper()
	c, err := substrateapi.DialControl(substrateapi.Endpoint{Addr: addr}, tok)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.CreateActorTemplate(ctx, &ateapi.ActorTemplate{
		Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: "agent"},
		Containers: []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1",
			VolumeMounts: []*ateapi.VolumeMount{{Name: "work", MountPath: "/work"}}}},
		Volumes:        []*ateapi.Volume{{Name: "work", DurableDir: &ateapi.DurableDirVolumeSource{}}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: f},
	}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
}

func suiteConfig(target, api, router, tok string) substrateapi.Config {
	return substrateapi.Config{Target: target, Control: substrateapi.Endpoint{Addr: api},
		Router: substrateapi.Router{Endpoint: substrateapi.Endpoint{Addr: router}, Token: tok}, Token: tok,
		Atespace: space, Template: "agent", Probe: substrateapi.EnvExec{}, VolumePath: "/work/conformance.txt",
		Wait: 20 * time.Second, Poll: 20 * time.Millisecond}
}

// runSuite runs the suite, logs the report and its skip list, and fails the test on any failed case or any skip
// the server did not declare.
func runSuite(t *testing.T, cfg substrateapi.Config) substrateapi.Report {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep := substrateapi.Run(ctx, cfg)
	var b bytes.Buffer
	rep.Print(&b)
	t.Log("\n" + b.String())
	for _, r := range rep.Results {
		if r.Outcome == substrateapi.Fail {
			t.Errorf("%s: %s", r.Case, r.Detail)
		}
	}
	if !rep.OK() {
		t.Errorf("the suite is not green on %s", cfg.Target)
	}
	return rep
}

func wantSkips(t *testing.T, rep substrateapi.Report, cases ...string) {
	t.Helper()
	got := map[string]bool{}
	for _, s := range rep.Skips() {
		got[s.Case] = true
	}
	for _, c := range cases {
		if !got[c] {
			t.Errorf("%s did not skip", c)
		}
		delete(got, c)
	}
	for c := range got {
		t.Errorf("%s skipped on %s; herd implements it", c, rep.Target)
	}
}

// startAllInOne runs `herd` all-in-one in process: herd-api on bbolt, herd-router, herd-egress and a herdlet joined
// over loopback, on the fake hypervisor with the real herd-guest in each VM. It returns the addresses and a token
// that writes in the suite's atespace, once the herdlet is a worker and the suite's MEMORY template exists.
func startAllInOne(t *testing.T) (deploy.Addrs, string) {
	t.Helper()
	data := t.TempDir()
	a := &deploy.AllInOne{Data: data, Listen: "127.0.0.1:0", RouterListen: "127.0.0.1:0", TunnelListen: "127.0.0.1:0",
		EgressListen: "127.0.0.1:0", DNSListen: "127.0.0.1:0", Egress: true, Herdlet: herdlet(t, filepath.Join(data, "herdlet"), "aio", "", ubuntu, nil)}
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
	tok := token(t, key)
	waitWorkers(t, addrs.API, tok, 1)
	template(t, addrs.API, tok, ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	return addrs, tok
}

// TestSuiteGreenOnAllInOne runs the suite against herd all-in-one (startAllInOne). Its one skip is the move case,
// which needs two worker pools.
func TestSuiteGreenOnAllInOne(t *testing.T) {
	addrs, tok := startAllInOne(t)
	cfg := suiteConfig("herd-all-in-one", addrs.API, addrs.Router, tok)
	cfg.ExpectFidelity = ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY
	rep := runSuite(t, cfg)
	wantSkips(t, rep, "move")
}

// TestHerdConformanceCommand runs the suite as an operator does, through `herd conformance` (deploy.Main, the
// binary's own entry point), against herd all-in-one; the command exits 0 only on a green run.
func TestHerdConformanceCommand(t *testing.T) {
	addrs, tok := startAllInOne(t)
	tf := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tf, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"conformance", "--target", "herd-all-in-one-cli", "--endpoint", addrs.API, "--token-file", tf,
		"--atespace", space, "--template", "agent", "--router", addrs.Router, "--probe", "env-exec",
		"--volume-path", "/work/conformance.txt", "--expect-fidelity", "MEMORY", "--wait", "20s"}
	if code := deploy.Main("herd", args); code != 0 {
		t.Fatalf("herd conformance exited %d", code)
	}
	// A template the target does not have fails every case, and the command says so.
	if code := deploy.Main("herd", append(args, "--template", "absent")); code != 1 {
		t.Errorf("herd conformance against an absent template exited %d, want 1", code)
	}
}

// TestSuiteGreenOnJoin runs the suite against the multi-host join mode: herd-api and herd-router on one "host", two
// herdlets of different families (a Mac on VZ declaring VOLUMES, an Ubuntu host on Cloud Hypervisor declaring
// MEMORY) joined with one join token and sharing only herd-api's snapshot directory. The move case takes an actor
// from the Mac to the Ubuntu host at VOLUMES and checks its volume.
func TestSuiteGreenOnJoin(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "jwt.key")
	key, err := deploy.EnsureKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	snaps := filepath.Join(dir, "snapshots")
	ctx, cancel := context.WithCancel(context.Background())
	var stops []chan error
	t.Cleanup(func() {
		cancel()
		for _, c := range stops {
			<-c
		}
	})
	serve := func(f func() error) {
		c := make(chan error, 1)
		stops = append(stops, c)
		go func() { c <- f() }()
	}
	apiLn, cl, tl := listen(t), listen(t), listen(t)
	api := &deploy.API{Key: key, Store: "bolt:" + filepath.Join(dir, "herd.db"), Snapshots: snaps, CallTimeout: 10 * time.Second,
		Reconcile: time.Second, AdvertiseTunnel: tl.Addr().String(), CredentialTTL: time.Hour}
	serve(func() error { return api.Run(ctx, apiLn) })
	rt := &deploy.Router{Key: key, API: apiLn.Addr().String(), ParkingBudget: 10 * time.Second, DefaultPort: fakeguest.GuestPort}
	serve(func() error { return rt.Run(ctx, cl, tl) })

	jt, err := join.MintToken(key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	vms := fake.NewStore()
	for _, h := range []deploy.Herdlet{herdlet(t, filepath.Join(dir, "mac-1"), "mac-1", "pool=mac", mac, vms), herdlet(t, filepath.Join(dir, "ubuntu-1"), "ubuntu-1", "pool=ubuntu", ubuntu, vms)} {
		h.API, h.Token, h.Snapshots = apiLn.Addr().String(), jt, snaps
		serve(func() error { return h.Run(ctx) })
	}
	tok := token(t, key)
	waitWorkers(t, apiLn.Addr().String(), tok, 2)
	template(t, apiLn.Addr().String(), tok, ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES)
	cfg := suiteConfig("herd-join", apiLn.Addr().String(), cl.Addr().String(), tok)
	cfg.ExpectFidelity = ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES
	cfg.MoveFrom, cfg.MoveTo = map[string]string{"pool": "mac"}, map[string]string{"pool": "ubuntu"}
	rep := runSuite(t, cfg)
	wantSkips(t, rep)
	if mv, _ := rep.Get("move"); mv.Outcome != substrateapi.Pass {
		t.Errorf("move: %v", mv)
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// waitWorkers waits until herd-api has n workers registered (test setup only: herd's own extension, never the
// suite's).
func waitWorkers(t *testing.T, addr, tok string, n int) {
	t.Helper()
	cc, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(auth.Bearer(tok)))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var resp herdext.ListWorkerCapabilitiesResponse
		err := cc.Invoke(context.Background(), "/herd.ext.v1.Capabilities/ListWorkerCapabilities", &herdext.ListWorkerCapabilitiesRequest{}, &resp)
		if err == nil && len(resp.GetWorkers()) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("herd-api has %d workers (%v), want %d", len(resp.GetWorkers()), err, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
