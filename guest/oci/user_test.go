package oci_test

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/everdict/herd/guest/oci"
)

// TestSpecGrantsDockerDefaultCapabilities (PLT-230): the container's process gets Docker's default capability set as
// its bounding, effective and permitted sets, with nothing inheritable or ambient, and keeps noNewPrivileges. Without
// a capabilities object crun grants none, and root in the container can neither chown nor switch user: redis's
// entrypoint (chown, su-exec redis) and nginx's workers (setuid nginx) die right after boot.
func TestSpecGrantsDockerDefaultCapabilities(t *testing.T) {
	s, err := oci.Build(mainConfig())
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// Read back as crun reads config.json.
	var raw struct {
		Process struct {
			Capabilities    map[string][]string `json:"capabilities"`
			NoNewPrivileges bool                `json:"noNewPrivileges"`
		} `json:"process"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	docker := []string{"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FSETID", "CAP_FOWNER", "CAP_MKNOD", "CAP_NET_RAW", "CAP_SETGID",
		"CAP_SETUID", "CAP_SETFCAP", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE", "CAP_SYS_CHROOT", "CAP_KILL", "CAP_AUDIT_WRITE"}
	caps := raw.Process.Capabilities
	if caps == nil {
		t.Fatalf("config.json has no process.capabilities: crun grants the container no capability at all")
	}
	for _, set := range []string{"bounding", "effective", "permitted"} {
		if !slices.Equal(caps[set], docker) {
			t.Errorf("capabilities.%s = %v, want Docker's default set %v", set, caps[set], docker)
		}
	}
	for _, set := range []string{"inheritable", "ambient"} {
		if len(caps[set]) != 0 {
			t.Errorf("capabilities.%s = %v, want empty (as Docker)", set, caps[set])
		}
	}
	if !raw.Process.NoNewPrivileges {
		t.Error("noNewPrivileges is off; the template asks for nothing else")
	}
}

// An Alpine-like image: root in the usual groups, redis (999:1000, in its group and in "www"), nginx (101:101).
func imageRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(root, "etc", name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("passwd", `root:x:0:0:root:/root:/bin/sh
bin:x:1:1:bin:/bin:/sbin/nologin
# a comment
nginx:x:101:101:nginx:/var/cache/nginx:/sbin/nologin
redis:x:999:1000:Linux User,,,:/home/redis:/sbin/nologin
`)
	write("group", `root:x:0:root
bin:x:1:root,bin,daemon
wheel:x:10:root
nginx:x:101:nginx
redis:x:1000:redis
www:x:33:nginx,redis
`)
	return root
}

// TestResolveUserAsDocker (PLT-230): the image's USER is resolved against its own /etc/passwd and /etc/group as
// Docker does (moby/sys/user GetExecUser), and the spec's process runs as the answer.
func TestResolveUserAsDocker(t *testing.T) {
	root := imageRoot(t)
	for _, c := range []struct {
		spec string
		want oci.User
	}{
		{"", oci.User{UID: 0, GID: 0, AdditionalGids: []uint32{0, 1, 10}}},
		{"root", oci.User{UID: 0, GID: 0, AdditionalGids: []uint32{0, 1, 10}}},
		{"redis", oci.User{UID: 999, GID: 1000, AdditionalGids: []uint32{1000, 33}}},
		{"999", oci.User{UID: 999, GID: 1000, AdditionalGids: []uint32{1000, 33}}},
		{"nginx:www", oci.User{UID: 101, GID: 33}},
		{"redis:101", oci.User{UID: 999, GID: 101}},
		{"1234", oci.User{UID: 1234, GID: 0}},
		{"1234:5678", oci.User{UID: 1234, GID: 5678}},
	} {
		got, err := oci.ResolveUser(root, c.spec)
		if err != nil {
			t.Errorf("USER %q: %v", c.spec, err)
			continue
		}
		if got.UID != c.want.UID || got.GID != c.want.GID || !slices.Equal(got.AdditionalGids, c.want.AdditionalGids) {
			t.Errorf("USER %q = %+v, want %+v", c.spec, got, c.want)
		}
	}
	for _, spec := range []string{"postgres", "redis:nosuch"} {
		if u, err := oci.ResolveUser(root, spec); err == nil {
			t.Errorf("USER %q resolved to %+v; Docker refuses a name the image does not have", spec, u)
		}
	}
	// No /etc/passwd at all (a scratch image): numeric users still run, root is 0:0.
	empty := t.TempDir()
	if u, err := oci.ResolveUser(empty, "65532:65532"); err != nil || u.UID != 65532 || u.GID != 65532 {
		t.Errorf("USER 65532:65532 in a scratch image = %+v, %v", u, err)
	}
	if u, err := oci.ResolveUser(empty, ""); err != nil || u.UID != 0 || u.GID != 0 || len(u.AdditionalGids) != 0 {
		t.Errorf("no USER in a scratch image = %+v, %v", u, err)
	}

	c := mainConfig()
	c.User, _ = oci.ResolveUser(root, "redis")
	s, err := oci.Build(c)
	if err != nil {
		t.Fatal(err)
	}
	if p := s.Process.User; p.UID != 999 || p.GID != 1000 || !slices.Equal(p.AdditionalGids, []uint32{1000, 33}) {
		t.Errorf("spec process.user = %+v, want redis's 999:1000 with groups [1000 33]", p)
	}
}

// TestResolveUserStaysInTheImage: /etc/passwd as a symlink out of the rootfs is not followed out of it.
func TestResolveUserStaysInTheImage(t *testing.T) {
	outside := imageRoot(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "etc", "passwd"), filepath.Join(root, "etc", "passwd")); err != nil {
		t.Fatal(err)
	}
	if u, err := oci.ResolveUser(root, "redis"); err == nil {
		t.Errorf("USER redis resolved to %+v through a symlink out of the image", u)
	}
}

func pipes(t *testing.T) (outR, outW, stR, stW *os.File) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stR, stW, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	return outR, outW, stR, stW
}

func waitDone(t *testing.T, w *oci.Watch) *oci.ContainerExit {
	t.Helper()
	select {
	case <-w.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not learn the exit")
	}
	return w.Exited()
}

// TestWatchReportsExitAndLastOutput (PLT-230): what cinit reports — the command's output and its status — is how
// the container ended: the status, and the last TailLines lines of output, bounded in bytes.
func TestWatchReportsExitAndLastOutput(t *testing.T) {
	outR, outW, stR, stW := pipes(t)
	w := oci.NewWatch("box", outR, stR)
	for i := 0; i < 100; i++ {
		io.WriteString(outW, "line "+strings.Repeat("x", i%7)+"\n")
	}
	io.WriteString(outW, "su-exec: setgroups(0): Operation not permitted\n")
	if e := w.Exited(); e != nil {
		t.Fatalf("Exited() = %v while the init runs", e)
	}
	io.WriteString(stW, oci.StatusLine(1, 0))
	outW.Close()
	stW.Close()
	e := waitDone(t, w)
	if e == nil || !e.Known || e.Code != 1 || e.Signal != 0 {
		t.Fatalf("exit = %+v, want status 1", e)
	}
	if len(e.Output) != oci.TailLines || e.Output[len(e.Output)-1] != "su-exec: setgroups(0): Operation not permitted" {
		t.Errorf("output = %q, want the last %d lines ending with su-exec's", e.Output, oci.TailLines)
	}
	if msg := e.Error(); !strings.HasPrefix(msg, "container box exited with status 1; its last output:\n") || !strings.Contains(msg, "Operation not permitted") {
		t.Errorf("message %q", msg)
	}

	// One very long line is bounded in bytes.
	outR, outW, stR, stW = pipes(t)
	w = oci.NewWatch("box", outR, stR)
	io.WriteString(outW, strings.Repeat("y", 100<<10))
	io.WriteString(stW, oci.StatusLine(0, syscall.SIGKILL))
	outW.Close()
	stW.Close()
	e = waitDone(t, w)
	if e.Signal != syscall.SIGKILL || !strings.Contains(e.Error(), "was killed by signal 9") {
		t.Errorf("exit = %v, want killed by signal 9", e)
	}
	if n := len(strings.Join(e.Output, "\n")); n > oci.TailBytes {
		t.Errorf("kept %d bytes of output, want at most %d", n, oci.TailBytes)
	}

	// An init that ends without a status (killed outright) is still an exit, of unknown status.
	outR, outW, stR, stW = pipes(t)
	w = oci.NewWatch("box", outR, stR)
	outW.Close()
	stW.Close()
	if e := waitDone(t, w); e == nil || e.Known || !strings.Contains(e.Error(), "ended without reporting a status; it wrote no output") {
		t.Errorf("exit = %v, want an unknown status", e)
	}
}
