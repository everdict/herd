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
// through "..", through a symlink it planted, or by making the volume itself a symlink, is refused, nothing is
// written outside the share, and no VM is started.
func TestVolumesNeverLeaveTheShare(t *testing.T) {
	outside := t.TempDir()
	cases := map[string][]imagetest.File{
		"dotdot": {{Name: "volumes/data/", Type: tar.TypeDir}, {Name: "volumes/data/../../pwned", Body: "x"}},
		"symlink-parent": {{Name: "volumes/data/", Type: tar.TypeDir}, {Name: "volumes/data/out", Type: tar.TypeSymlink, Link: outside},
			{Name: "volumes/data/out/pwned", Body: "x"}},
		"volume-is-symlink": {{Name: "volumes/data", Type: tar.TypeSymlink, Link: outside}},
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
