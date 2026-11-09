package conformance_test

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/apiserver"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/link"
)

// goldenAttempts makes a herdlet store every snapshot it is asked for (the object is written) and then lose it for
// herd-api: the first cut answers nothing and signals cut, as when herd-api stops while the call is in flight; every
// other answer names a digest the object does not have, so herd-api cannot prove what was written (on PLT-382's node,
// an object herd-api looked for in another store).
type goldenAttempts struct {
	mu    sync.Mutex
	asked int
	cuts  int // asks still to cut (< 0: every one)
	cut   chan struct{}
}

func loseGoldenSnapshots(h *conformance.Herdlet, cuts int) *goldenAttempts {
	a := &goldenAttempts{cuts: cuts, cut: make(chan struct{}, 64)}
	h.Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) {
		if w.Op != link.OpSnapshot {
			return o, true
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.asked++
		if o.Result != link.ResultDone || o.Snapshot == nil {
			return o, true
		}
		if a.cuts != 0 {
			a.cuts--
			a.cut <- struct{}{}
			return o, false
		}
		ref := *o.Snapshot
		ref.Digest = strings.Repeat("0", 64)
		o.Snapshot = &ref
		return o, true
	})
	return a
}

func (a *goldenAttempts) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked
}

// snapshotBytes is the size of the files in the snapshot store's directory.
func snapshotBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		n += fi.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// reconcileUntilCut runs Reconcile on the current herd-api, as its loop does, until the herdlet signals a cut snapshot
// (true) or d passes with none (false); then it restarts herd-api, which ends the call the cut left in flight.
func reconcileUntilCut(t *testing.T, e *conformance.Env, a *goldenAttempts, d time.Duration) bool {
	t.Helper()
	srv, stop, done := e.Server, make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			// A process that ended answers errors; the loop's next pass is the new process's.
			_, _ = srv.Reconcile(context.Background())
			time.Sleep(5 * time.Millisecond)
		}
	}()
	var cut bool
	select {
	case <-a.cut:
		cut = true
	case <-time.After(d):
	}
	close(stop)
	if cut {
		e.RestartAPI(t)
	}
	<-done
	return cut
}

// templateGolden reads the template's golden snapshot status.
func templateGolden(t *testing.T, e *conformance.Env, name string) *ateapi.GoldenSnapshotStatus {
	t.Helper()
	got, err := e.Client.GetActorTemplate(ctxT(t), &ateapi.GetActorTemplateRequest{ActorTemplate: oref(e, name)})
	if err != nil {
		t.Fatalf("GetActorTemplate: %v", err)
	}
	return got.GetStatus().GetGoldenSnapshotStatus()
}

// TestFailedGoldenBuildsLeaveNoSnapshotBytes (PLT-382, criterion failed-golden-no-bytes): a golden build that fails
// after the herdlet wrote its snapshot leaves no bytes of that attempt in the snapshot store, however the attempt
// ended: the first is cut by herd-api stopping while the herdlet's answer is in flight, and the others answer a
// snapshot herd-api cannot prove. Once the build has failed for good, the store's directory is the size it was
// before the template.
func TestFailedGoldenBuildsLeaveNoSnapshotBytes(t *testing.T) {
	conformance.GoldenRetryBackoff = 20 * time.Millisecond
	t.Cleanup(func() { conformance.GoldenRetryBackoff = 0 })
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	before := snapshotBytes(t, e.SnapshotDir)
	a := loseGoldenSnapshots(e.Herdlets["h1"], 1)
	goldenTemplate(t, e, "tpl", nil)
	if !reconcileUntilCut(t, e, a, 5*time.Second) {
		t.Fatalf("the golden build asked no snapshot within 5 s")
	}
	reconcileFor(t, e, 3*time.Second)
	if got := snapshotBytes(t, e.SnapshotDir); got != before {
		t.Errorf("after %d golden attempts that failed once their snapshot was written: the snapshot store holds %d bytes, held %d before the template",
			a.count(), got, before)
	}
	if gs := templateGolden(t, e, "tpl"); gs.GetGoldenTag() != nil || !strings.Contains(gs.GetErrorMessage(), "retries failed") {
		t.Errorf("golden_snapshot_status = %v, want the build failed for good (asked %d snapshots)", gs, a.count())
	}
}

// TestGoldenAttemptsBoundedAcrossAPIRestart (PLT-382, criterion retries-survive-restart): a template whose golden
// snapshot never reaches herd-api, because herd-api restarts while each attempt's answer is in flight, is asked for at
// most 1 + apiserver.DefaultGoldenRetries snapshots in all: an attempt a restart cut counts as a failed one, and the
// count is the one herd-api keeps in its records, not in the process that restarted. Afterwards the template answers
// failed for good, and the herdlet is asked for nothing further.
func TestGoldenAttemptsBoundedAcrossAPIRestart(t *testing.T) {
	conformance.GoldenRetryBackoff = 20 * time.Millisecond
	t.Cleanup(func() { conformance.GoldenRetryBackoff = 0 })
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	a := loseGoldenSnapshots(e.Herdlets["h1"], -1)
	goldenTemplate(t, e, "tpl", nil)
	want := 1 + apiserver.DefaultGoldenRetries
	restarts := 0
	for restarts <= 2*want && reconcileUntilCut(t, e, a, 2*time.Second) {
		restarts++
	}
	if got := a.count(); got != want {
		t.Fatalf("across %d herd-api restarts the herdlet was asked for %d golden snapshots, want %d (one build and %d retries)",
			restarts, got, want, apiserver.DefaultGoldenRetries)
	}
	if gs := templateGolden(t, e, "tpl"); gs.GetGoldenTag() != nil || !strings.Contains(gs.GetErrorMessage(), "retries failed") {
		t.Errorf("golden_snapshot_status = %v, want the build failed for good", gs)
	}
	reconcileFor(t, e, 500*time.Millisecond)
	if got := a.count(); got != want {
		t.Errorf("Reconcile after the last retry asked %d more snapshots, want none", got-want)
	}
}
