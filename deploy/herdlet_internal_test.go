package deploy

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/join"
	"github.com/everdict/herd/snapstore/disk"
	"github.com/everdict/herd/snapstore/s3"
)

func TestParseFlags(t *testing.T) {
	l, err := parseLabels("host=mac,os=darwin")
	if err != nil || l["host"] != "mac" || l["os"] != "darwin" {
		t.Fatalf("parseLabels = %v, %v", l, err)
	}
	if _, err := parseLabels("nokey"); err == nil {
		t.Error("parseLabels accepted a label without =")
	}
	w, err := parseWarm("tpl-a=2,tpl-b=0")
	if err != nil || len(w) != 2 || w[0].UID != "tpl-a" || w[0].Warm != 2 || len(w[0].Containers) != 0 {
		t.Fatalf("parseWarm = %+v, %v", w, err)
	}
	for _, bad := range []string{"tpl", "tpl=-1", "=2", "tpl=x"} {
		if _, err := parseWarm(bad); err == nil {
			t.Errorf("parseWarm(%q) accepted", bad)
		}
	}
}

// TestHerdletKeepsR2cCommandLines: the command lines R2c's herdlet took (bare `herdlet` with a herdlet credential,
// and `herdlet doctor`) still parse, with every flag it had, and `herdlet run` takes the same.
func TestHerdletKeepsR2cCommandLines(t *testing.T) {
	r2c := []string{"-api", "10.0.0.1:7443", "-router", "10.0.0.1:7444", "-token-file", "/etc/herd/node.jwt", "-worker", "node-1",
		"-labels", "host=ubuntu", "-slots", "4", "-state", "/var/lib/herdlet", "-snapshots", "/srv/herd/snapshots",
		"-kernel", "/usr/share/herd/vmlinux", "-initrd", "/usr/share/herd/initramfs", "-cmdline", "console=hvc0",
		"-vcpus", "2", "-memory-mib", "1024", "-warm", "tpl=2", "-guest-port", "8080", "-cloud-hypervisor", "/usr/local/bin/cloud-hypervisor",
		"-virtiofsd", "/usr/libexec/virtiofsd", "-virtiofsd-args", "--sandbox=none", "-host", "mac-1"}
	for _, args := range [][]string{r2c, append([]string{"run"}, r2c...), append([]string{"doctor"}, r2c...)} {
		if _, err := Parse("herdlet", args, io.Discard); err != nil {
			t.Errorf("herdlet %v: %v", args[:1], err)
		}
	}
}

// TestGrantStoreWinsOverSnapshotsFlag (criterion no-local-override): with a grant that names an S3 store, -snapshots
// is ignored with a warning that names it, the store is the grant's and no directory is made; without one,
// -snapshots is the store.
func TestGrantStoreWinsOverSnapshotsFlag(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	dir := filepath.Join(t.TempDir(), "snapshots")
	h := &Herdlet{StateDir: t.TempDir(), Snapshots: dir}
	g := join.Grant{Snapshots: &s3.Config{Endpoint: "http://127.0.0.1:9", Bucket: "herd", AccessKeyID: "k", SecretAccessKey: "s"}}
	s, err := h.snapshots(g)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(*s3.Store); !ok {
		t.Errorf("with the join's S3 store and -snapshots %s, the herdlet's store is %T", dir, s)
	}
	if want := "-snapshots " + dir + " ignored"; !strings.Contains(logs.String(), want) {
		t.Errorf("log %q lacks %q", logs.String(), want)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("-snapshots %s was made: %v", dir, err)
	}
	if s, err := h.snapshots(join.Grant{}); err != nil {
		t.Fatal(err)
	} else if _, ok := s.(*disk.Store); !ok {
		t.Errorf("without a store in the grant, the herdlet's store is %T, want -snapshots' directory", s)
	}
}

