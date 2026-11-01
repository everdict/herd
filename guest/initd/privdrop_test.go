package initd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	"github.com/everdict/herd/guest/agent"
	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/guest/oci"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// privRuntime stands in for crun and the guest kernel where they decide whether an entrypoint that drops privileges
// survives. It runs the container's command (after herd-guest's cinit) as a model of two entrypoints:
//
//	drop-privileges <user> <argv...>   redis's docker-entrypoint.sh: chown the data dir, then su-exec <user> (setgroups,
//	                                   setgid, setuid); each needs its capability in the effective set, as the kernel
//	                                   checks it, and the user must be in the image's /etc/passwd
//	fail <code> <message>              prints message and exits with code
//
// anything else runs. A container that runs keeps its init's descriptors open until the test ends; one that ends
// writes its output and status on them as cinit does, so the Watch is the one Crun.Run would hand back.
type privRuntime struct {
	t     *testing.T
	specs map[string]*oci.Spec
}

func (p *privRuntime) Run(_ context.Context, id, bundle string) (*oci.Watch, error) {
	b, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return nil, err
	}
	var s oci.Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if p.specs == nil {
		p.specs = map[string]*oci.Spec{}
	}
	p.specs[id] = &s
	argv := s.Process.Args
	if i := slices.Index(argv, "--"); i >= 0 {
		argv = argv[i+1:]
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stR, stW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	w := oci.NewWatch(id, outR, stR)
	code, msg := p.run(&s, argv)
	if code < 0 {
		p.t.Cleanup(func() { outW.Close(); stW.Close() })
		return w, nil
	}
	io.WriteString(outW, msg+"\n")
	io.WriteString(stW, oci.StatusLine(code, 0))
	outW.Close()
	stW.Close()
	return w, nil
}

// run answers the command's exit code and output, or -1 while it keeps running.
func (p *privRuntime) run(s *oci.Spec, argv []string) (int, string) {
	var eff []string
	if s.Process.Capabilities != nil {
		eff = s.Process.Capabilities.Effective
	}
	has := func(c string) bool { return s.Process.User.UID == 0 && slices.Contains(eff, c) }
	switch {
	case len(argv) >= 3 && argv[0] == "drop-privileges":
		if !has("CAP_CHOWN") {
			return 1, "chown: /data: Operation not permitted"
		}
		if !has("CAP_SETGID") {
			return 1, "su-exec: setgroups: Operation not permitted"
		}
		if !has("CAP_SETUID") {
			return 1, "su-exec: setuid: Operation not permitted"
		}
		passwd, _ := os.ReadFile(filepath.Join(s.Root.Path, "etc", "passwd"))
		if !strings.Contains("\n"+string(passwd), "\n"+argv[1]+":") {
			return 1, "su-exec: getpwnam(" + argv[1] + "): No such file or directory"
		}
		return -1, ""
	case len(argv) >= 3 && argv[0] == "fail":
		var code int
		fmt.Sscan(argv[1], &code)
		return code, argv[2]
	}
	return -1, ""
}

func (p *privRuntime) Exec(_ context.Context, id string, _ []string, _ bool) error {
	return fmt.Errorf("oci: crun [exec --detach %s]: exit status 255: the container `%s` is not running", id, id)
}
func (p *privRuntime) Kill(context.Context, string, syscall.Signal) error { return nil }

const privPlan = `{
  "containers": [
    {"name": "box", "image": {"device": "/dev/vdb", "fsType": "erofs"},
     "command": ["drop-privileges", "redis", "redis-server"]},
    {"name": "web", "image": {"device": "/dev/vdc", "fsType": "erofs"},
     "command": ["nginx", "-g", "daemon off;"], "user": "nginx", "workdir": "/srv"},
    {"name": "bad", "image": {"device": "/dev/vdd", "fsType": "erofs"},
     "command": ["fail", "2", "redis: config missing"]}
  ]
}`

