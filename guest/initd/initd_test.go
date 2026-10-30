package initd_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/guest/oci"
)

type mountCall struct{ source, target, fstype string }

// bindCall is a bind mount, with its target as the /proc/self/fd link resolved at the time of the call.
type bindCall struct {
	source, target string
	flags          uintptr
}

type fakeMounter struct {
	calls []mountCall
	binds []bindCall
}

func (f *fakeMounter) Mount(source, target, fstype string, flags uintptr, _ string) error {
	const msBind = 0x1000
	if flags&msBind != 0 {
		real, err := os.Readlink(target)
		if err != nil {
			real = target
		}
		f.binds = append(f.binds, bindCall{source, real, flags})
		return nil
	}
	f.calls = append(f.calls, mountCall{source, target, fstype})
	return nil
}

// fakeRuntime checks, at the moment a container starts, that every bind mount in its spec is a directory that
// already exists inside the share, and records the order of events.
type fakeRuntime struct {
	t      *testing.T
	share  string
	events []string
	specs  map[string]*oci.Spec
}

func (f *fakeRuntime) Run(_ context.Context, id, bundle string) error {
	b, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		f.t.Fatalf("Run(%s): no config.json: %v", id, err)
	}
	var s oci.Spec
	if err := json.Unmarshal(b, &s); err != nil {
		f.t.Fatal(err)
	}
	for _, m := range s.Mounts {
		if m.Type != "bind" || m.Destination == oci.GuestBinary {
			continue
		}
		real, err := filepath.EvalSymlinks(m.Source)
		if err != nil {
			f.t.Errorf("container %s started before its volume %s exists: %v", id, m.Source, err)
			continue
		}
		share, _ := filepath.EvalSymlinks(f.share)
		if !strings.HasPrefix(real, share+"/") {
			f.t.Errorf("container %s mounts %s (really %s), outside the share %s", id, m.Source, real, share)
		}
		if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
			f.t.Errorf("container %s: volume %s is not a directory", id, m.Source)
		}
	}
	if f.specs == nil {
		f.specs = map[string]*oci.Spec{}
	}
	f.specs[id] = &s
	f.events = append(f.events, "run "+id)
	return nil
}

func (f *fakeRuntime) Exec(context.Context, string, []string, bool) error { return nil }
func (f *fakeRuntime) Kill(context.Context, string, syscall.Signal) error { return nil }

const planJSON = `{
  "volumes": [{"name": "workspace"}, {"name": "home"}],
  "containers": [
    {"name": "main", "image": {"device": "/dev/vdb", "fsType": "erofs"},
     "command": ["claude"], "args": ["--print"], "env": [{"name": "HOME", "value": "/home/agent"}],
     "workdir": "/workspace",
     "volumeMounts": [{"name": "workspace", "mountPath": "/workspace"}, {"name": "home", "mountPath": "/home/agent"}]},
    {"name": "sidecar", "image": {"device": "/dev/vdc", "fsType": "ext4"},
     "command": ["sleep", "infinity"],
     "volumeMounts": [{"name": "workspace", "mountPath": "/data", "readOnly": true}]}
  ]
}`

func layout(t *testing.T) initd.Layout {
	t.Helper()
	d := t.TempDir()
	l := initd.Layout{ShareDir: filepath.Join(d, "share"), RunDir: filepath.Join(d, "run"), GuestBinary: "/init"}
	if err := os.MkdirAll(filepath.Join(l.ShareDir, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	return l
}

func boot(t *testing.T, l initd.Layout, plan string) (*fakeMounter, *fakeRuntime, []initd.Booted, error) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(l.ShareDir, initd.PlanPath), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	m, rt := &fakeMounter{}, &fakeRuntime{t: t, share: l.ShareDir}
	_, booted, err := initd.Boot(context.Background(), m, rt, l, "herd")
	return m, rt, booted, err
}

func TestBootMountsShareCreatesVolumesThenStartsContainers(t *testing.T) {
	l := layout(t)
	m, rt, booted, err := boot(t, l, planJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.calls) == 0 || m.calls[0] != (mountCall{"herd", l.ShareDir, "virtiofs"}) {
		t.Fatalf("first mount = %v, want the virtio-fs share", m.calls)
	}
	for _, v := range []string{"workspace", "home"} {
		if fi, err := os.Stat(filepath.Join(l.ShareDir, v)); err != nil || !fi.IsDir() {
			t.Errorf("volume %s not created in the share: %v", v, err)
		}
	}
	if got := strings.Join(rt.events, ","); got != "run main,run sidecar" {
		t.Errorf("events = %s", got)
	}
	if len(booted) != 2 || booted[0].Rootfs != filepath.Join(l.RunDir, "containers", "main", "rootfs") || booted[0].Workdir != "/workspace" {
		t.Errorf("booted = %+v", booted)
	}
	// Image read-only, then the overlay on top, per container.
	want := []mountCall{
		{"herd", l.ShareDir, "virtiofs"},
		{"/dev/vdb", filepath.Join(l.RunDir, "containers/main/image"), "erofs"},
		{"overlay", filepath.Join(l.RunDir, "containers/main/rootfs"), "overlay"},
		{"/dev/vdc", filepath.Join(l.RunDir, "containers/sidecar/image"), "ext4"},
		{"overlay", filepath.Join(l.RunDir, "containers/sidecar/rootfs"), "overlay"},
	}
	if len(m.calls) != len(want) {
		t.Fatalf("mounts = %v,\nwant %v", m.calls, want)
	}
	for i := range want {
		if m.calls[i] != want[i] {
			t.Errorf("mount %d = %v, want %v", i, m.calls[i], want[i])
		}
	}
	// A resumed actor keeps what its volumes hold.
	if err := os.WriteFile(filepath.Join(l.ShareDir, "workspace", "kept"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := boot(t, l, planJSON); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.ShareDir, "workspace", "kept")); err != nil {
		t.Errorf("volume content lost across boots: %v", err)
	}
}

// TestBootBindsVolumesWhereTheFileAPILooks (PLT-195): herd-guest's file API resolves a container's paths in the
// guest's own view of its rootfs, where crun's binds do not exist. Each volume is bound there too, from its share
// directory, read-only when the template says so, so a file the API writes under a volume's path is in the share.
func TestBootBindsVolumesWhereTheFileAPILooks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("herd-guest binds through /proc/self/fd, which only Linux has; the guest is Linux-only")
	}
	l := layout(t)
	m, _, _, err := boot(t, l, planJSON)
	if err != nil {
		t.Fatal(err)
	}
	const bind, ro = 0x1000, 0x1000 | 0x20 | 0x1
	main, side := filepath.Join(l.RunDir, "containers/main/rootfs"), filepath.Join(l.RunDir, "containers/sidecar/rootfs")
	want := []bindCall{
		{filepath.Join(l.ShareDir, "workspace"), filepath.Join(main, "workspace"), bind},
		{filepath.Join(l.ShareDir, "home"), filepath.Join(main, "home/agent"), bind},
		{filepath.Join(l.ShareDir, "workspace"), filepath.Join(side, "data"), bind},
		{"", filepath.Join(side, "data"), ro},
	}
	if len(m.binds) != len(want) {
		t.Fatalf("binds = %v,\nwant %v", m.binds, want)
	}
	for i := range want {
		if m.binds[i] != want[i] {
			t.Errorf("bind %d = %v, want %v", i, m.binds[i], want[i])
		}
	}
}

