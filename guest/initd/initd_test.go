package initd_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/guest/oci"
)

type mountCall struct{ source, target, fstype string }

type fakeMounter struct{ calls []mountCall }

func (f *fakeMounter) Mount(source, target, fstype string, _ uintptr, _ string) error {
	f.calls = append(f.calls, mountCall{source, target, fstype})
	return nil
}

// fakeRuntime checks, at the moment a container starts, that every bind mount in its spec is a directory that
// already exists inside the share (or, for a guest file under the run dir, a regular file there), and records the
// order of events.
type fakeRuntime struct {
	t      *testing.T
	share  string
	run    string
	net    *fakeNet
	events []string
	specs  map[string]*oci.Spec
}

func (f *fakeRuntime) Run(_ context.Context, id, bundle string) (*oci.Watch, error) {
	b, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		f.t.Fatalf("Run(%s): no config.json: %v", id, err)
	}
	var s oci.Spec
	if err := json.Unmarshal(b, &s); err != nil {
		f.t.Fatal(err)
	}
	for _, m := range s.Mounts {
		if m.Destination == "/tmp" {
			fi, err := os.Stat(m.Source)
			if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSticky == 0 || fi.Mode().Perm() != 0o777 {
				f.t.Errorf("container %s binds %s at /tmp, which is not a sticky world-writable directory: %v", id, m.Source, err)
			}
			continue
		}
		if m.Type == "bind" && strings.HasPrefix(m.Source, f.run+"/") {
			if fi, err := os.Stat(m.Source); err != nil || !fi.Mode().IsRegular() {
				f.t.Errorf("container %s binds guest file %s, which is not a file: %v", id, m.Source, err)
			}
			continue
		}
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
	return nil, nil
}

func (f *fakeRuntime) Exec(context.Context, string, oci.Process, bool) error { return nil }
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
	l := initd.Layout{ShareDir: filepath.Join(d, "share"), RunDir: filepath.Join(d, "run"), GuestBinary: "/init",
		SysClassNet: sysNet(t, map[string]string{"lo": "00:00:00:00:00:00", "eth0": "52:54:00:12:34:56"})}
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
	m, rt := &fakeMounter{}, &fakeRuntime{t: t, share: l.ShareDir, run: l.RunDir}
	rt.net = &fakeNet{events: &rt.events}
	_, booted, err := initd.Boot(context.Background(), m, rt.net, rt, l, "herd")
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
