package machine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	"github.com/everdict/herd/guest/agent"
	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/guest/oci"
	"github.com/everdict/herd/vmm"
)

type noMounts struct{}

func (noMounts) Mount(source, target, fstype string, flags uintptr, data string) error { return nil }

type runs struct{ bundles []string }

func (r *runs) Run(ctx context.Context, id, bundle string) (*oci.Watch, error) {
	r.bundles = append(r.bundles, bundle)
	return nil, nil
}
func (r *runs) Exec(context.Context, string, []string, bool) error { return nil }
func (r *runs) Kill(context.Context, string, syscall.Signal) error { return nil }

var _ oci.Runtime = (*runs)(nil)

// TestGuestBootsTheHerdletShare hands the share herdlet laid out to herd-guest's own boot sequence (R5 initd, with the
// share directory standing in for the virtio-fs mount): both containers start, and each binds the volume from the
// share's directory.
func TestGuestBootsTheHerdletShare(t *testing.T) {
	h := newHost(t, ubuntuCaps, registry())
	if err := h.m.Boot(context.Background(), "actor-1", template()); err != nil {
		t.Fatal(err)
	}
	l := initd.Layout{ShareDir: h.m.ShareDir("actor-1"), RunDir: t.TempDir(), GuestBinary: "/init"}
	p, err := initd.ReadPlan(l)
	if err != nil {
		t.Fatal(err)
	}
	vols, err := initd.Volumes(l, p)
	if err != nil {
		t.Fatal(err)
	}
	rt := &runs{}
	booted, err := initd.Containers(context.Background(), noMounts{}, rt, l, p, vols)
	if err != nil || len(booted) != 2 {
		t.Fatalf("Containers = %d, %v", len(booted), err)
	}
	for _, b := range booted {
		raw, err := os.ReadFile(filepath.Join(b.Bundle, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var spec oci.Spec
		if err := json.Unmarshal(raw, &spec); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, m := range spec.Mounts {
			if m.Destination == "/data" {
				found = true
				if m.Source != filepath.Join(l.ShareDir, "data") || !strings.HasPrefix(m.Source, l.ShareDir+"/") {
					t.Errorf("%s: /data bound from %s, not the share's volume", b.Name, m.Source)
				}
			}
		}
		if !found {
			t.Errorf("%s: no bind of the volume", b.Name)
		}
	}
}

// guestOn runs herd-guest's boot sequence on the actor's share (the share directory standing in for the virtio-fs
// mount, an empty directory for each image) and serves its API on the targets herd-guest would serve.
func guestOn(t *testing.T, m *Machines, id string) *agent.Guest {
	t.Helper()
	l := initd.Layout{ShareDir: m.ShareDir(id), RunDir: t.TempDir(), GuestBinary: "/init"}
	_, booted, err := initd.Boot(context.Background(), noMounts{}, nil, &runs{}, l, ShareTag)
	if err != nil {
		t.Fatal(err)
	}
	var targets []agent.Target
	for _, b := range booted {
		targets = append(targets, b.Target(&runs{}))
	}
	g, err := agent.New(targets...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	return g
}

func writeFile(g *agent.Guest, p, body string) error {
	sent := false
	_, err := g.WriteFile(func(string) string { return "" }, func() (*ateenv.WriteFileRequest, error) {
		if sent {
			return nil, io.EOF
		}
		sent = true
		return &ateenv.WriteFileRequest{Path: p, Chunk: []byte(body)}, nil
	})
	return err
}

func readFile(g *agent.Guest, p string) (string, error) {
	var b strings.Builder
	err := g.ReadFile(func(string) string { return "" }, &ateenv.ReadFileRequest{Path: p}, func(r *ateenv.ReadFileResponse) error {
		b.Write(r.GetChunk())
		return nil
	})
	return b.String(), err
}

// TestGuestFileWritesReachForks is the real-host fork-via-tag run with herd-guest's file API in the loop: the parent
// writes /data/notes through the API, is suspended at MEMORY and released, and two forks are restored from its
// bundle. The second cannot have the parent's VM id, so it cold-boots from the bundle's volume tree, which holds the
// file only if the API wrote it into the volume (the share) rather than into the container's rootfs, where the
// guest sees the mount point without the container's bind. Each fork's guest reads the parent's file, and their
// writes stay apart.
func TestGuestFileWritesReachForks(t *testing.T) {
	ctx := context.Background()
	h := newHost(t, ubuntuCaps, registry())
	if err := h.m.Boot(ctx, "parent", template()); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(guestOn(t, h.m, "parent"), "/data/notes", "parent"); err != nil {
		t.Fatalf("WriteFile in the parent: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(h.m.ShareDir("parent"), "data", "notes")); err != nil || string(b) != "parent" {
		t.Errorf("the parent's share holds %q, %v after the guest wrote /data/notes; want the write in the volume", b, err)
	}
	var bundle bytes.Buffer
	if _, err := h.m.Snapshot(ctx, "parent", vmm.FidelityMemory, true, &bundle); err != nil {
		t.Fatal(err)
	}
	if p, err := h.m.Delete(ctx, "parent"); p != vmm.PresenceGone {
		t.Fatalf("release: %v, %v", p, err)
	}
	forks := []string{"fork-1", "fork-2"}
	guests := map[string]*agent.Guest{}
	for _, id := range forks {
		if err := h.m.Restore(ctx, id, bytes.NewReader(bundle.Bytes())); err != nil {
			t.Fatalf("Restore %s: %v", id, err)
		}
		guests[id] = guestOn(t, h.m, id)
		if got, err := readFile(guests[id], "/data/notes"); err != nil || got != "parent" {
			t.Errorf("fork %s reads %q, %v from /data/notes; want the parent's %q", id, got, err, "parent")
		}
		if err := writeFile(guests[id], "/data/notes", id); err != nil {
			t.Fatalf("WriteFile in %s: %v", id, err)
		}
	}
	for _, id := range forks {
		if got, err := readFile(guests[id], "/data/notes"); err != nil || got != id {
			t.Errorf("fork %s reads %q, %v after both wrote; want its own write", id, got, err)
		}
	}
}