// TestHerdletRefusesAVirtiofsdThatRejectsHerdsArguments (criterion doctor): a herdlet given a virtiofsd 1.10 does not
// join or serve: Run stops at the preflight with doctor's reason, before it dials herd-api, and leaves no grant. Before
// the check it joined and failed every boot instead: red.
func TestHerdletRefusesAVirtiofsdThatRejectsHerdsArguments(t *testing.T) {
	help := func(version string) Host {
		b, err := os.ReadFile("testdata/virtiofsd-" + version + ".help")
		if err != nil {
			t.Fatal(err)
		}
		return Host{GOOS: "linux", GOARCH: "amd64",
			OpenRW:   func(string) error { return nil },
			LookPath: func(n string) (string, error) { return n, nil },
			Stat:     func(string) error { return nil },
			Help:     func(string) (string, error) { return string(b), nil },
		}
	}
	old := thisHost
	t.Cleanup(func() { thisHost = old })
	state := t.TempDir()
	h := &Herdlet{CHBinary: "cloud-hypervisor", Virtiofsd: "/usr/libexec/virtiofsd", Kernel: "k", Initrd: "i",
		Worker: "node-1", StateDir: state, API: "127.0.0.1:1", Token: "join-token"}
	thisHost = func() Host { return help("1.10.1") }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := h.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "cannot run herd actors") || !strings.Contains(err.Error(), "--migration-mode") {
		t.Errorf("herdlet with virtiofsd 1.10.1: Run = %v, want the preflight's refusal naming --migration-mode", err)
	}
	if ents, _ := os.ReadDir(state); len(ents) != 0 {
		t.Errorf("herdlet with virtiofsd 1.10.1 left %d entries in its state directory", len(ents))
	}
	thisHost = func() Host { return help("1.13.2") }
	if err := h.preflight(); err != nil {
		t.Errorf("herdlet with virtiofsd 1.13.2: preflight = %v", err)
	}
}

// TestHerdletRefusesAHostWhereVMsCannotLeaveItsCgroup (PLT-214): the herdlet's flags place each VM in a cgroup of its
// own under herd's by default, and its preflight (doctor) refuses to join a host where that cgroup cannot be made,
// rather than starting VMs whose memory its own task or unit limit would OOM-kill.
func TestHerdletRefusesAHostWhereVMsCannotLeaveItsCgroup(t *testing.T) {
	h := &Herdlet{}
	fs := flag.NewFlagSet("herdlet", flag.ContinueOnError)
	h.HostFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if h.VMCgroup != "herd" || h.VMMemoryOverheadMiB != 512 {
		t.Fatalf("defaults: -vm-cgroup %q, -vm-memory-overhead %d", h.VMCgroup, h.VMMemoryOverheadMiB)
	}
	h.CHBinary, h.Virtiofsd, h.Kernel, h.Initrd = "cloud-hypervisor", "virtiofsd", "k", "i"
	var asked []string
	cgroupErr := errors.New("cgroup: /sys/fs/cgroup is not a cgroup v2 hierarchy")
	old := thisHost
	t.Cleanup(func() { thisHost = old })
	thisHost = func() Host {
		return Host{GOOS: "linux", GOARCH: "amd64",
			OpenRW:   func(string) error { return nil },
			LookPath: func(n string) (string, error) { return n, nil },
			Stat:     func(string) error { return nil },
			VMCgroup: func(parent string) error { asked = append(asked, parent); return cgroupErr },
		}
	}
	if err := h.preflight(); err == nil || !strings.Contains(err.Error(), "vm cgroup") || !strings.Contains(err.Error(), "cgroup v2") {
		t.Errorf("preflight on a host without a writable cgroup v2 hierarchy = %v", err)
	}
	if strings.Join(asked, ",") != "herd" {
		t.Errorf("doctor checked parents %q", asked)
	}
	cgroupErr = nil
	if err := h.preflight(); err != nil {
		t.Errorf("preflight on a host that can place VMs: %v", err)
	}
	h.VMCgroup = ""
	cgroupErr = errors.New("unused")
	if err := h.preflight(); err != nil {
		t.Errorf("preflight with -vm-cgroup off: %v", err)
	}
}