func writeImageUsers(t *testing.T, l initd.Layout, container string) {
	t.Helper()
	etc := filepath.Join(l.RunDir, "containers", container, "rootfs", "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(etc, "passwd"), []byte("root:x:0:0:root:/root:/bin/sh\nnginx:x:101:101:nginx:/var/cache/nginx:/sbin/nologin\nredis:x:999:1000::/home/redis:/sbin/nologin\n"), 0o644)
	os.WriteFile(filepath.Join(etc, "group"), []byte("root:x:0:root\nnginx:x:101:nginx\nredis:x:1000:redis\n"), 0o644)
}

// TestEntrypointThatDropsPrivilegesKeepsRunning (PLT-230, guest): an image whose entrypoint chowns and then switches
// user (redis's docker-entrypoint.sh: chown, su-exec redis) keeps running in herd's guest, and an image's USER is
// resolved in its own rootfs; a container whose main process ends is reported — its status and last output in the
// share for the host, and exec into it answers FAILED_PRECONDITION naming the exit, not crun's bare "not running".
func TestEntrypointThatDropsPrivilegesKeepsRunning(t *testing.T) {
	l := layout(t)
	for _, c := range []string{"box", "web", "bad"} {
		writeImageUsers(t, l, c)
	}
	if err := os.WriteFile(filepath.Join(l.ShareDir, initd.PlanPath), []byte(privPlan), 0o600); err != nil {
		t.Fatal(err)
	}
	// An exit recorded by an earlier boot of a kept share is not this boot's.
	if err := os.WriteFile(filepath.Join(l.ShareDir, initd.ExitPath), []byte("container old exited with status 9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := &privRuntime{t: t}
	_, booted, err := initd.Boot(context.Background(), &fakeMounter{}, &fakeNet{}, rt, l, "herd")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reported := make(chan error, len(booted))
	var targets []agent.Target
	for _, b := range booted {
		targets = append(targets, b.Target(rt))
		go func() { reported <- initd.ReportExit(ctx, l, b) }()
	}

	box, web, bad := targets[0].(*oci.Target), targets[1].(*oci.Target), targets[2].(*oci.Target)
	if err := <-reported; err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := box.Exited(); err != nil {
		t.Fatalf("redis-like entrypoint (chown, su-exec redis) did not keep running: %v", err)
	}
	if err := web.Exited(); err != nil {
		t.Fatalf("container with USER nginx did not keep running: %v", err)
	}
	if u := rt.specs["web"].Process.User; u.UID != 101 || u.GID != 101 {
		t.Errorf("USER nginx ran as %+v, want 101:101 from the image's /etc/passwd", u)
	}
	if uid, gid := web.Owner(); uid != 101 || gid != 101 {
		t.Errorf("exec records of the USER nginx container go to %d:%d, want 101:101", uid, gid)
	}
	if cwd := rt.specs["web"].Process.Cwd; cwd != "/srv" {
		t.Errorf("WORKDIR /srv ran in %q", cwd)
	}

	b, err := os.ReadFile(filepath.Join(l.ShareDir, initd.ExitPath))
	if err != nil {
		t.Fatalf("the exit of container bad is not in the share: %v", err)
	}
	if got := string(b); got != "container bad exited with status 2; its last output:\nredis: config missing\n" {
		t.Errorf("share %s = %q, want only this boot's exit of bad with its status and output", initd.ExitPath, got)
	}

	g, err := agent.New(targets...)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	_, err = g.StartProcess(ctx, func(k string) string {
		if k == agent.ContainerHeader {
			return "bad"
		}
		return ""
	}, &ateenv.StartProcessRequest{Command: []string{"redis-cli", "ping"}})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "container bad exited with status 2") ||
		!strings.Contains(err.Error(), "redis: config missing") {
		t.Errorf("exec into the exited container: %v, want FAILED_PRECONDITION naming its exit and last output", err)
	}
	if err := bad.Exited(); err == nil {
		t.Error("the exited container reports no exit")
	}
}
