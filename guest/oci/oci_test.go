package oci_test

import (
	"bytes"
	"context"
	"flag"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/everdict/herd/guest/oci"
)

var update = flag.Bool("update", false, "rewrite testdata/config.golden.json")

func mainConfig() oci.Config {
	return oci.Config{
		Name:        "main",
		Rootfs:      "/run/herd/containers/main/rootfs",
		Args:        []string{"claude", "--print"},
		Env:         []string{"HOME=/home/agent"},
		Cwd:         "/workspace",
		ShareDir:    "/mnt/volumes",
		GuestBinary: "/init",
		Volumes: []oci.VolumeMount{
			{Source: "/mnt/volumes/workspace", Destination: "/workspace"},
			{Source: "/mnt/volumes/home", Destination: "/home/agent", ReadOnly: true},
		},
	}
}

func TestSpecGolden(t *testing.T) {
	s, err := oci.Build(mainConfig())
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile("testdata/config.golden.json", got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("testdata/config.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("config.json differs from testdata/config.golden.json (go test ./guest/oci -update to accept):\n%s", got)
	}
}

func TestSpecRefusesVolumeOutsideTheShare(t *testing.T) {
	for _, src := range []string{"/etc", "/mnt/volumes", "/mnt/volumes/../etc", "/mnt/volumesX/w", "relative"} {
		c := mainConfig()
		c.Volumes = []oci.VolumeMount{{Source: src, Destination: "/w"}}
		if _, err := oci.Build(c); err == nil {
			t.Errorf("volume source %q accepted", src)
		}
	}
}

func TestCrunCommands(t *testing.T) {
	var calls []string
	rt := &oci.Crun{Runner: func(_ context.Context, _ []*os.File, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}}
	tg := &oci.Target{ContainerName: "main", Runtime: rt}
	ctx := context.Background()
	rt.Run(ctx, "main", "/run/herd/containers/main")
	tg.StartShim(ctx, "/everdict/exec/abc")
	tg.Signal(ctx, 42, syscall.SIGINT)
	rt.Kill(ctx, "main", syscall.SIGTERM)
	want := []string{
		"/sbin/crun --root /run/crun run --detach --preserve-fds 2 --bundle /run/herd/containers/main main",
		"/sbin/crun --root /run/crun exec --detach main /.herd/herd-guest shim /everdict/exec/abc",
		"/sbin/crun --root /run/crun exec main /.herd/herd-guest signal 42 2",
		"/sbin/crun --root /run/crun kill main 15",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("crun calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestSpecFileMounts(t *testing.T) {
	c := mainConfig()
	c.Files = []oci.FileMount{{Source: "/run/herd/net/resolv.conf", Destination: "/etc/resolv.conf"}}
	s, err := oci.Build(c)
	if err != nil {
		t.Fatal(err)
	}
	last := s.Mounts[len(s.Mounts)-1]
	want := oci.Mount{Destination: "/etc/resolv.conf", Type: "bind", Source: "/run/herd/net/resolv.conf", Options: []string{"bind", "ro", "nosuid", "nodev"}}
	if last.Destination != want.Destination || last.Type != want.Type || last.Source != want.Source || strings.Join(last.Options, ",") != strings.Join(want.Options, ",") {
		t.Errorf("file mount = %+v, want %+v", last, want)
	}
	for _, f := range []oci.FileMount{
		{Source: "run/herd/net/resolv.conf", Destination: "/etc/resolv.conf"},
		{Source: "/run/herd/../etc/shadow", Destination: "/etc/resolv.conf"},
		{Source: "/run/herd/net/resolv.conf", Destination: "etc/resolv.conf"},
		{Source: "/run/herd/net/resolv.conf", Destination: "/etc/../resolv.conf"},
		{Source: "/run/herd/net/resolv.conf", Destination: "/"},
		{Source: "/run/herd/net/resolv.conf", Destination: "/.herd/herd-guest"},
		{Source: "/run/herd/net/resolv.conf", Destination: "/workspace"},
	} {
		c := mainConfig()
		c.Files = []oci.FileMount{f}
		if _, err := oci.Build(c); err == nil {
			t.Errorf("file mount %+v accepted", f)
		}
	}
}

// A tmpfs at /tmp exists only in the container's mount namespace, so the guest's file API wrote beneath it and a process
// never saw the file. /tmp is a bind of a guest directory the file API can map, as a volume is.
func TestTmpIsABindOfTheGivenDirectory(t *testing.T) {
	c := mainConfig()
	c.TmpDir = "/run/herd/containers/box/tmp"
	s, err := oci.Build(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range s.Mounts {
		if m.Destination == "/tmp" {
			if m.Type != "bind" || m.Source != c.TmpDir {
				t.Fatalf("/tmp mount = %+v, want a bind of %s", m, c.TmpDir)
			}
			return
		}
	}
	t.Fatal("no mount at /tmp")
}
