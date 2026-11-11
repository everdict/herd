package conformance_test

import (
	"context"
	"io"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/herdlet"
	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/snapstore/bundlecache"
	"google.golang.org/grpc/codes"
)

// pacedStore is the snapshot store as a herdlet on a slow link reaches it: each read of an object takes pace and
// brings at most chunk bytes.
type pacedStore struct {
	snapstore.Store
	pace  time.Duration
	chunk int
}

type pacedReader struct {
	io.ReadCloser
	s pacedStore
}

func (r pacedReader) Read(p []byte) (int, error) {
	time.Sleep(r.s.pace)
	if len(p) > r.s.chunk {
		p = p[:r.s.chunk]
	}
	return r.ReadCloser.Read(p)
}

func (s pacedStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := s.Store.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	return pacedReader{rc, s}, nil
}

// TestRestoreWhoseFetchOutlastsEachCallConverges (PLT-457): a resume whose snapshot object takes longer to fetch than
// any one call to the herdlet waits ends unknown, and each later attempt (Reconcile's, here each bounded shorter than
// the fetch) picks up the fetch where it is instead of starting it again, so the actor ends RUNNING.
func TestRestoreWhoseFetchOutlastsEachCallConverges(t *testing.T) {
	e := conformance.Start(t)
	e.AddHerdlet(t, conformance.HerdletSpec{Name: "w1", Slots: 2, Caps: chCaps(famA), Configure: func(a *herdlet.Agent) {
		c, err := bundlecache.New(t.TempDir(), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		// The fake's snapshot object is a few hundred bytes: several seconds at 4 bytes per 50 ms.
		a.Bundles, a.Snapshots = c, pacedStore{Store: e.Snapshots, pace: 50 * time.Millisecond, chunk: 4}
	}})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)
	if _, err := e.Client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: oref(e, "a1")}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	_, err := e.Client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: oref(e, "a1")})
	wantCode(t, "ResumeActor whose bundle takes longer to fetch than its call waits", err, codes.Unavailable)

	for i := 0; i < 10; i++ {
		rctx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
		_, _ = e.Server.Reconcile(rctx)
		cancel()
		if a, _ := getActor(t, e, "a1"); a.GetStatus().GetState() == ateapi.ActorState_ACTOR_STATE_RUNNING {
			return
		}
	}
	a, owed := getActor(t, e, "a1")
	t.Fatalf("actor a1 is %v (owed %q) after ten Reconciles of 400 ms each, want RUNNING: each restore fetched its bundle from zero",
		a.GetStatus().GetState(), owed)
}
