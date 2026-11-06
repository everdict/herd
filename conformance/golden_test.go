package conformance_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc/codes"
)

// workLog records the work a herdlet was asked for.
type workLog struct {
	mu   sync.Mutex
	work []link.Work
}

func (l *workLog) watch(h *conformance.Herdlet) {
	h.Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) {
		l.mu.Lock()
		l.work = append(l.work, w)
		l.mu.Unlock()
		return o, true
	})
}

// of returns the work done on one VM.
func (l *workLog) of(vm string) []link.Work {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []link.Work
	for _, w := range l.work {
		if w.VM == vm {
			out = append(out, w)
		}
	}
	return out
}

func goldenTemplate(t *testing.T, e *conformance.Env, name string, gc *ateapi.GoldenSnapshotConfig) *ateapi.ActorTemplate {
	t.Helper()
	tpl, err := e.Client.CreateActorTemplate(ctxT(t), &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata:   &ateapi.ResourceMetadata{Atespace: e.Space, Name: name},
		Containers: []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
		// The client's status is the server's to write.
		Status:         &ateapi.ActorTemplateStatus{GoldenSnapshotStatus: &ateapi.GoldenSnapshotStatus{ErrorMessage: "client-written"}},
		SnapshotConfig: &ateapi.SnapshotConfig{GoldenSnapshotConfig: gc},
	}})
	if err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	return tpl
}

// waitGolden polls GetActorTemplate (bounded, as a client does) until the golden build is terminal.
func waitGolden(t *testing.T, e *conformance.Env, name string) *ateapi.GoldenSnapshotStatus {
	t.Helper()
	var last *ateapi.ActorTemplate
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		tpl, err := e.Client.GetActorTemplate(ctxT(t), &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, name)})
		if err != nil {
			t.Fatalf("GetActorTemplate: %v", err)
		}
		last = tpl
		if gs := tpl.GetStatus().GetGoldenSnapshotStatus(); gs.GetGoldenTag() != nil || gs.GetErrorMessage() != "" {
			return gs
		}
	}
	t.Fatalf("template %s: no golden_snapshot_status.golden_tag or error_message within 10 s (status: %v)", name, last.GetStatus())
	return nil
}

// TestGoldenSnapshotBuiltAndRestored (criterion golden): a template whose golden mode is ENABLED gets a golden
// snapshot built on a worker keeping the highest fidelity, at that fidelity, and reported as
// status.golden_snapshot_status.golden_tag; the golden actor is gone; CreateActor restores from the tag.
func TestGoldenSnapshotBuiltAndRestored(t *testing.T) {
	e := conformance.Start(t,
		conformance.HerdletSpec{Name: "mac", Slots: 4, Labels: map[string]string{"host": "mac"}, Caps: vmm.Capabilities{Platform: vmm.PlatformVZ, Arch: "arm64", SuspendMaxFidelity: vmm.FidelityVolumes}},
		conformance.HerdletSpec{Name: "ubuntu", Slots: 2, Caps: chCaps(famA)})
	var log, macLog workLog
	log.watch(e.Herdlets["ubuntu"])
	macLog.watch(e.Herdlets["mac"])
	tpl := goldenTemplate(t, e, "tpl", &ateapi.GoldenSnapshotConfig{Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED})
	if m := tpl.GetSnapshotConfig().GetGoldenSnapshotConfig().GetMode(); m != ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED {
		t.Errorf("CreateActorTemplate answered mode %v", m)
	}
	uid := tpl.GetMetadata().GetUid()
	gs := waitGolden(t, e, "tpl")
	if gs.GetErrorMessage() != "" || gs.GetGoldenTag().GetName() != uid || gs.GetGoldenTag().GetAtespace() != e.Space {
		t.Fatalf("golden_snapshot_status = %v, want golden_tag %s/%s and no error", gs, e.Space, uid)
	}
	if gs.GetSnapshotTime() == nil {
		t.Errorf("golden_snapshot_status has no snapshot_time")
	}
	tag, err := e.Client.GetTag(ctxT(t), &ateapi.GetTagRequest{Tag: gs.GetGoldenTag()})
	if err != nil {
		t.Fatalf("GetTag golden: %v", err)
	}
	snap := tag.GetStatus().GetSnapshot()
	if tag.GetScope() != ateapi.TagScope_TAG_SCOPE_PUBLISHED || tag.GetStatus().GetActorTemplateUid() != uid ||
		snap.GetFidelity() != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY || snap.GetLocality() != famA {
		t.Errorf("golden tag = scope %v template %q snapshot %v %q, want PUBLISHED, %q, MEMORY of %q (the worker keeping the highest fidelity)",
			tag.GetScope(), tag.GetStatus().GetActorTemplateUid(), snap.GetFidelity(), snap.GetLocality(), uid, famA)
	}
	_, err = e.Client.GetActor(ctxT(t), &ateapi.GetActorRequest{Actor: oref(e, uid)})
	wantCode(t, "GetActor of the golden actor once tagged", err, codes.NotFound)

	a := mkActor(t, e, "a1", "tpl", nil)
	work := log.of(a.GetMetadata().GetUid())
	if len(work) == 0 || work[0].Op != link.OpRestore || work[0].Snapshot == nil || work[0].Snapshot.Key != snap.GetUuid() {
		t.Fatalf("CreateActor of a template with a golden tag: herdlet work %+v, want a restore of %s", work, snap.GetUuid())
	}
	if n := a.GetStatus().GetAssignedNode(); n != "ubuntu" {
		t.Errorf("a1 restored on %s", n)
	}
	// An actor whose selector keeps it off the MEMORY snapshot's restore family boots cold, as without a golden tag.
	m := mkActor(t, e, "m1", "tpl", map[string]string{"host": "mac"})
	if work := macLog.of(m.GetMetadata().GetUid()); len(work) == 0 || work[0].Op != link.OpBoot {
		t.Errorf("CreateActor m1 on mac: herdlet work %+v, want a cold boot", work)
	}
	for _, w := range macLog.of(uid) {
		t.Errorf("golden actor ran on mac (VOLUMES) with ubuntu (MEMORY) free: %+v", w)
	}
}

