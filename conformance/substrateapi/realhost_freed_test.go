//go:build linux

package substrateapi_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance/substrateapi"
	"github.com/everdict/herd/deploy"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/mkfs"
	ociregistry "github.com/everdict/herd/image/registry"
	"github.com/everdict/herd/join"
	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/snapstore/s3/s3test"
	"github.com/everdict/herd/vmm/cloudhypervisor"
)

// TestRealHostFreedMemoryLeavesTheBundle (PLT-591; HERD_MEMORY_REAL=1, root, KVM, Cloud Hypervisor, virtiofsd, a guest
// kernel and initramfs from guest/build.sh, a registry reachable for the image) fills and frees a real guest's memory
// and reads what its MEMORY suspend sends to the snapshot store (an in-process S3 endpoint).
//
// One herdlet gives its VMs 1024 MiB. An actor is suspended and resumed once as it booted: what that suspend sends is
// the baseline. In the resumed actor the container then writes 384 MiB of random bytes into its /tmp (the guest's
// tmpfs) and removes them, runs a dd whose 256 MiB buffer of random bytes is freed when it exits, and writes, syncs
// and reads back 128 MiB of random bytes in its volume (/work), which the guest's page cache then holds. Its second
// suspend may send the baseline, the volume's 128 MiB (the bundle carries the volume) and 64 MiB more, no more: the
// guest reports its free pages and Cloud Hypervisor gives them back to the host (vmm.FreePageReporting), so the tmpfs
// and dd pages read zero once freed, and the flush drops the page cache.
// On v0.1.23 the 384 + 256 + 128 MiB freed or cached stay in the memory, and the bundle carries them.
func TestRealHostFreedMemoryLeavesTheBundle(t *testing.T) {
	if os.Getenv("HERD_MEMORY_REAL") != "1" {
		t.Skip("set HERD_MEMORY_REAL=1 (root, KVM, cloud-hypervisor, virtiofsd, HERD_REAL_KERNEL, HERD_REAL_INITRD) to fill and free a real guest's memory and read its bundle")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatal("TestRealHostFreedMemoryLeavesTheBundle needs root on Linux")
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
	dir, err := os.MkdirTemp("", "hf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	key, err := deploy.EnsureKey(filepath.Join(dir, "jwt.key"))
	if err != nil {
		t.Fatal(err)
	}
	creds := s3.Credentials{AccessKeyID: "herd", SecretAccessKey: "herd-secret"}
	store := s3test.New("us-east-1", creds)
	endpoint := s3test.Start(t, store).URL
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
	api := &deploy.API{Key: key, Store: "bolt:" + filepath.Join(dir, "herd.db"), S3: "s3://herd-snapshots", S3Endpoint: endpoint,
		S3Credentials: creds, CallTimeout: 5 * time.Minute, Reconcile: time.Second, AdvertiseTunnel: tl.Addr().String(), CredentialTTL: time.Hour}
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
	state := filepath.Join(dir, "herdlet-f")
	v := cloudhypervisor.New(cloudhypervisor.Config{
		Binary: env("HERD_REAL_CH", "cloud-hypervisor"), Virtiofsd: env("HERD_REAL_VIRTIOFSD", deploy.DefaultVirtiofsd),
		StateDir: filepath.Join(state, "vms"), SnapshotDir: filepath.Join(state, "vm-snapshots")})
	h := deploy.Herdlet{Worker: "herdlet-f", Labels: "pool=herdlet-f", StateDir: state, Slots: 2, Kernel: kernel, Initrd: initrd,
		VCPUs: 1, MemoryMiB: 1024, GuestPort: 8080, VMM: v, Guests: v, Retry: 200 * time.Millisecond,
		API: apiLn.Addr().String(), Token: jt,
		Images: &image.Cache{Dir: filepath.Join(state, "images"), Source: &ociregistry.Client{}, Builder: builder,
			Platform: image.Platform{OS: "linux", Architecture: runtime.GOARCH}}}
	serve(func() error { return h.Run(ctx) })
	tok := token(t, key)
	waitWorkers(t, apiLn.Addr().String(), tok, 1)
	c, err := substrateapi.DialControl(substrateapi.Endpoint{Addr: apiLn.Addr().String()}, tok)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cctx, ccancel := context.WithTimeout(ctx, 15*time.Minute)
	defer ccancel()
	tpl := &ateapi.ActorTemplate{
		Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: "fill"},
		Containers: []*ateapi.Container{{Name: "main", Image: env("HERD_REAL_IMAGE", "docker.io/library/busybox:1.36.1"), Command: []string{"sleep", "86400"},
			VolumeMounts: []*ateapi.VolumeMount{{Name: "work", MountPath: "/work"}}}},
		Volumes:        []*ateapi.Volume{{Name: "work", DurableDir: &ateapi.DurableDirVolumeSource{}}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY},
	}
	if _, err := c.CreateActorTemplate(cctx, tpl); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	if _, err := c.CreateActor(cctx, &ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: "filler"},
		ActorTemplate: &ateapi.ObjectRef{Atespace: space, Name: "fill"}}); err != nil {
		t.Fatalf("CreateActor: %v", err)
	}
	// sent suspends the actor and resumes it, and answers the bytes the store received for the suspend and its seconds.
	sent := func(what string) int64 {
		t.Helper()
		before, start := store.Received(), time.Now()
		if _, err := c.SuspendActor(cctx, space, "filler", ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS); err != nil {
			t.Fatalf("SuspendActor (%s): %v", what, err)
		}
		n, took := store.Received()-before, time.Since(start)
		if _, err := c.ResumeActor(cctx, space, "filler"); err != nil {
			t.Fatalf("ResumeActor (%s): %v", what, err)
		}
		t.Logf("MEASURE suspend %s: %d bytes sent to the store in %.2f s", what, n, took.Seconds())
		return n
	}
	// held is what the host holds for the guest's memory: the resident shared memory of its Cloud Hypervisor process.
	held := func(what string) int64 {
		t.Helper()
		// The template's golden actor may still be shutting down beside the actor: the largest live VM is read.
		pids, _ := filepath.Glob(filepath.Join(state, "vms", "*", "*", "cloud-hypervisor.pid"))
		var most int64 = -1
		for _, p := range pids {
			pid, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			// The pid file's first line is the pid; its second names the API socket.
			first, _, _ := strings.Cut(string(pid), "\n")
			st, err := os.ReadFile(filepath.Join("/proc", strings.TrimSpace(first), "status"))
			if err != nil {
				continue // that VM has ended
			}
			for _, l := range strings.Split(string(st), "\n") {
				if f := strings.Fields(l); len(f) == 3 && f[0] == "RssShmem:" {
					kb, err := strconv.ParseInt(f[1], 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("MEASURE host memory %s: VM %s holds %d MiB of its 1024", what, filepath.Base(filepath.Dir(p)), kb>>10)
					most = max(most, kb<<10)
				}
			}
		}
		if most >= 0 {
			return most
		}
		t.Fatalf("no live Cloud Hypervisor under %s", state)
		return 0
	}
	// A guest that has only booted busybox holds a fraction of its memory on the host. With init_on_free=1 (v0.1.24)
	// the kernel wrote every page it freed at boot, and the host held all 1024 MiB.
	booted := held("as booted")
	if limit := int64(512 << 20); booted > limit {
		t.Errorf("the host holds %d bytes for a guest that only booted, want at most %d of its 1024 MiB", booted, limit)
	}
	base := sent("as booted")
	const volume = 128 << 20
	script := "dd if=/dev/urandom of=/tmp/fill bs=1M count=384 2>/dev/null && rm /tmp/fill && " +
		"dd if=/dev/urandom of=/dev/null bs=256M count=1 2>/dev/null && " +
		"dd if=/dev/urandom of=/work/cached bs=1M count=128 2>/dev/null && sync && cat /work/cached >/dev/null && " +
		"grep -E '^(MemFree|Cached|Shmem):' /proc/meminfo"
	res, err := substrateapi.EnvExec{}.Exec(cctx, substrateapi.Router{Endpoint: substrateapi.Endpoint{Addr: cl.Addr().String()}, Token: tok},
		space+"/filler", "/bin/sh", "-c", script)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("filling the guest's memory: exit %d, %v\n%s%s", res.ExitCode, err, res.Stdout, res.Stderr)
	}
	t.Logf("the guest after the fill:\n%s", res.Stdout)
	// The 384 MiB of tmpfs and the 256 MiB of the process are free in the guest; once it has reported them the host
	// holds what the guest uses and the page cache's 128 MiB, not the 768 MiB the script touched.
	time.Sleep(6 * time.Second)
	if after, limit := held("after the fill, before the suspend"), int64(640<<20); after > limit {
		t.Errorf("the host holds %d bytes after the guest freed 640 MiB of its 1024, want at most %d: the freed pages were not given back", after, limit)
	}
	filled := sent("after 384 MiB of tmpfs, 256 MiB of a process and 128 MiB of page cache were filled and freed")
	if limit := base + volume + 64<<20; filled > limit {
		t.Errorf("the suspend after the fill sent %d bytes, want at most %d (the baseline %d, the volume's %d and 64 MiB): the freed memory or the page cache is in the bundle",
			filled, limit, base, volume)
	}
	if _, err := c.DeleteActor(cctx, space, "filler"); err != nil {
		t.Errorf("DeleteActor: %v", err)
	}
}
