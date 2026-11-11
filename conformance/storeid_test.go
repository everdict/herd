package conformance_test

import (
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/store/memstore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
)

// storeIDKey is the response metadata that names herd-api's record store (apiserver.StoreIDHeader), spelled out so
// that this test reads the wire, not the constant.
const storeIDKey = "herd-store-id"

// storeIDOf calls method and returns the store id its response named, in its header or (an error answer has no
// header of its own) its trailer, and the call's error.
func storeIDOf(t *testing.T, e *conformance.Env, method string, req, resp any) (string, error) {
	t.Helper()
	var hdr, trl metadata.MD
	err := e.Client.Conn().Invoke(ctxT(t), "/substrate.ateapi.v1.Control/"+method, req, resp, grpc.Header(&hdr), grpc.Trailer(&trl))
	if v := hdr.Get(storeIDKey); len(v) > 0 {
		return v[0], err
	}
	if v := trl.Get(storeIDKey); len(v) > 0 {
		return v[0], err
	}
	return "", err
}

// TestStoreIDNamesTheRecords (PLT-458): every answer of herd-api names the record store it answers from, a uuid made
// with the store and never changed — a NOT_FOUND too, which is where a caller must learn that it asked another store
// than the one that holds its actors. A herd-api restarted on the same records names the same store; one started on
// an empty store names another.
func TestStoreIDNamesTheRecords(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "w1", Slots: 4, Caps: chCaps(famA)})
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
	mkActor(t, e, "a1", "tpl", nil)

	id, err := storeIDOf(t, e, "ListWorkers", &ateapi.ListWorkersRequest{}, &ateapi.ListWorkersResponse{})
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if len(id) != 36 {
		t.Fatalf("ListWorkers named store %q, want a uuid in %s", id, storeIDKey)
	}
	got, err := storeIDOf(t, e, "GetActor", &ateapi.GetActorRequest{Actor: oref(e, "a1")}, &ateapi.Actor{})
	if err != nil || got != id {
		t.Fatalf("GetActor of a1 named store %q (%v), want %q", got, err, id)
	}
	got, err = storeIDOf(t, e, "GetActor", &ateapi.GetActorRequest{Actor: oref(e, "nobody")}, &ateapi.Actor{})
	wantCode(t, "GetActor of an actor that does not exist", err, codes.NotFound)
	if got != id {
		t.Fatalf("a NOT_FOUND named store %q, want %q: a caller cannot tell an actor gone from a store it never held", got, id)
	}

	e.RestartAPI(t)
	if got, err := storeIDOf(t, e, "ListWorkers", &ateapi.ListWorkersRequest{}, &ateapi.ListWorkersResponse{}); err != nil || got != id {
		t.Fatalf("herd-api restarted on the same records named store %q (%v), want %q", got, err, id)
	}

	e.Store = memstore.New()
	e.RestartAPI(t)
	got, err = storeIDOf(t, e, "GetActor", &ateapi.GetActorRequest{Actor: oref(e, "a1")}, &ateapi.Actor{})
	wantCode(t, "GetActor of a1 on an empty store", err, codes.NotFound)
	if got == "" || got == id {
		t.Fatalf("herd-api on an empty store named store %q, want a store id other than %q", got, id)
	}
}