// TestGoldenUnspecifiedIsEnabled (criterion golden): Substrate at the pin stores an omitted or UNSPECIFIED golden
// snapshot mode as ENABLED (defaults.Apply), and builds the golden snapshot.
func TestGoldenUnspecifiedIsEnabled(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	for _, c := range []struct {
		name string
		gc   *ateapi.GoldenSnapshotConfig
	}{{"omitted", nil}, {"unspecified", &ateapi.GoldenSnapshotConfig{}}} {
		tpl := goldenTemplate(t, e, c.name, c.gc)
		if m := tpl.GetSnapshotConfig().GetGoldenSnapshotConfig().GetMode(); m != ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED {
			t.Errorf("%s: CreateActorTemplate answered mode %v, want ENABLED", c.name, m)
		}
		if gs := waitGolden(t, e, c.name); gs.GetGoldenTag() == nil {
			t.Errorf("%s: golden_snapshot_status = %v, want a golden tag", c.name, gs)
		}
	}
}

// TestGoldenDisabledBootsCold (criterion golden): DISABLED is reported as the template's mode, no golden actor or tag
// is made, and CreateActor boots cold.
func TestGoldenDisabledBootsCold(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	var log workLog
	log.watch(e.Herdlets["h1"])
	tpl := goldenTemplate(t, e, "tpl", coldBoot)
	got, err := e.Client.GetActorTemplate(ctxT(t), &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, "tpl")})
	if err != nil {
		t.Fatal(err)
	}
	if m := got.GetSnapshotConfig().GetGoldenSnapshotConfig().GetMode(); m != ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED {
		t.Errorf("GetActorTemplate answered mode %v, want DISABLED", m)
	}
	if gs := got.GetStatus().GetGoldenSnapshotStatus(); gs != nil {
		t.Errorf("DISABLED template has golden_snapshot_status %v (the client's own included)", gs)
	}
	if left, err := e.Server.Reconcile(ctxT(t)); err != nil || left != 0 {
		t.Errorf("Reconcile = %d, %v", left, err)
	}
	_, err = e.Client.GetTag(ctxT(t), &ateapi.GetTagRequest{Tag: oref(e, tpl.GetMetadata().GetUid())})
	wantCode(t, "GetTag of a DISABLED template's golden tag", err, codes.NotFound)
	a := mkActor(t, e, "a1", "tpl", nil)
	if work := log.of(a.GetMetadata().GetUid()); len(work) == 0 || work[0].Op != link.OpBoot {
		t.Errorf("CreateActor of a DISABLED template: herdlet work %+v, want a cold boot", work)
	}
	for _, w := range log.work {
		if w.VM == tpl.GetMetadata().GetUid() {
			t.Errorf("a golden actor was started for a DISABLED template: %+v", w)
		}
	}
}

