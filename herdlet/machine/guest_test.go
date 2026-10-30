package machine

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

type noMounts struct{}

func (noMounts) Mount(source, target, fstype string, flags uintptr, data string) error { return nil }

type runs struct{ bundles []string }

func (r *runs) Run(ctx context.Context, id, bundle string) error {
	r.bundles = append(r.bundles, bundle)
	return nil
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
