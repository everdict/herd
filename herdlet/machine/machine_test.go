package machine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/image"
	"github.com/everdict/herd/image/imagetest"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
)

// recorder is a fake host that records the specs it was asked to boot or restore.
type recorder struct {
	*fake.Fake
	mu       sync.Mutex
	booted   []vmm.Spec
	restored []vmm.Spec
}

func (r *recorder) Boot(ctx context.Context, s vmm.Spec) (vmm.VM, error) {
	r.mu.Lock()
	r.booted = append(r.booted, s)
	r.mu.Unlock()
	return r.Fake.Boot(ctx, s)
}

func (r *recorder) Restore(ctx context.Context, snap vmm.Snapshot, s vmm.Spec) (vmm.VM, error) {
	r.mu.Lock()
	r.restored = append(r.restored, s)
	r.mu.Unlock()
	return r.Fake.Restore(ctx, snap, s)
}

func (r *recorder) starts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.booted) + len(r.restored)
}

var (
	macCaps    = vmm.Capabilities{Platform: vmm.PlatformVZ, Arch: "arm64", SuspendMaxFidelity: vmm.FidelityVolumes}
	ubuntuCaps = vmm.Capabilities{Platform: vmm.PlatformCloudHypervisor, Arch: "amd64", SuspendMaxFidelity: vmm.FidelityMemory, RestoreFamily: "microvm-ch-amd64-intel-146f56876e"}
	ubuntu2    = vmm.Capabilities{Platform: vmm.PlatformCloudHypervisor, Arch: "amd64", SuspendMaxFidelity: vmm.FidelityMemory, RestoreFamily: "microvm-ch-amd64-amd-8ca21de47b"}
)

// registry holds the two images of the test template, multi-arch as herd's images are published.
func registry() *imagetest.Registry {
	reg := imagetest.NewRegistry()
	for _, repo := range []string{"team/agent", "team/sidecar"} {
		var ms []imagetest.Manifest
		for _, arch := range []string{"amd64", "arm64"} {
			ms = append(ms, imagetest.Manifest{
				Platform: image.Platform{OS: "linux", Architecture: arch},
				Config:   image.Config{Entrypoint: []string{"/bin/" + filepath.Base(repo)}, Env: []string{"PATH=/bin", "ARCH=" + arch}, WorkingDir: "/work"},
				Layers:   [][]byte{imagetest.Layer(imagetest.File{Name: "bin/" + filepath.Base(repo), Body: arch, Mode: 0o755})},
			})
		}
		reg.Push(repo, "1", ms...)
	}
	return reg
}

func template() link.Template {
	return link.Template{
		UID: "tpl-1",
		Containers: []link.Container{
			{Name: "agent", Image: "registry.example/team/agent:1", Args: []string{"--serve"}, Env: []link.EnvVar{{Name: "ARCH", Value: "template"}},
				VolumeMounts: []link.VolumeMount{{Name: "data", MountPath: "/data"}}},
			{Name: "sidecar", Image: "registry.example/team/sidecar:1", Command: []string{"/bin/sh", "-c"}, Args: []string{"tail -f /data/log"},
				VolumeMounts: []link.VolumeMount{{Name: "data", MountPath: "/data"}}},
		},
		Volumes: []string{"data"},
	}
}

type host struct {
	vm *recorder
	m  *Machines
}

func newHost(t *testing.T, caps vmm.Capabilities, reg *imagetest.Registry) host {
	t.Helper()
	dir := t.TempDir()
	r := &recorder{Fake: fake.New(caps, nil)}
	cache := &image.Cache{Dir: filepath.Join(dir, "images"), Source: reg, Builder: &imagetest.Builder{}, Platform: image.Platform{OS: "linux", Architecture: caps.Arch}}
	m, err := New(Config{VMM: r, Images: cache, ShareDir: filepath.Join(dir, "shares"), Base: vmm.Spec{Kernel: "vmlinux", Initrd: "initramfs"}})
	if err != nil {
		t.Fatal(err)
	}
	return host{vm: r, m: m}
}

