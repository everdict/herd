package deploy_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/deploy"
	"github.com/everdict/herd/vmm/fake"
	"github.com/everdict/herd/vmm/fakeguest"
)

// TestSessionsReadTheHostsClock (DEFAUL-1595): on a herdlet as `herdlet run` builds it (deploy's machines, whose guest
// clock is herd-guest's SetClock), over VMs that run the real herd-guest and whose MEMORY restores run on from their
// snapshot's clock, every actor reads the host's wall clock within a second once herd-api answers it RUNNING:
//   - a session from a golden built three days ago,
//   - the same session resumed 12 minutes after its suspend,
//   - a fork of it from a tag, and the source the tag's snapshot paused for 20 s.
//
// Its counterexample is herd at b553a7b: the session reads the golden's clock, three days behind, the bug measured
// live 12 minutes behind on 2026-11-06.
func TestSessionsReadTheHostsClock(t *testing.T) {
	a := startAPI(t)
	tok, err := (&deploy.JoinToken{KeyFile: filepath.Join(a.dir, "jwt.key"), TTL: time.Hour}).Mint()
	if err != nil {
		t.Fatal(err)
	}
	vms, err := os.MkdirTemp("", "dclk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(vms) })
	v := fakeguest.New(fake.New(chCaps(), nil), vms, nil)
	t.Cleanup(v.Close)
	h := herdletOn(t, "node-1", nil)
	h.VMM, h.Guests = v, v
	h.API, h.Token, h.Snapshots, h.Slots = a.addr, tok, filepath.Join(a.dir, "snapshots"), 4
	runHerdlet(t, h)
	ctl := writeToken(t, a.key)
	waitWorkers(t, a.addr, ctl, 1)

	c := conformance.NewClient(a.addr, ctl)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ref := func(name string) *ateapi.ObjectRef { return &ateapi.ObjectRef{Atespace: space, Name: name} }
	if _, err := c.CreateActorTemplate(ctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata:   &ateapi.ResourceMetadata{Atespace: space, Name: "agent"},
		Containers: []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
			GoldenSnapshotConfig: &ateapi.GoldenSnapshotConfig{Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED}},
	}}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		tpl, err := c.GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{ActorTemplate: ref("agent")})
		if err != nil {
			t.Fatalf("GetActorTemplate: %v", err)
		}
		gs := tpl.GetStatus().GetGoldenSnapshotStatus()
		if gs.GetErrorMessage() != "" {
			t.Fatalf("golden build: %s", gs.GetErrorMessage())
		}
		if gs.GetGoldenTag() != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no golden tag within 20 s")
		}
	}
	onHostClock := func(act *ateapi.Actor, what string) {
		t.Helper()
		if s := act.GetStatus().GetState(); s != ateapi.ActorState_ACTOR_STATE_RUNNING {
			t.Fatalf("%s: actor %s is %v, want RUNNING", what, act.GetMetadata().GetName(), s)
		}
		if d := time.Since(v.Clock(act.GetMetadata().GetUid())); d > time.Second || d < -time.Second {
			t.Fatalf("%s: actor %s's guest reads %v behind the host's wall clock, want within a second", what, act.GetMetadata().GetName(), d.Round(time.Second))
		}
	}

	v.AgeSnapshots(72 * time.Hour)
	s1, err := c.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: "s1"}, ActorTemplate: ref("agent")}})
	if err != nil {
		t.Fatalf("CreateActor from the golden: %v", err)
	}
	onHostClock(s1, "from a golden built 72h ago")

	if _, err := c.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: ref("s1")}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	v.AgeSnapshots(12 * time.Minute)
	r, err := c.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref("s1")})
	if err != nil {
		t.Fatalf("ResumeActor: %v", err)
	}
	onHostClock(r.GetActor(), "resumed 12m after its suspend")

	v.PauseLag = 20 * time.Second
	if _, err := c.CreateTag(ctx, &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: "t1"}, SourceActor: ref("s1")}}); err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	src, err := c.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref("s1")})
	if err != nil {
		t.Fatal(err)
	}
	onHostClock(src, "the source a tag's snapshot paused for 20s")
	fork, err := c.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: "fork"}, ActorTemplate: ref("agent"), SourceTag: ref("t1")}})
	if err != nil {
		t.Fatalf("CreateActor from the tag: %v", err)
	}
	onHostClock(fork, "a fork")
}