// TestGoldenBuildFailureReported (criterion golden): a build that cannot succeed answers error_message
// "<Reason>: <detail>", never a silent absence, and lets its golden actor go.
func TestGoldenBuildFailureReported(t *testing.T) {
	for _, c := range []struct {
		name, reason string
		op           link.Op
	}{
		{"boot refused", "GoldenActorInvalid: ", link.OpBoot},
		{"snapshot refused", "GoldenSnapshotFailed: ", link.OpSnapshot},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
			e.Herdlets["h1"].Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) {
				if w.Op == c.op {
					return link.Outcome{ID: o.ID, Result: link.ResultFailed, Reason: "refused by the test"}, true
				}
				return o, true
			})
			tpl := goldenTemplate(t, e, "tpl", nil)
			gs := waitGolden(t, e, "tpl")
			if !strings.HasPrefix(gs.GetErrorMessage(), c.reason) || !strings.Contains(gs.GetErrorMessage(), "refused by the test") || gs.GetGoldenTag() != nil {
				t.Errorf("golden_snapshot_status = %v, want error_message %q... and no tag", gs, c.reason)
			}
			_, err := e.Client.GetActor(ctxT(t), &ateapi.GetActorRequest{Actor: oref(e, tpl.GetMetadata().GetUid())})
			wantCode(t, "GetActor of the failed golden actor", err, codes.NotFound)
			// A later pass within the retry backoff (a minute here) does not rebuild it (goldenretry_test.go: after it).
			if left, err := e.Server.Reconcile(ctxT(t)); err != nil || left != 0 {
				t.Errorf("Reconcile = %d, %v", left, err)
			}
		})
	}
}

// TestGoldenRetriedUntilAWorkerConnects (criterion golden): a build with no worker to run on is owed, not failed,
// and Reconcile finishes it once one connects.
func TestGoldenRetriedUntilAWorkerConnects(t *testing.T) {
	e := conformance.Start(t)
	goldenTemplate(t, e, "tpl", nil)
	if left, err := e.Server.Reconcile(ctxT(t)); err != nil || left != 1 {
		t.Errorf("Reconcile with no worker = %d, %v; want the golden build owed", left, err)
	}
	e.AddHerdlet(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	if left, err := e.Server.Reconcile(ctxT(t)); err != nil || left != 0 {
		t.Errorf("Reconcile with a worker = %d, %v", left, err)
	}
	if gs := waitGolden(t, e, "tpl"); gs.GetGoldenTag() == nil {
		t.Errorf("golden_snapshot_status = %v", gs)
	}
}

// TestDeleteTemplateDeletesGoldenTag (criterion golden): DeleteActorTemplate removes the golden tag and its snapshot.
func TestDeleteTemplateDeletesGoldenTag(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	goldenTemplate(t, e, "tpl", nil)
	gs := waitGolden(t, e, "tpl")
	tag, err := e.Client.GetTag(ctxT(t), &ateapi.GetTagRequest{Tag: gs.GetGoldenTag()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Client.DeleteActorTemplate(ctxT(t), &ateapi.DeleteActorTemplateRequest{ActorTemplate: oref(e, "tpl")}); err != nil {
		t.Fatalf("DeleteActorTemplate: %v", err)
	}
	_, err = e.Client.GetTag(ctxT(t), &ateapi.GetTagRequest{Tag: gs.GetGoldenTag()})
	wantCode(t, "GetTag after the template's delete", err, codes.NotFound)
	if _, err := e.Snapshots.Stat(ctxT(t), tag.GetStatus().GetSnapshot().GetUuid()); err == nil {
		t.Errorf("golden snapshot object %s survived the template", tag.GetStatus().GetSnapshot().GetUuid())
	}
}

// TestGoldenSnapshotAfterWarmup (criterion golden): the golden snapshot is taken once the golden actor has run for
// the warmup, as Substrate's reconciler does; snapshot_time says when.
func TestGoldenSnapshotAfterWarmup(t *testing.T) {
	const warmup = 400 * time.Millisecond
	conformance.GoldenWarmup = warmup
	t.Cleanup(func() { conformance.GoldenWarmup = 0 })
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	start := time.Now()
	tpl := goldenTemplate(t, e, "tpl", nil)
	var running *ateapi.GoldenSnapshotStatus
	for deadline := time.Now().Add(5 * time.Second); running == nil && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		got, err := e.Client.GetActorTemplate(ctxT(t), &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, "tpl")})
		if err != nil {
			t.Fatal(err)
		}
		if gs := got.GetStatus().GetGoldenSnapshotStatus(); gs.GetSnapshotTime() != nil {
			running = gs
		}
	}
	if running == nil || running.GetGoldenTag() != nil {
		t.Fatalf("during the warmup: golden_snapshot_status %v, want snapshot_time set and no tag yet", running)
	}
	if a, _ := getActor(t, e, tpl.GetMetadata().GetUid()); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("golden actor during the warmup: %v", a.GetStatus().GetState())
	}
	gs := waitGolden(t, e, "tpl")
	if gs.GetGoldenTag() == nil || time.Since(start) < warmup {
		t.Errorf("golden_snapshot_status %v after %v, want a tag after the %v warmup", gs, time.Since(start), warmup)
	}
	tag, err := e.Client.GetTag(ctxT(t), &ateapi.GetTagRequest{Tag: gs.GetGoldenTag()})
	if err != nil {
		t.Fatal(err)
	}
	if taken := tag.GetMetadata().GetCreateTime().AsTime(); taken.Before(gs.GetSnapshotTime().AsTime()) {
		t.Errorf("golden tag created at %v, before snapshot_time %v", taken, gs.GetSnapshotTime().AsTime())
	}
}