func readPlan(t *testing.T, share string) *initd.Plan {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(share, initd.PlanPath))
	if err != nil {
		t.Fatal(err)
	}
	p, err := initd.ParsePlan(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBootLaysVolumesInShare(t *testing.T) {
	h := newHost(t, ubuntuCaps, registry())
	if err := h.m.Boot(context.Background(), "actor-1", template()); err != nil {
		t.Fatal(err)
	}
	share := h.m.ShareDir("actor-1")
	spec := h.vm.booted[0]
	if len(spec.Shares) != 1 || spec.Shares[0].Tag != ShareTag || spec.Shares[0].HostPath != share || len(spec.Rootfs) != 2 {
		t.Fatalf("spec %+v", spec)
	}
	if fi, err := os.Lstat(filepath.Join(share, "data")); err != nil || !fi.IsDir() {
		t.Fatalf("volume data in the share: %v %v", fi, err)
	}
	p := readPlan(t, share)
	want := []initd.Container{
		{Name: "agent", Image: initd.Image{Device: "/dev/vda", FSType: "erofs"}, Command: []string{"/bin/agent", "--serve"},
			Env: []initd.EnvVar{{Name: "PATH", Value: "/bin"}, {Name: "ARCH", Value: "template"}}, Workdir: "/work",
			VolumeMounts: []initd.VolumeMount{{Name: "data", MountPath: "/data"}}},
		{Name: "sidecar", Image: initd.Image{Device: "/dev/vdb", FSType: "erofs"}, Command: []string{"/bin/sh", "-c", "tail -f /data/log"},
			Env: []initd.EnvVar{{Name: "PATH", Value: "/bin"}, {Name: "ARCH", Value: "amd64"}}, Workdir: "/work",
			VolumeMounts: []initd.VolumeMount{{Name: "data", MountPath: "/data"}}},
	}
	if len(p.Volumes) != 1 || p.Volumes[0].Name != "data" || len(p.Containers) != 2 {
		t.Fatalf("plan %+v", p)
	}
	for i := range want {
		if !equalJSON(t, p.Containers[i], want[i]) {
			t.Errorf("container %d: %+v, want %+v", i, p.Containers[i], want[i])
		}
	}
	// Each disk is the image built for this host's arch.
	b, _ := os.ReadFile(spec.Rootfs[0])
	if !strings.Contains(string(b), "bin/agent") {
		t.Errorf("first disk holds %q", b)
	}
	if p, err := h.m.Delete(context.Background(), "actor-1"); p != vmm.PresenceGone || err != nil {
		t.Fatalf("Delete = %v, %v", p, err)
	}
	if _, err := os.Lstat(share); !os.IsNotExist(err) {
		t.Fatalf("share still there after a gone: %v", err)
	}
}

func TestBootRefusesBadVolumeNames(t *testing.T) {
	h := newHost(t, ubuntuCaps, registry())
	for _, name := range []string{"..", "../escape", ".herd", "a/b", "/abs"} {
		tpl := template()
		tpl.Volumes = []string{name}
		tpl.Containers[0].VolumeMounts = []link.VolumeMount{{Name: name, MountPath: "/data"}}
		tpl.Containers[1].VolumeMounts = nil
		if err := h.m.Boot(context.Background(), "actor-bad", tpl); err == nil {
			t.Errorf("volume %q: booted", name)
		}
		if h.vm.starts() != 0 {
			t.Fatalf("volume %q: the VMM was asked to boot", name)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(h.m.cfg.ShareDir))
	if len(entries) != 0 {
		t.Fatalf("refused boots left %d shares", len(entries))
	}
}

// bundleWith is a VOLUMES bundle of the test template whose volume tree is the given entries.
func bundleWith(t *testing.T, entries ...imagetest.File) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	man := []byte(`{"version":1,"snapshot":{"ID":"s","VMID":"actor-src","Fidelity":1,"RestoreFamily":"microvm-volumes"},"template":` + string(mustJSON(t, template())) + `}`)
	_ = tw.WriteHeader(&tar.Header{Name: bundleManifest, Mode: 0o644, Size: int64(len(man)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(man)
	_ = tw.Close()
	inner := imagetest.Layer(entries...)
	// Concatenate the volume entries after the manifest (drop the first tar's end blocks).
	out := append([]byte(nil), b.Bytes()[:b.Len()-1024]...)
	return append(out, inner...)
}

// TestVolumesNeverLeaveTheShare is the volumes-in-share counterexample: a bundle whose volume tree escapes the share
// through "..", through a symlink it planted, or by making the volume itself a symlink, or whose layer archive is
// no plain file of the share's layer directory (DEFAUL-1619), is refused, nothing is
// written outside the share, and no VM is started.
func TestVolumesNeverLeaveTheShare(t *testing.T) {
	outside := t.TempDir()
	cases := map[string][]imagetest.File{
		"dotdot": {{Name: "volumes/data/", Type: tar.TypeDir}, {Name: "volumes/data/../../pwned", Body: "x"}},
		"symlink-parent": {{Name: "volumes/data/", Type: tar.TypeDir}, {Name: "volumes/data/out", Type: tar.TypeSymlink, Link: outside},
			{Name: "volumes/data/out/pwned", Body: "x"}},
		"volume-is-symlink": {{Name: "volumes/data", Type: tar.TypeSymlink, Link: outside}},
		"layer-dotdot":      {{Name: "rootfs/../../../pwned.tar", Body: "x"}},
		"layer-not-a-file":  {{Name: "rootfs/main.tar", Type: tar.TypeSymlink, Link: outside}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHost(t, macCaps, registry())
			err := h.m.Restore(context.Background(), "actor-1", bytes.NewReader(bundleWith(t, entries...)))
			if !errors.Is(err, ErrOutsideShare) {
				t.Errorf("Restore err = %v, want ErrOutsideShare", err)
			}
			if h.vm.starts() != 0 {
				t.Errorf("a VM was started with an escaping volume")
			}
			if left, _ := os.ReadDir(outside); len(left) != 0 {
				t.Errorf("written outside the share: %v", left[0].Name())
			}
			// "../.." from a volume is the directory every VM's share lives in; the refused restore leaves nothing there.
			if left, _ := os.ReadDir(h.m.cfg.ShareDir); len(left) != 0 {
				t.Errorf("left beside the shares: %v", left[0].Name())
			}
		})
	}
}

func TestSnapshotBundleCarriesVolumesAndSpec(t *testing.T) {
	ctx := context.Background()
	reg := registry()
	mac, ubuntu := newHost(t, macCaps, reg), newHost(t, ubuntuCaps, reg)
	if err := mac.m.Boot(ctx, "actor-1", template()); err != nil {
		t.Fatal(err)
	}
	vol := filepath.Join(mac.m.ShareDir("actor-1"), "data")
	if err := os.MkdirAll(filepath.Join(vol, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vol, "notes", "turn-1.md"), []byte("remember this"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("notes/turn-1.md", filepath.Join(vol, "latest")); err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	snap, err := mac.m.Snapshot(ctx, "actor-1", vmm.FidelityVolumes, true, &bundle)
	if err != nil {
		t.Fatal(err)
	}
	if snap.RestoreFamily != vmm.PortableRestoreFamily {
		t.Fatalf("snapshot %+v", snap)
	}
	if p, _ := mac.vm.Inspect(ctx, "actor-1"); p != vmm.PresenceGone {
		t.Fatalf("a suspend snapshot left the VM %v", p)
	}
	if err := ubuntu.m.Restore(ctx, "actor-1", bytes.NewReader(bundle.Bytes())); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(ubuntu.m.ShareDir("actor-1"), "data")
	if b, err := os.ReadFile(filepath.Join(moved, "latest")); err != nil || string(b) != "remember this" {
		t.Fatalf("volume after the move: %q, %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(moved, "notes", "turn-1.md")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode after the move: %v, %v", fi, err)
	}
	// The containers boot from images built for the restoring host's arch, from the same references.
	spec := ubuntu.vm.booted[0]
	b, _ := os.ReadFile(spec.Rootfs[0])
	if !strings.Contains(string(b), "bin/agent") || !strings.Contains(string(spec.Rootfs[0]), "erofs") {
		t.Fatalf("rootfs on the new host: %s holds %q", spec.Rootfs[0], b)
	}
	if p := readPlan(t, ubuntu.m.ShareDir("actor-1")); p.Containers[1].Env[1].Value != "amd64" {
		t.Fatalf("plan on the new host %+v", p.Containers[1])
	}
}

// TestMemoryBundleStaysInItsFamily: a MEMORY bundle is refused by a host of another family before anything is
// started or written there.
func TestMemoryBundleStaysInItsFamily(t *testing.T) {
	ctx := context.Background()
	reg := registry()
	src := newHost(t, ubuntuCaps, reg)
	if err := src.m.Boot(ctx, "actor-1", template()); err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	if _, err := src.m.Snapshot(ctx, "actor-1", vmm.FidelityMemory, true, &bundle); err != nil {
		t.Fatal(err)
	}
	for name, caps := range map[string]vmm.Capabilities{"mac": macCaps, "other-cpu-class": ubuntu2} {
		dst := newHost(t, caps, reg)
		if err := dst.m.Restore(ctx, "actor-1", bytes.NewReader(bundle.Bytes())); !errors.Is(err, vmm.ErrRestoreFamilyMismatch) {
			t.Errorf("%s: Restore err = %v, want ErrRestoreFamilyMismatch", name, err)
		}
		if dst.vm.starts() != 0 {
			t.Errorf("%s: the VMM was asked to start a MEMORY snapshot of another family", name)
		}
		if left, _ := os.ReadDir(dst.m.cfg.ShareDir); len(left) != 0 {
			t.Errorf("%s: a share was laid out", name)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func equalJSON(t *testing.T, a, b any) bool {
	return bytes.Equal(mustJSON(t, a), mustJSON(t, b))
}

// TestSecondMemoryForkColdBoots: two actors restored from one MEMORY bundle on one host with VM links (two forks of
// one tag) both start, each under its own VM id. The first restores the memory on the snapshot's link (the guest
// keeps its address); that link is then taken, so the second cold-boots from the bundle's volume tree on a link of
// its own instead of running a second copy of one address.
func TestSecondMemoryForkColdBoots(t *testing.T) {
	ctx := context.Background()
	h := newNetHost(t, registry(), "10.88.0.0/16").host
	if err := h.m.Boot(ctx, "parent", template()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.m.ShareDir("parent"), "data", "notes"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	if _, err := h.m.Snapshot(ctx, "parent", vmm.FidelityMemory, true, &bundle); err != nil {
		t.Fatal(err)
	}
	// The suspend releases the parent: its VM and share are gone before the forks.
	if p, err := h.m.Delete(ctx, "parent"); p != vmm.PresenceGone {
		t.Fatalf("release: %v, %v", p, err)
	}
	for _, id := range []string{"fork-1", "fork-2"} {
		if err := h.m.Restore(ctx, id, bytes.NewReader(bundle.Bytes())); err != nil {
			t.Fatalf("Restore %s: %v", id, err)
		}
		if b, err := os.ReadFile(filepath.Join(h.m.ShareDir(id), "data", "notes")); err != nil || string(b) != "kept" {
			t.Errorf("%s: volume holds %q, %v", id, b, err)
		}
	}
	if got := h.m.ShareDir("fork-1"); filepath.Base(got) != "fork-1" {
		t.Errorf("fork-1 runs in %s, want its own id", got)
	}
	if got := h.m.ShareDir("fork-2"); filepath.Base(got) != "fork-2" {
		t.Errorf("fork-2 runs in %s, want its own id", got)
	}
	h.vm.mu.Lock()
	defer h.vm.mu.Unlock()
	if len(h.vm.restored) != 1 || h.vm.restored[0].ID != "fork-1" || len(h.vm.booted) != 2 || h.vm.booted[1].ID != "fork-2" {
		t.Errorf("restored %d, booted %d (%v); want one memory restore and fork-2 cold-booted", len(h.vm.restored), len(h.vm.booted), h.vm.booted)
	}
}

// gate is an Images that holds each Get until n callers are inside it (or a second has passed), so concurrent
// restores reach the share's claim together.
type gate struct {
	Images
	mu      sync.Mutex
	n, in   int
	release chan struct{}
}

func (g *gate) Get(ctx context.Context, ref string) (image.Rootfs, error) {
	g.mu.Lock()
	g.in++
	if g.in == g.n {
		close(g.release)
	}
	g.mu.Unlock()
	select {
	case <-g.release:
	case <-time.After(time.Second):
	}
	return g.Images.Get(ctx, ref)
}

// TestConcurrentMemoryForks: two actors restored at once from one MEMORY bundle on its own host (two forks of one
// tag created together) both start, each in a share of its own holding the parent's volume tree, and what one
// writes the other does not see. Only one of them can have the memory on the snapshot's link; the other cold-boots
// on a link of its own.
func TestConcurrentMemoryForks(t *testing.T) {
	ctx := context.Background()
	h := newNetHost(t, registry(), "10.88.0.0/16").host
	if err := h.m.Boot(ctx, "parent", template()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.m.ShareDir("parent"), "data", "notes"), []byte("parent"), 0o644); err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	if _, err := h.m.Snapshot(ctx, "parent", vmm.FidelityMemory, true, &bundle); err != nil {
		t.Fatal(err)
	}
	if p, err := h.m.Delete(ctx, "parent"); p != vmm.PresenceGone {
		t.Fatalf("release: %v, %v", p, err)
	}
	h.m.cfg.Images = &gate{Images: h.m.cfg.Images, n: 2, release: make(chan struct{})}
	forks := []string{"fork-1", "fork-2"}
	errs := make([]error, len(forks))
	var wg sync.WaitGroup
	for i, id := range forks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = h.m.Restore(ctx, id, bytes.NewReader(bundle.Bytes()))
		}()
	}
	wg.Wait()
	for i, id := range forks {
		if errs[i] != nil {
			t.Fatalf("Restore %s at the same time as its sibling: %v", id, errs[i])
		}
		if b, err := os.ReadFile(filepath.Join(h.m.ShareDir(id), "data", "notes")); err != nil || string(b) != "parent" {
			t.Errorf("%s: volume holds %q, %v; want the parent's", id, b, err)
		}
	}
	if a, b := h.m.ShareDir(forks[0]), h.m.ShareDir(forks[1]); a == b {
		t.Fatalf("both forks run in %s", a)
	}
	for _, id := range forks {
		if err := os.WriteFile(filepath.Join(h.m.ShareDir(id), "data", "notes"), []byte(id), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range forks {
		if b, err := os.ReadFile(filepath.Join(h.m.ShareDir(id), "data", "notes")); err != nil || string(b) != id {
			t.Errorf("%s reads %q, %v after both wrote; want its own write", id, b, err)
		}
	}
	h.vm.mu.Lock()
	defer h.vm.mu.Unlock()
	if len(h.vm.restored) != 1 || len(h.vm.booted) != 2 {
		t.Errorf("restored %d, booted %d; want one memory restore and one cold boot besides the parent's", len(h.vm.restored), len(h.vm.booted))
	}
}

// TestMemoryForksWithoutLinksBothResume: on a host whose VMs have no link there is no address to keep apart, so two
// forks of one MEMORY tag both resume the memory, each under its own VM id and in a share of its own, while the
// tag's source still runs there: a fork never runs under another actor's VM id.
func TestMemoryForksWithoutLinksBothResume(t *testing.T) {
	ctx := context.Background()
	h := newHost(t, ubuntuCaps, registry())
	if err := h.m.Boot(ctx, "parent", template()); err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	if _, err := h.m.Snapshot(ctx, "parent", vmm.FidelityMemory, false, &bundle); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"fork-1", "fork-2"} {
		if err := h.m.Restore(ctx, id, bytes.NewReader(bundle.Bytes())); err != nil {
			t.Fatalf("Restore %s: %v", id, err)
		}
		if got := h.m.VMID(id); got != id {
			t.Errorf("%s runs as VM %s, want its own id", id, got)
		}
	}
	h.vm.mu.Lock()
	defer h.vm.mu.Unlock()
	if len(h.vm.restored) != 2 || h.vm.restored[0].ID != "fork-1" || h.vm.restored[1].ID != "fork-2" || len(h.vm.booted) != 1 {
		t.Errorf("restored %v, booted %d; want both forks' memory restored under their own ids", h.vm.restored, len(h.vm.booted))
	}
}

// TestForkIsNotReachedByItsParentsID: a fork restored from its parent's MEMORY snapshot on another host runs under
// its own VM id, so work on the parent's id there (herd-api releasing or inspecting a VM of the parent on that host)
// does not reach the fork's VM. A fork that ran under its parent's VM id (before PLT-212 round 3) was deleted by it.
func TestForkIsNotReachedByItsParentsID(t *testing.T) {
	ctx := context.Background()
	reg := registry()
	a, b := newHost(t, ubuntuCaps, reg), newHost(t, ubuntuCaps, reg)
	if err := a.m.Boot(ctx, "parent", template()); err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	if _, err := a.m.Snapshot(ctx, "parent", vmm.FidelityMemory, false, &bundle); err != nil {
		t.Fatal(err)
	}
	if err := b.m.Restore(ctx, "fork-1", bytes.NewReader(bundle.Bytes())); err != nil {
		t.Fatal(err)
	}
	if p, err := b.m.Inspect(ctx, "parent"); p != vmm.PresenceGone {
		t.Errorf("the parent's id on the fork's host: %v (%v), want gone", p, err)
	}
	if p, err := b.m.Delete(ctx, "parent"); p != vmm.PresenceGone {
		t.Fatalf("Delete of the parent's id on the fork's host: %v (%v)", p, err)
	}
	if p, err := b.m.Inspect(ctx, "fork-1"); p != vmm.PresencePresent {
		t.Errorf("fork-1 after a delete of its parent's id there: %v (%v), want present", p, err)
	}
}
