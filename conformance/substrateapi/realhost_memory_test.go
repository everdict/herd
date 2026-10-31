//go:build linux

package substrateapi_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance/substrateapi"
	"github.com/everdict/herd/deploy"
	"github.com/everdict/herd/egress/nft"
	"github.com/everdict/herd/guest/agent"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/mkfs"
	ociregistry "github.com/everdict/herd/image/registry"
	"github.com/everdict/herd/join"
	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/snapstore/s3/s3test"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/cloudhypervisor"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// realMemoryRestores is a herdlet's real Cloud Hypervisor that records the VM ids of its MEMORY restores, so the test
// tells a restore of the memory from a cold boot of the volumes.
type realMemoryRestores struct {
	*cloudhypervisor.VMM
	mu  sync.Mutex
	ids []string
}

func (r *realMemoryRestores) Restore(ctx context.Context, s vmm.Snapshot, spec vmm.Spec) (vmm.VM, error) {
	vm, err := r.VMM.Restore(ctx, s, spec)
	if err == nil && s.Fidelity == vmm.FidelityMemory {
		r.mu.Lock()
		r.ids = append(r.ids, spec.ID)
		r.mu.Unlock()
	}
	return vm, err
}

func (r *realMemoryRestores) restored() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ids)
}

// TestRealHostMemoryAcrossHerdlets (criteria cross-node, loss, real-host-test; HERD_MEMORY_REAL=1, root, KVM, Cloud
// Hypervisor v54, virtiofsd >= 1.11, a guest kernel and initramfs from guest/build.sh, and a registry reachable for
// the image, plus ip(8) and nft(8)) runs three herdlets on this host as nodes of one pool, each with a -state of its
// own (VM state, vm-snapshots, shares, image cache), joined to one herd-api whose snapshots are in an S3 bucket (an
// in-process S3 endpoint, or HERD_REAL_S3_ENDPOINT with AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY and HERD_REAL_S3,
// default s3://herd-snapshots).
//
// herdlet-a and herdlet-b give their VMs no NIC. A tag of a MEMORY snapshot taken on herdlet-a forks twice onto
// herdlet-b, and both forks resume the memory there under their own VM ids; an actor suspended at MEMORY on
// herdlet-a, whose vm-snapshots are then removed, resumes on herdlet-b by a vm.restore; the conformance suite moves
// from herdlet-a to herdlet-b at MEMORY.
//
// herdlet-n gives its VMs TAP links from HERD_REAL_POOL (default 10.89.0.0/24; its egress table redirects to ports
// 15101/15153, where nothing need listen). Its MEMORY snapshots restore with their NIC, from the bundle after its
// vm-snapshots are removed: a fork of a tag whose source was deleted resumes the memory under its own VM id on the
// source's link (its TAP, MAC and guest address) and a second fork, finding that link held, cold-boots on its own; a
// fork while the source runs cold-boots (one address runs once per host); a suspended actor resumes by a vm.restore
// with its NIC. A cross-herdlet MEMORY restore with a NIC is not possible on one host: two herdlets on one host name
// their TAPs alike (herd<slot>) and a guest's memory keeps its address, which only the taking host's pool holds while
// its source runs; across hosts, as the Nomad walk runs them, the target claims the same link. The test shows instead
// that a herdlet-n MEMORY snapshot moved to herdlet-b (no NIC) cold-boots there, as the device sets differ.
//
// A failed restore leaves Cloud Hypervisor's exit, its info-level log, virtiofsd's log and the kernel's messages
// about the process in the test log.
//
//	sudo -E env "PATH=$PATH" HERD_MEMORY_REAL=1 HERD_REAL_KERNEL=/usr/local/share/herd/kernel \
//	  HERD_REAL_INITRD=/usr/local/share/herd/initrd.img go test -count=1 -v -timeout 40m \
//	  -run TestRealHostMemoryAcrossHerdlets ./conformance/substrateapi/
//
// Optional: HERD_REAL_IMAGE (default docker.io/library/busybox:1.36.1; it needs sleep), HERD_REAL_CH
// (cloud-hypervisor), HERD_REAL_VIRTIOFSD (/usr/local/libexec/herd/virtiofsd), HERD_REAL_POOL.
func TestRealHostMemoryAcrossHerdlets(t *testing.T) {
	if os.Getenv("HERD_MEMORY_REAL") != "1" {
		t.Skip("set HERD_MEMORY_REAL=1 (root, KVM, cloud-hypervisor, virtiofsd, HERD_REAL_KERNEL, HERD_REAL_INITRD) to move MEMORY actors between two real herdlets")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatal("TestRealHostMemoryAcrossHerdlets needs root on Linux")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	kernel, initrd := os.Getenv("HERD_REAL_KERNEL"), os.Getenv("HERD_REAL_INITRD")
	if kernel == "" || initrd == "" {
		t.Fatal("HERD_REAL_KERNEL and HERD_REAL_INITRD must name the guest kernel and initramfs (guest/build.sh)")
	}
	// Short: the VMs' unix sockets live under each -state.
	dir, err := os.MkdirTemp("", "hm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	key, err := deploy.EnsureKey(filepath.Join(dir, "jwt.key"))
	if err != nil {
		t.Fatal(err)
	}
	bucket, endpoint := env("HERD_REAL_S3", "s3://herd-snapshots"), os.Getenv("HERD_REAL_S3_ENDPOINT")
	creds := s3.Credentials{AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY")}
	if endpoint == "" {
		creds = s3.Credentials{AccessKeyID: "herd", SecretAccessKey: "herd-secret"}
		endpoint = s3test.Start(t, s3test.New("us-east-1", creds)).URL
	}
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
	api := &deploy.API{Key: key, Store: "bolt:" + filepath.Join(dir, "herd.db"), S3: bucket, S3Endpoint: endpoint,
		S3Credentials: creds, CallTimeout: 2 * time.Minute, Reconcile: time.Second, AdvertiseTunnel: tl.Addr().String(), CredentialTTL: time.Hour}
	serve(func() error { return api.Run(ctx, apiLn) })
	rt := &deploy.Router{Key: key, API: apiLn.Addr().String(), ParkingBudget: time.Minute, DefaultPort: 8080}
	serve(func() error { return rt.Run(ctx, cl, tl) })
	jt, err := join.MintToken(key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	builder, why, err := mkfs.Detect(ctx, mkfs.Exec{})
	if err != nil {
		t.Fatalf("rootfs builder: %s: %v", why, err)
	}
	nodes := map[string]*realMemoryRestores{}
	for _, w := range []string{"herdlet-a", "herdlet-b", "herdlet-n"} {
		state := filepath.Join(dir, w)
		v := &realMemoryRestores{VMM: cloudhypervisor.New(cloudhypervisor.Config{
			Binary: env("HERD_REAL_CH", "cloud-hypervisor"), Virtiofsd: env("HERD_REAL_VIRTIOFSD", deploy.DefaultVirtiofsd),
			StateDir: filepath.Join(state, "vms"), SnapshotDir: filepath.Join(state, "vm-snapshots")})}
		nodes[w] = v
		h := deploy.Herdlet{Worker: w, Labels: "pool=" + w, StateDir: state, Slots: 4, Kernel: kernel, Initrd: initrd,
			VCPUs: 1, MemoryMiB: 512, GuestPort: 8080, VMM: v, Guests: v, Retry: 200 * time.Millisecond,
			API: apiLn.Addr().String(), Token: jt,
			Images: &image.Cache{Dir: filepath.Join(state, "images"), Source: &ociregistry.Client{}, Builder: builder,
				Platform: image.Platform{OS: "linux", Architecture: runtime.GOARCH}}}
		if w == "herdlet-n" {
			// Real TAPs and egress tables (ip, nft), with ports and a bindings file of its own.
			h.NetHost, h.NetPool = nft.ExecHost{}, env("HERD_REAL_POOL", "10.89.0.0/24")
			h.EgressProxyPort, h.EgressDNSPort, h.EgressBindings = 15101, 15153, filepath.Join(state, "egress-bindings.json")
		}
		serve(func() error { return h.Run(ctx) })
	}
	tok := token(t, key)
	waitWorkers(t, apiLn.Addr().String(), tok, 3)
	c, err := substrateapi.DialControl(substrateapi.Endpoint{Addr: apiLn.Addr().String()}, tok)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	img := env("HERD_REAL_IMAGE", "docker.io/library/busybox:1.36.1")
	for _, name := range []string{"agent", "pair"} {
		tpl := &ateapi.ActorTemplate{
			Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: name},
			Containers: []*ateapi.Container{{Name: "main", Image: img, Command: []string{"sleep", "86400"},
				VolumeMounts: []*ateapi.VolumeMount{{Name: "work", MountPath: "/work"}}}},
			Volumes:        []*ateapi.Volume{{Name: "work", DurableDir: &ateapi.DurableDirVolumeSource{}}},
			SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY},
		}
		if name == "pair" {
			tpl.Containers = []*ateapi.Container{
				{Name: "left", Image: img, Command: []string{"sleep", "86400"}, VolumeMounts: []*ateapi.VolumeMount{{Name: "shared", MountPath: "/left"}}},
				{Name: "right", Image: img, Command: []string{"sleep", "86400"}, VolumeMounts: []*ateapi.VolumeMount{{Name: "shared", MountPath: "/right"}}},
			}
			tpl.Volumes = []*ateapi.Volume{{Name: "shared", DurableDir: &ateapi.DurableDirVolumeSource{}}}
		}
		tctx, tcancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := c.CreateActorTemplate(tctx, tpl)
		tcancel()
		if err != nil {
			t.Fatalf("CreateActorTemplate %s: %v", name, err)
		}
	}

	cctx, ccancel := context.WithTimeout(ctx, 15*time.Minute)
	defer ccancel()
	ref := func(name string) *ateapi.ObjectRef { return &ateapi.ObjectRef{Atespace: space, Name: name} }
	create := func(t *testing.T, name, pool, tag string) *ateapi.Actor {
		t.Helper()
		a := &ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: name}, ActorTemplate: ref("agent"),
			WorkerSelector: &ateapi.Selector{MatchLabels: map[string]string{"pool": pool}}}
		if tag != "" {
			a.SourceTag = ref(tag)
		}
		act, err := c.CreateActor(cctx, a)
		if err != nil {
			t.Fatalf("CreateActor %s on %s: %v", name, pool, err)
		}
		if n, s := act.GetStatus().GetAssignedNode(), act.GetStatus().GetState(); n != pool || s != ateapi.ActorState_ACTOR_STATE_RUNNING {
			t.Fatalf("%s: %v on %q, want RUNNING on %s", name, s, n, pool)
		}
		return act
	}
	t.Run("fork-via-tag", func(t *testing.T) {
		create(t, "real-src", "herdlet-a", "")
		if _, err := c.CreateTag(cctx, &ateapi.Tag{Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: "real-t1"}, SourceActor: ref("real-src")}); err != nil {
			t.Fatalf("CreateTag: %v", err)
		}
		f1 := create(t, "real-fork1", "herdlet-b", "real-t1")
		f2 := create(t, "real-fork2", "herdlet-b", "real-t1")
		if got := nodes["herdlet-b"].restored(); !slices.Equal(got, []string{f1.GetMetadata().GetUid(), f2.GetMetadata().GetUid()}) {
			t.Errorf("herdlet-b's MEMORY restores: %v; want both forks', each under its own VM id", got)
		}
		for _, n := range []string{"real-fork1", "real-fork2", "real-src"} {
			if _, err := c.DeleteActor(cctx, space, n); err != nil {
				t.Errorf("DeleteActor %s: %v", n, err)
			}
		}
	})
	t.Run("move-after-node-loss", func(t *testing.T) {
		before := len(nodes["herdlet-b"].restored())
		mover := create(t, "real-mover", "herdlet-a", "")
		sr, err := c.SuspendActor(cctx, space, "real-mover", ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS)
		if err != nil {
			t.Fatalf("SuspendActor on herdlet-a: %v", err)
		}
		act := sr.GetActor()
		act.WorkerSelector = &ateapi.Selector{MatchLabels: map[string]string{"pool": "herdlet-b"}}
		if _, err := c.UpdateActor(cctx, act); err != nil {
			t.Fatalf("UpdateActor: %v", err)
		}
		// herdlet-a keeps running for the suite below, but nothing of its snapshots is left to restore from.
		if err := os.RemoveAll(filepath.Join(dir, "herdlet-a", "vm-snapshots")); err != nil {
			t.Fatal(err)
		}
		rr, err := c.ResumeActor(cctx, space, "real-mover")
		if err != nil {
			t.Fatalf("ResumeActor onto herdlet-b with herdlet-a's vm-snapshots gone: %v", err)
		}
		if n, s := rr.GetActor().GetStatus().GetAssignedNode(), rr.GetActor().GetStatus().GetState(); n != "herdlet-b" || s != ateapi.ActorState_ACTOR_STATE_RUNNING {
			t.Errorf("resumed: %v on %q, want RUNNING on herdlet-b", s, n)
		}
		if got := nodes["herdlet-b"].restored(); len(got) != before+1 || got[before] != mover.GetMetadata().GetUid() {
			t.Errorf("herdlet-b's MEMORY restores: %v; want the mover's %s after the %d before", got, mover.GetMetadata().GetUid(), before)
		}
		if _, err := c.DeleteActor(cctx, space, "real-mover"); err != nil {
			t.Errorf("DeleteActor: %v", err)
		}
	})
	// gone deletes actors and waits until herd-api no longer has them: their VMs, shares and links are released.
	gone := func(t *testing.T, names ...string) {
		t.Helper()
		for _, n := range names {
			if _, err := c.DeleteActor(cctx, space, n); err != nil {
				t.Errorf("DeleteActor %s: %v", n, err)
			}
		}
		deadline := time.Now().Add(2 * time.Minute)
		for _, n := range names {
			for {
				if _, err := c.GetActor(cctx, space, n); status.Code(err) == codes.NotFound {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s is still there after its delete", n)
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}
	suspendTo := func(t *testing.T, name, pool string) *ateapi.ResumeActorResponse {
		t.Helper()
		sr, err := c.SuspendActor(cctx, space, name, ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS)
		if err != nil {
			t.Fatalf("SuspendActor %s: %v", name, err)
		}
		act := sr.GetActor()
		act.WorkerSelector = &ateapi.Selector{MatchLabels: map[string]string{"pool": pool}}
		if _, err := c.UpdateActor(cctx, act); err != nil {
			t.Fatalf("UpdateActor %s: %v", name, err)
		}
		// Nothing of herdlet-n's own snapshots is left: a restore there reads the memory the bundle carries.
		if err := os.RemoveAll(filepath.Join(dir, "herdlet-n", "vm-snapshots")); err != nil {
			t.Fatal(err)
		}
		rr, err := c.ResumeActor(cctx, space, name)
		if err != nil {
			t.Fatalf("ResumeActor %s onto %s: %v", name, pool, err)
		}
		if n, s := rr.GetActor().GetStatus().GetAssignedNode(), rr.GetActor().GetStatus().GetState(); n != pool || s != ateapi.ActorState_ACTOR_STATE_RUNNING {
			t.Errorf("%s resumed: %v on %q, want RUNNING on %s", name, s, n, pool)
		}
		return rr
	}
	t.Run("nic-fork-after-source-deleted", func(t *testing.T) {
		create(t, "nic-src", "herdlet-n", "")
		if _, err := c.CreateTag(cctx, &ateapi.Tag{Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: "nic-t1"}, SourceActor: ref("nic-src")}); err != nil {
			t.Fatalf("CreateTag: %v", err)
		}
		gone(t, "nic-src")
		if err := os.RemoveAll(filepath.Join(dir, "herdlet-n", "vm-snapshots")); err != nil {
			t.Fatal(err)
		}
		before := len(nodes["herdlet-n"].restored())
		f1 := create(t, "nic-fork1", "herdlet-n", "nic-t1")
		create(t, "nic-fork2", "herdlet-n", "nic-t1")
		if got := nodes["herdlet-n"].restored()[before:]; !slices.Equal(got, []string{f1.GetMetadata().GetUid()}) {
			t.Errorf("herdlet-n's MEMORY restores: %v; want nic-fork1's alone (on the source's link), nic-fork2 cold-booted", got)
		}
		gone(t, "nic-fork1", "nic-fork2")
	})
	t.Run("nic-fork-while-source-runs", func(t *testing.T) {
		create(t, "nic-src2", "herdlet-n", "")
		if _, err := c.CreateTag(cctx, &ateapi.Tag{Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: "nic-t2"}, SourceActor: ref("nic-src2")}); err != nil {
			t.Fatalf("CreateTag: %v", err)
		}
		before := len(nodes["herdlet-n"].restored())
		create(t, "nic-fork3", "herdlet-n", "nic-t2")
		if got := nodes["herdlet-n"].restored()[before:]; len(got) != 0 {
			t.Errorf("herdlet-n restored memory %v while the source holds its link; want a cold boot", got)
		}
		gone(t, "nic-fork3", "nic-src2")
	})
	t.Run("nic-suspend-resume-after-loss", func(t *testing.T) {
		before := len(nodes["herdlet-n"].restored())
		mover := create(t, "nic-mover", "herdlet-n", "")
		suspendTo(t, "nic-mover", "herdlet-n")
		if got := nodes["herdlet-n"].restored()[before:]; !slices.Equal(got, []string{mover.GetMetadata().GetUid()}) {
			t.Errorf("herdlet-n's MEMORY restores: %v; want nic-mover's, with its NIC", got)
		}
		gone(t, "nic-mover")
	})
	t.Run("nic-to-a-host-without-nics", func(t *testing.T) {
		before := len(nodes["herdlet-b"].restored())
		create(t, "nic-away", "herdlet-n", "")
		suspendTo(t, "nic-away", "herdlet-b")
		if got := nodes["herdlet-b"].restored()[before:]; len(got) != 0 {
			t.Errorf("herdlet-b restored memory %v taken with a NIC; want a cold boot", got)
		}
		gone(t, "nic-away")
	})
	t.Run("suite", func(t *testing.T) {
		cfg := suiteConfig("herd-real-memory", apiLn.Addr().String(), cl.Addr().String(), tok)
		cfg.Wait = 2 * time.Minute
		cfg.ExpectFidelity = ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY
		cfg.MoveFrom, cfg.MoveTo = map[string]string{"pool": "herdlet-a"}, map[string]string{"pool": "herdlet-b"}
		cfg.Template2, cfg.ContainerHeader = "pair", agent.ContainerHeader
		rep := substrateapi.Run(cctx, cfg)
		for _, r := range rep.Results {
			t.Logf("%s %s %s", r.Outcome, r.Case, r.Detail)
		}
		for _, name := range []string{"suspend", "fork-via-tag", "move", "two-containers"} {
			if r, _ := rep.Get(name); r.Outcome != substrateapi.Pass {
				t.Errorf("%s: %s %s; want PASS", name, r.Outcome, r.Detail)
			}
		}
	})
}
