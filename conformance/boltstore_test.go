package conformance_test

import (
	"path/filepath"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/store"
	"github.com/everdict/herd/store/boltstore"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// TestOnBoltstore runs the record, workflow and scheduling tests again with herd-api on the all-in-one store.
func TestOnBoltstore(t *testing.T) {
	prev := conformance.NewStore
	conformance.NewStore = func(t testing.TB) store.Store {
		s, err := boltstore.Open(filepath.Join(t.TempDir(), "herd.db"), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	defer func() { conformance.NewStore = prev }()
	for name, f := range map[string]func(*testing.T){
		"StaleVersionRefused":           TestStaleVersionRefused,
		"RecordsRoundTrip":              TestRecordsRoundTrip,
		"SuspendDurableBeforeRelease":   TestSuspendDurableBeforeRelease,
		"SuspendUnknownStaysSuspending": TestSuspendUnknownStaysSuspending,
		"DeleteNotVerifiedStaysOwed":    TestDeleteNotVerifiedStaysOwed,
		"Fork":                          TestFork,
		"SchedulerSlots":                TestSchedulerSlots,
		"SchedulerRestoreFamily":        TestSchedulerRestoreFamily,
		"HerdletTimeoutIsNotSuccess":    TestHerdletTimeoutIsNotSuccess,
	} {
		t.Run(name, f)
	}
}

// TestRecordsSurviveRestart: what herd-api acknowledged on the all-in-one store is there for the next herd-api on the
// same file, preconditions included.
func TestRecordsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herd.db")
	prev := conformance.NewStore
	conformance.NewStore = func(t testing.TB) store.Store {
		s, err := boltstore.Open(path, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	defer func() { conformance.NewStore = prev }()
	var before *ateapi.Actor
	t.Run("first herd-api", func(t *testing.T) {
		e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
		mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
		mkActor(t, e, "a1", "tpl", nil)
		r, err := e.Client.SuspendActor(ctxT(t), &ateapi.SuspendActorRequest{Actor: oref(e, "a1")})
		if err != nil {
			t.Fatal(err)
		}
		before = r.GetActor()
	})
	if before == nil {
		t.Fatal("first herd-api did not suspend the actor")
	}
	t.Run("second herd-api", func(t *testing.T) {
		e := conformance.Start(t)
		got, owed := getActor(t, e, "a1")
		if got.GetMetadata().GetUid() != before.GetMetadata().GetUid() || got.GetMetadata().GetVersion() != before.GetMetadata().GetVersion() ||
			got.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED || len(got.GetStatus().GetSnapshots()) != 1 || owed != "" {
			t.Errorf("after restart: %v owed %q, want %v", got, owed, before)
		}
		stale := proto.Clone(before.GetMetadata()).(*ateapi.ResourceMetadata)
		stale.Version--
		_, err := e.Client.UpdateActor(ctxT(t), &ateapi.UpdateActorRequest{Actor: &ateapi.Actor{Metadata: stale}})
		wantCode(t, "stale UpdateActor after restart", err, codes.Aborted)
		if _, err := e.Client.UpdateActor(ctxT(t), &ateapi.UpdateActorRequest{Actor: &ateapi.Actor{Metadata: before.GetMetadata()}}); err != nil {
			t.Errorf("UpdateActor at the version read before the restart: %v", err)
		}
	})
}
