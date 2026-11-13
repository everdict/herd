package conformance_test

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/store"
	"github.com/everdict/herd/store/boltstore"
	"google.golang.org/grpc/codes"
)

// harnessCreator is the creator herd-api records for the harness's default client: a break-glass HS256 bearer whose sub
// is test-1 (Env.Token).
const harnessCreator = "hs256:test-1"

func wantProvenance(t *testing.T, what string, a *ateapi.Actor, creator string, provenance []byte) {
	t.Helper()
	if got := a.GetStatus().GetCreator(); got != creator {
		t.Errorf("%s: status.creator %q, want %q (the authenticated caller of CreateActor)", what, got, creator)
	}
	if got := a.GetProvenance(); !bytes.Equal(got, provenance) {
		t.Errorf("%s: provenance %q, want %q (the bytes CreateActor carried)", what, got, provenance)
	}
}

// TestActorRecordsItsCreator (PLT-489, A-60): CreateActor records the authenticated caller as status.creator and keeps
// the provenance bytes it carried; a creator the request names is ignored. GetActor and ListActors answer both. An
// UpdateActor that carries other provenance is refused InvalidArgument and changes nothing; one that carries the same
// bytes or none, or another creator, leaves both as they were.
func TestActorRecordsItsCreator(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 3})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	grant := []byte("eyJhbGciOiJFUzI1NiJ9.grant.sig")
	req := actorReq(e, "a1", "tpl", nil)
	req.Actor.Provenance = grant
	req.Actor.Status = &ateapi.ActorStatus{Creator: "spiffe://forged.example/everdict/ws/other/runner/r1"}
	a, err := e.Client.CreateActor(ctx, req)
	if err != nil {
		t.Fatalf("CreateActor: %v", err)
	}
	wantProvenance(t, "CreateActor's answer", a, harnessCreator, grant)
	got, _ := getActor(t, e, "a1")
	wantProvenance(t, "GetActor", got, harnessCreator, grant)
	l, err := e.Client.ListActors(ctx, &ateapi.ListActorsRequest{Atespace: e.Space})
	if err != nil || len(l.GetActors()) != 1 {
		t.Fatalf("ListActors: %v, %d actors", err, len(l.GetActors()))
	}
	wantProvenance(t, "ListActors", l.GetActors()[0], harnessCreator, grant)

	up := &ateapi.Actor{Metadata: got.GetMetadata(), Provenance: []byte("another grant")}
	_, err = e.Client.UpdateActor(ctx, &ateapi.UpdateActorRequest{Actor: up})
	wantCode(t, "UpdateActor with other provenance", err, codes.InvalidArgument)
	got, _ = getActor(t, e, "a1")
	wantProvenance(t, "GetActor after the refused update", got, harnessCreator, grant)

	for _, u := range []*ateapi.Actor{
		{Metadata: got.GetMetadata(), Provenance: grant},
		{Metadata: nil, Status: &ateapi.ActorStatus{Creator: "spiffe://forged.example/x"}},
	} {
		cur, _ := getActor(t, e, "a1")
		u.Metadata = cur.GetMetadata()
		if _, err := e.Client.UpdateActor(ctx, &ateapi.UpdateActorRequest{Actor: u}); err != nil {
			t.Fatalf("UpdateActor (provenance %q, creator %q): %v", u.GetProvenance(), u.GetStatus().GetCreator(), err)
		}
		cur, _ = getActor(t, e, "a1")
		wantProvenance(t, "GetActor after an update", cur, harnessCreator, grant)
	}

	big := actorReq(e, "a2", "tpl", nil)
	big.Actor.Provenance = make([]byte, 4097)
	_, err = e.Client.CreateActor(ctx, big)
	wantCode(t, "CreateActor with 4097 bytes of provenance", err, codes.InvalidArgument)
	ok := actorReq(e, "a3", "tpl", nil)
	ok.Actor.Provenance = bytes.Repeat([]byte{0xff}, 4096)
	if _, err := e.Client.CreateActor(ctx, ok); err != nil {
		t.Fatalf("CreateActor with 4096 bytes of provenance: %v", err)
	}
}

// TestCreatorSurvivesRestart (PLT-489): status.creator and provenance are in the record, so the next herd-api on the
// same bbolt file answers them.
func TestCreatorSurvivesRestart(t *testing.T) {
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
	grant := []byte("grant-across-restart")
	t.Run("first herd-api", func(t *testing.T) {
		e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
		mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
		req := actorReq(e, "a1", "tpl", nil)
		req.Actor.Provenance = grant
		if _, err := e.Client.CreateActor(ctxT(t), req); err != nil {
			t.Fatalf("CreateActor: %v", err)
		}
	})
	t.Run("second herd-api", func(t *testing.T) {
		e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
		a, _ := getActor(t, e, "a1")
		wantProvenance(t, "GetActor from the next herd-api", a, harnessCreator, grant)
	})
}
