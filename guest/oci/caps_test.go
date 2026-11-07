package oci_test

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/everdict/herd/guest/oci"
)

// sets names a process's five capability sets, so a test can say which one holds a capability.
func sets(c *oci.Capabilities) map[string][]string {
	if c == nil {
		return nil
	}
	return map[string][]string{"bounding": c.Bounding, "effective": c.Effective, "inheritable": c.Inheritable,
		"permitted": c.Permitted, "ambient": c.Ambient}
}

// holdsIn answers the sets of c that hold capability name.
func holdsIn(c *oci.Capabilities, name string) []string {
	var in []string
	for set, caps := range sets(c) {
		if slices.Contains(caps, name) {
			in = append(in, set)
		}
	}
	slices.Sort(in)
	return in
}

// DEFAUL-1611: a template's drop NET_RAW leaves CAP_NET_RAW in no set of the container's process (the pool measured
// CapBnd 00000000a80425fb, NET_RAW kept, whatever the template said), in either spelling Kubernetes accepts, and the
// rest of the default set stays.
func TestDropNetRawHoldsItInNoSet(t *testing.T) {
	for _, name := range []string{"NET_RAW", "CAP_NET_RAW", "net_raw"} {
		c := mainConfig()
		c.CapDrop = []string{name}
		s, err := oci.Build(c)
		if err != nil {
			t.Fatalf("drop %s: %v", name, err)
		}
		if in := holdsIn(s.Process.Capabilities, "CAP_NET_RAW"); len(in) > 0 {
			t.Errorf("drop %s: CAP_NET_RAW still in %v", name, in)
		}
		want := slices.DeleteFunc(slices.Clone(oci.DefaultCapabilities), func(c string) bool { return c == "CAP_NET_RAW" })
		if got := s.Process.Capabilities.Bounding; !slices.Equal(got, want) {
			t.Errorf("drop %s: bounding %v, want the default set without CAP_NET_RAW %v", name, got, want)
		}
	}
}

// An added capability is held in all five sets, so a non-root USER keeps it across execve too; a capability both
// added and dropped is in none (drop wins); drop ALL empties the default set and leaves exactly what is added.
func TestAddIsHonouredAndDropWins(t *testing.T) {
	c := mainConfig()
	c.CapAdd = []string{"SYS_PTRACE", "CAP_NET_ADMIN"}
	s, err := oci.Build(c)
	if err != nil {
		t.Fatal(err)
	}
	all := []string{"ambient", "bounding", "effective", "inheritable", "permitted"}
	for _, name := range []string{"CAP_SYS_PTRACE", "CAP_NET_ADMIN"} {
		if in := holdsIn(s.Process.Capabilities, name); !slices.Equal(in, all) {
			t.Errorf("add %s: held in %v, want %v", name, in, all)
		}
	}
	if in := holdsIn(s.Process.Capabilities, "CAP_CHOWN"); !slices.Equal(in, []string{"bounding", "effective", "permitted"}) {
		t.Errorf("default CAP_CHOWN held in %v, want bounding, effective and permitted as before", in)
	}

	c = mainConfig()
	c.CapAdd, c.CapDrop = []string{"NET_ADMIN", "NET_RAW"}, []string{"CAP_NET_ADMIN", "NET_RAW"}
	if s, err = oci.Build(c); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CAP_NET_ADMIN", "CAP_NET_RAW"} {
		if in := holdsIn(s.Process.Capabilities, name); len(in) > 0 {
			t.Errorf("%s both added and dropped: held in %v, want none (drop wins)", name, in)
		}
	}

	c = mainConfig()
	c.CapAdd, c.CapDrop = []string{"NET_BIND_SERVICE"}, []string{"ALL"}
	if s, err = oci.Build(c); err != nil {
		t.Fatal(err)
	}
	for set, caps := range sets(s.Process.Capabilities) {
		if !slices.Equal(caps, []string{"CAP_NET_BIND_SERVICE"}) {
			t.Errorf("drop ALL, add NET_BIND_SERVICE: %s = %v, want exactly CAP_NET_BIND_SERVICE", set, caps)
		}
	}
}

