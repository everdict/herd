package machine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/everdict/herd/guest/initd"
	"github.com/everdict/herd/guest/oci"
)

// execs is crun as far as an exec goes: it keeps the process each exec was handed.
type execs struct {
	runs
	procs []oci.Process
}

func (e *execs) Exec(_ context.Context, _ string, p oci.Process, _ bool) error {
	e.procs = append(e.procs, p)
	return nil
}

var _ oci.Runtime = (*execs)(nil)

func capsIn(c *oci.Capabilities, name string) []string {
	var in []string
	if c == nil {
		return nil
	}
	for set, caps := range map[string][]string{"bounding": c.Bounding, "effective": c.Effective, "inheritable": c.Inheritable,
		"permitted": c.Permitted, "ambient": c.Ambient} {
		if slices.Contains(caps, name) {
			in = append(in, set)
		}
	}
	slices.Sort(in)
	return in
}

// DEFAUL-1611, herdlet to guest: a template container's capabilities.drop NET_RAW and add SYS_PTRACE reach the plan
// herdlet writes, and herd-guest's own boot of that share writes a config.json whose process holds CAP_NET_RAW in no
// set and CAP_SYS_PTRACE in all five, and an exec into the container (the shim every process starts through, and
// the signal helper) is handed the same set. The other container, with neither field, gets the default set.
func TestTemplateCapabilitiesReachTheContainerAndEveryExec(t *testing.T) {
	h := newHost(t, ubuntuCaps, registry())
	tpl := template()
	tpl.Containers[0].CapDrop = []string{"NET_RAW"}
	tpl.Containers[0].CapAdd = []string{"CAP_SYS_PTRACE"}
	if err := h.m.Boot(context.Background(), "actor-1", tpl); err != nil {
		t.Fatal(err)
	}
	share := h.m.ShareDir("actor-1")
	raw, err := os.ReadFile(filepath.Join(share, initd.PlanPath))
	if err != nil {
		t.Fatal(err)
	}
	p, err := initd.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c := p.Containers[0]; !slices.Equal(c.CapDrop, []string{"NET_RAW"}) || !slices.Equal(c.CapAdd, []string{"CAP_SYS_PTRACE"}) {
		t.Fatalf("plan container %s carries add %q drop %q, want the template's", c.Name, c.CapAdd, c.CapDrop)
	}

	l := initd.Layout{ShareDir: share, RunDir: t.TempDir(), GuestBinary: "/init"}
	vols, err := initd.Volumes(l, p)
	if err != nil {
		t.Fatal(err)
	}
	rt := &execs{}
	booted, err := initd.Containers(context.Background(), noMounts{}, rt, l, p, vols)
	if err != nil || len(booted) != 2 {
		t.Fatalf("Containers = %d, %v", len(booted), err)
	}
	all := []string{"ambient", "bounding", "effective", "inheritable", "permitted"}
	for i, b := range booted {
		raw, err := os.ReadFile(filepath.Join(b.Bundle, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var spec oci.Spec
		if err := json.Unmarshal(raw, &spec); err != nil {
			t.Fatal(err)
		}
		tg := b.Target(rt)
		if err := tg.StartShim(context.Background(), "/.herd/exec/x"); err != nil {
			t.Fatal(err)
		}
		if err := tg.Signal(context.Background(), 7, syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		procs := map[string]*oci.Capabilities{"container": spec.Process.Capabilities}
		for j, p := range rt.procs[2*i:] {
			procs[[]string{"shim exec", "signal exec"}[j]] = p.Capabilities
		}
		for what, c := range procs {
			raw, inPtrace, inChown := capsIn(c, "CAP_NET_RAW"), capsIn(c, "CAP_SYS_PTRACE"), capsIn(c, "CAP_CHOWN")
			if !slices.Equal(inChown, []string{"bounding", "effective", "permitted"}) {
				t.Errorf("%s %s: default CAP_CHOWN in %v", b.Name, what, inChown)
			}
			switch b.Name {
			case "agent":
				if len(raw) > 0 {
					t.Errorf("agent %s: CAP_NET_RAW in %v, want no set (the template dropped NET_RAW)", what, raw)
				}
				if !slices.Equal(inPtrace, all) {
					t.Errorf("agent %s: CAP_SYS_PTRACE in %v, want all five sets (the template added it)", what, inPtrace)
				}
			case "sidecar":
				if !slices.Equal(raw, []string{"bounding", "effective", "permitted"}) || len(inPtrace) > 0 {
					t.Errorf("sidecar %s, neither field: CAP_NET_RAW in %v, CAP_SYS_PTRACE in %v, want the default set", what, raw, inPtrace)
				}
			}
		}
	}
	if len(rt.procs) != 4 {
		t.Errorf("%d execs handed a process, want 4", len(rt.procs))
	}
}

// A template with neither field writes the plan it wrote before: no capability key at all, so a guest that predates
// the fields reads it.
func TestNoCapabilityFieldsLeaveThePlanAsBefore(t *testing.T) {
	h := newHost(t, ubuntuCaps, registry())
	if err := h.m.Boot(context.Background(), "actor-1", template()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(h.m.ShareDir("actor-1"), initd.PlanPath))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(raw); strings.Contains(s, `"capAdd"`) || strings.Contains(s, `"capDrop"`) {
		t.Errorf("plan of a template with no capabilities names one:\n%s", raw)
	}
}