// A volume's mount path that the image makes a symlink out of the rootfs is refused: the guest-side bind would
// otherwise land outside the container.
func TestBootRefusesAVolumeMountPointOutsideTheRootfs(t *testing.T) {
	l := layout(t)
	outside := t.TempDir()
	rootfs := filepath.Join(l.RunDir, "containers/main/rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootfs, "workspace")); err != nil {
		t.Fatal(err)
	}
	m, rt, _, err := boot(t, l, planJSON)
	if err == nil {
		t.Fatal("boot accepted a volume mount point outside the rootfs")
	}
	for _, b := range m.binds {
		if strings.HasPrefix(b.target, outside) {
			t.Errorf("bound %s onto %s, outside the rootfs", b.source, b.target)
		}
	}
	if len(rt.events) != 0 {
		t.Errorf("containers started: %v", rt.events)
	}
}

// A volume must be a directory of the share: a symlink planted there under the volume's name is refused, and no
// container starts.
func TestBootRefusesAVolumeThatLeavesTheShare(t *testing.T) {
	l := layout(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(l.ShareDir, "workspace")); err != nil {
		t.Fatal(err)
	}
	_, rt, _, err := boot(t, l, planJSON)
	if err == nil {
		t.Fatal("boot with a volume symlinked out of the share succeeded")
	}
	if len(rt.events) != 0 {
		t.Errorf("containers started anyway: %v", rt.events)
	}
}

func TestPlanValidation(t *testing.T) {
	bad := map[string]string{
		"volume name with ..":   `{"volumes":[{"name":".."}],"containers":[{"name":"m","image":{"device":"/dev/vdb","fsType":"erofs"},"command":["x"]}]}`,
		"volume name .herd":     `{"volumes":[{"name":".herd"}],"containers":[{"name":"m","image":{"device":"/dev/vdb","fsType":"erofs"},"command":["x"]}]}`,
		"volume name a/b":       `{"volumes":[{"name":"a/b"}],"containers":[{"name":"m","image":{"device":"/dev/vdb","fsType":"erofs"},"command":["x"]}]}`,
		"undeclared volume":     `{"containers":[{"name":"m","image":{"device":"/dev/vdb","fsType":"erofs"},"command":["x"],"volumeMounts":[{"name":"w","mountPath":"/w"}]}]}`,
		"relative mount path":   `{"volumes":[{"name":"w"}],"containers":[{"name":"m","image":{"device":"/dev/vdb","fsType":"erofs"},"command":["x"],"volumeMounts":[{"name":"w","mountPath":"w"}]}]}`,
		"mount path with ..":    `{"volumes":[{"name":"w"}],"containers":[{"name":"m","image":{"device":"/dev/vdb","fsType":"erofs"},"command":["x"],"volumeMounts":[{"name":"w","mountPath":"/a/../b"}]}]}`,
		"mount over herd-guest": `{"volumes":[{"name":"w"}],"containers":[{"name":"m","image":{"device":"/dev/vdb","fsType":"erofs"},"command":["x"],"volumeMounts":[{"name":"w","mountPath":"/.herd"}]}]}`,
		"image not a device":    `{"containers":[{"name":"m","image":{"device":"/tmp/x","fsType":"erofs"},"command":["x"]}]}`,
		"no container":          `{"volumes":[]}`,
		"unknown field":         `{"containers":[{"name":"m","image":{"device":"/dev/vdb","fsType":"erofs"},"command":["x"],"privileged":true}]}`,
	}
	for name, plan := range bad {
		if _, err := initd.ParsePlan([]byte(plan)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
