package conformance_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/apiserver"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/link"
)

// snapshotRefusals makes h refuse its first n snapshots (n < 0: every one) and records when each snapshot was asked.
type snapshotRefusals struct {
	mu    sync.Mutex
	n     int
	asked []time.Time
}

func refuseSnapshots(h *conformance.Herdlet, n int) *snapshotRefusals {
	r := &snapshotRefusals{n: n}
	h.Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) {
		if w.Op != link.OpSnapshot {
			return o, true
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.asked = append(r.asked, time.Now())
		if r.n != 0 {
			r.n--
			return link.Outcome{ID: o.ID, Result: link.ResultFailed, Reason: "refused by the test"}, true
		}
		return o, true
	})
	return r
}

func (r *snapshotRefusals) times() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.asked...)
}

// reconcileFor runs Reconcile, as herd-api's loop does, every few milliseconds for d.
func reconcileFor(t *testing.T, e *conformance.Env, d time.Duration) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if _, err := e.Server.Reconcile(ctxT(t)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
}

// TestFailedGoldenRebuiltWithNoSpecChange (DEFAUL-1601): a golden build that failed on something transient (here a
// snapshot the herdlet refused once; on the cluster a golden actor lost when herd-api restarted) answers failed, and
// Reconcile builds it again after a backoff with no change to the template: the template gets its golden tag, the
// error is cleared, and an actor of it starts from the golden. Before, the failure was terminal and only a new spec
// made a new template.
func TestFailedGoldenRebuiltWithNoSpecChange(t *testing.T) {
	conformance.GoldenRetryBackoff = 50 * time.Millisecond
	t.Cleanup(func() { conformance.GoldenRetryBackoff = 0 })
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	refused := refuseSnapshots(e.Herdlets["h1"], 1)
	tpl := goldenTemplate(t, e, "tpl", nil)
	gs := waitGolden(t, e, "tpl")
	if !strings.HasPrefix(gs.GetErrorMessage(), "GoldenSnapshotFailed: ") || gs.GetGoldenTag() != nil {
		t.Fatalf("after a refused snapshot: golden_snapshot_status = %v, want GoldenSnapshotFailed", gs)
	}
	reconcileFor(t, e, time.Second)
	got, err := e.Client.GetActorTemplate(ctxT(t), &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, "tpl")})
	if err != nil {
		t.Fatal(err)
	}
	gs = got.GetStatus().GetGoldenSnapshotStatus()
	if gs.GetGoldenTag() == nil || gs.GetErrorMessage() != "" {
		t.Fatalf("after the backoff: golden_snapshot_status = %v, want the golden built and no error (snapshots asked: %d)", gs, len(refused.times()))
	}
	if got.GetMetadata().GetUid() != tpl.GetMetadata().GetUid() {
		t.Errorf("the template's uid changed: %s, was %s", got.GetMetadata().GetUid(), tpl.GetMetadata().GetUid())
	}
	a := mkActor(t, e, "a1", "tpl", nil)
	if a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("an actor of the rebuilt template: %v", a.GetStatus().GetState())
	}
}

// TestPermanentGoldenFailureEnds (DEFAUL-1601): a golden build that fails every time is built again
// apiserver.DefaultGoldenRetries times, each retry waiting twice as long as the one before, and then no more: the
// template answers failed for good, saying its retries failed, and Reconcile asks the herdlet for nothing further.
func TestPermanentGoldenFailureEnds(t *testing.T) {
	const backoff = 20 * time.Millisecond
	conformance.GoldenRetryBackoff = backoff
	t.Cleanup(func() { conformance.GoldenRetryBackoff = 0 })
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	refused := refuseSnapshots(e.Herdlets["h1"], -1)
	goldenTemplate(t, e, "tpl", nil)
	waitGolden(t, e, "tpl")
	reconcileFor(t, e, 3*time.Second)
	asked := refused.times()
	if want := 1 + apiserver.DefaultGoldenRetries; len(asked) != want {
		t.Fatalf("the herdlet was asked for %d snapshots, want %d (one build and %d retries)", len(asked), want, apiserver.DefaultGoldenRetries)
	}
	for i := 1; i < len(asked); i++ {
		if gap, want := asked[i].Sub(asked[i-1]), backoff<<(i-1); gap < want {
			t.Errorf("retry %d came %v after the build before it, want at least %v", i, gap, want)
		}
	}
	gs := waitGolden(t, e, "tpl")
	if !strings.HasPrefix(gs.GetErrorMessage(), "GoldenSnapshotFailed: ") || !strings.Contains(gs.GetErrorMessage(), "retries failed") {
		t.Errorf("after the last retry: error_message %q, want GoldenSnapshotFailed saying the retries failed", gs.GetErrorMessage())
	}
	if left, err := e.Server.Reconcile(ctxT(t)); err != nil || left != 0 || len(refused.times()) != len(asked) {
		t.Errorf("Reconcile after the last retry = %d, %v, and %d snapshots asked; want nothing owed and none asked", left, err, len(refused.times())-len(asked))
	}
}