// An unknown name is refused, naming it, as is ALL in add; Build refuses what CheckCapabilities refuses.
func TestUnknownCapabilityRefused(t *testing.T) {
	for _, tc := range []struct {
		add, drop []string
		bad       string
	}{
		{drop: []string{"NET_RAWR"}, bad: "NET_RAWR"},
		{add: []string{"CAP_SYS_PTRACE", "SYS_TIME_TRAVEL"}, bad: "SYS_TIME_TRAVEL"},
		{add: []string{"ALL"}, bad: "ALL"},
		{drop: []string{""}, bad: `""`},
		{add: []string{"CAP_"}, bad: "CAP_"},
	} {
		err := oci.CheckCapabilities(tc.add, tc.drop)
		if err == nil || !strings.Contains(err.Error(), tc.bad) {
			t.Errorf("CheckCapabilities(add %q, drop %q) = %v, want a refusal naming %s", tc.add, tc.drop, err, tc.bad)
		}
		c := mainConfig()
		c.CapAdd, c.CapDrop = tc.add, tc.drop
		if _, err := oci.Build(c); err == nil {
			t.Errorf("Build(add %q, drop %q) accepted", tc.add, tc.drop)
		}
	}
	if err := oci.CheckCapabilities([]string{"SYS_PTRACE", "cap_bpf", "CHECKPOINT_RESTORE"}, []string{"ALL", "CAP_NET_RAW"}); err != nil {
		t.Errorf("known names refused: %v", err)
	}
}

// A container with neither field gets exactly the set it got before: Docker's default as bounding, effective and
// permitted, nothing inheritable or ambient (testdata/config.golden.json pins the whole spec).
func TestNoCapabilityFieldsNoChange(t *testing.T) {
	s, err := oci.Build(mainConfig())
	if err != nil {
		t.Fatal(err)
	}
	c := s.Process.Capabilities
	if !slices.Equal(c.Bounding, oci.DefaultCapabilities) || !slices.Equal(c.Effective, oci.DefaultCapabilities) ||
		!slices.Equal(c.Permitted, oci.DefaultCapabilities) || c.Inheritable != nil || c.Ambient != nil {
		t.Errorf("capabilities %+v, want the default set as bounding, effective and permitted only", c)
	}
}

// Every exec into the container runs as the container's process does: crun exec is handed the container's own
// process (user, environment, capabilities, noNewPrivileges) with the exec's argv, so a dropped capability is in no
// set of an exec either.
func TestExecRunsWithTheContainersCapabilities(t *testing.T) {
	c := mainConfig()
	c.CapDrop, c.CapAdd = []string{"NET_RAW"}, []string{"SYS_PTRACE"}
	c.User = oci.User{UID: 1000, GID: 1000}
	s, err := oci.Build(c)
	if err != nil {
		t.Fatal(err)
	}
	var execs []oci.Process
	var calls []string
	rt := &oci.Crun{Runner: func(_ context.Context, _ []*os.File, name string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if i := slices.Index(args, "--process"); i >= 0 {
			b, err := os.ReadFile(args[i+1])
			if err != nil {
				t.Fatalf("crun exec --process: %v", err)
			}
			var p oci.Process
			if err := json.Unmarshal(b, &p); err != nil {
				t.Fatal(err)
			}
			execs = append(execs, p)
		}
		return nil, nil
	}}
	tg := &oci.Target{ContainerName: "main", Runtime: rt, Process: &s.Process}
	if err := tg.StartShim(context.Background(), "/.herd/exec/abc"); err != nil {
		t.Fatal(err)
	}
	if err := tg.Signal(context.Background(), 42, 2); err != nil {
		t.Fatal(err)
	}
	if len(execs) != 2 {
		t.Fatalf("crun calls %q: want two execs, each with --process", calls)
	}
	for i, argv := range [][]string{{oci.GuestBinary, "shim", "/.herd/exec/abc"}, {oci.GuestBinary, "signal", "42", "2"}} {
		p := execs[i]
		if !slices.Equal(p.Args, argv) {
			t.Errorf("exec %d runs %q, want %q", i, p.Args, argv)
		}
		if in := holdsIn(p.Capabilities, "CAP_NET_RAW"); len(in) > 0 {
			t.Errorf("exec %d: CAP_NET_RAW in %v, want no set (the template dropped it)", i, in)
		}
		if in := holdsIn(p.Capabilities, "CAP_SYS_PTRACE"); len(in) != 5 {
			t.Errorf("exec %d: added CAP_SYS_PTRACE in %v, want all five sets", i, in)
		}
		want := s.Process
		want.Args = argv
		gotJ, _ := json.Marshal(p)
		wantJ, _ := json.Marshal(want)
		if string(gotJ) != string(wantJ) {
			t.Errorf("exec %d process:\n%s\nwant the container's with the exec's argv:\n%s", i, gotJ, wantJ)
		}
	}
	if s.Process.Args[0] != oci.GuestBinary || s.Process.Args[1] != "cinit" {
		t.Errorf("an exec's argv leaked into the container's process: %q", s.Process.Args)
	}
}
