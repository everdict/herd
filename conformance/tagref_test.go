package conformance_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/store"
	"github.com/everdict/herd/store/memstore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// tagSnapshotSize is how large the padded snapshots of TestTagReferencesTheSnapshot are.
const tagSnapshotSize = 32 << 20

// padSnapshots makes the herdlet's snapshot objects size bytes: the manifest it wrote, then JSON whitespace (a
// restore decodes the manifest and reads no further), so a test sees what herd-api does with a large object.
func padSnapshots(t *testing.T, e *conformance.Env, h *conformance.Herdlet, size int) {
	h.Rewrite(func(w link.Work, o link.Outcome) (link.Outcome, bool) {
		if w.Op != link.OpSnapshot || o.Result != link.ResultDone || o.Snapshot == nil {
			return o, true
		}
		ctx := context.Background()
		rc, err := e.Snapshots.Open(ctx, o.Snapshot.Key)
		if err != nil {
			t.Errorf("padding %s: %v", o.Snapshot.Key, err)
			return o, true
		}
		manifest, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || len(manifest) > size {
			t.Errorf("padding %s: %d bytes, %v", o.Snapshot.Key, len(manifest), err)
			return o, true
		}
		obj, err := e.Snapshots.Put(ctx, o.Snapshot.Key, io.MultiReader(bytes.NewReader(manifest), bytes.NewReader(bytes.Repeat([]byte{' '}, size-len(manifest)))))
		if err != nil {
			t.Errorf("padding %s: %v", o.Snapshot.Key, err)
			return o, true
		}
		r := *o.Snapshot
		r.Digest = obj.Digest
		o.Snapshot = &r
		return o, true
	})
}

// measured is one of herd-api's measured log lines: object_bytes, bytes_moved and seconds.
type measured struct {
	object, moved int64
	seconds       float64
}

// measuredLine finds the last log line that matches what and carries the measurements.
func measuredLine(t *testing.T, l *logBuffer, what string) (measured, bool) {
	t.Helper()
	re := regexp.MustCompile(what + `.*object_bytes=(\d+) bytes_moved=(\d+) seconds=([0-9.]+)`)
	all := re.FindAllStringSubmatch(l.String(), -1)
	if len(all) == 0 {
		return measured{}, false
	}
	m := all[len(all)-1]
	var r measured
	r.object, _ = strconv.ParseInt(m[1], 10, 64)
	r.moved, _ = strconv.ParseInt(m[2], 10, 64)
	r.seconds, _ = strconv.ParseFloat(m[3], 64)
	return r, true
}

func wantObject(t *testing.T, e *conformance.Env, key string, there bool, after string) {
	t.Helper()
	_, err := e.Snapshots.Stat(context.Background(), key)
	switch {
	case there && err != nil:
		t.Errorf("after %s: snapshot object %s: %v; want it kept", after, key, err)
	case !there && !errors.Is(err, snapstore.ErrNotFound):
		t.Errorf("after %s: snapshot object %s: %v; want it deleted", after, key, err)
	}
}

func timedTag(t *testing.T, e *conformance.Env, tag, actor string) (*ateapi.Tag, time.Duration) {
	t.Helper()
	start := time.Now()
	tg, err := e.Client.CreateTag(ctxT(t), &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: tag}, SourceActor: oref(e, actor),
	}})
	if err != nil {
		t.Fatalf("CreateTag %s of %s: %v", tag, actor, err)
	}
	return tg, time.Since(start)
}

// TestTagReferencesTheSnapshot (DEFAUL-1610): a tag of a suspended actor records the actor's own sealed snapshot
// object after one Stat, so for a 32 MiB snapshot it moves no byte through herd-api and answers in well under 2 s; the
// object outlives its source's delete, a fork restores from it, and it goes with the last record that names it,
// whichever of the tag and the actor goes last. herd-api logs the suspend's check and the tag with object_bytes,
// bytes_moved and seconds. An object that is gone or holds other bytes than the actor recorded makes no tag (the copy
// refused those too, by its digest check). Its counterexample is the copy (A-39): on the disk store the tag read the
// object and wrote it again under a key of its own, and nothing logged what it moved.
func TestTagReferencesTheSnapshot(t *testing.T) {
	logs := captureLog(t)
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 4})
	padSnapshots(t, e, e.Herdlets["h1"], tagSnapshotSize)
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES)

	// The tag outlives its source actor; the object goes with the tag.
	mkActor(t, e, "src", "tpl", nil)
	key := suspend(t, e, "src").GetUuid()
	if obj, err := e.Snapshots.Stat(ctx, key); err != nil || obj.Size != tagSnapshotSize {
		t.Fatalf("the suspend's object = %+v, %v; want %d bytes", obj, err, tagSnapshotSize)
	}
	if m, ok := measuredLine(t, logs, "actor "+e.Space+"/src: snapshot check"); !ok || m.object != tagSnapshotSize || m.moved != tagSnapshotSize {
		// The disk store keeps no checksum: its Prove reads the object, and the meter counts that read.
		t.Errorf("the suspend's check logged %+v (found %v); want object_bytes=bytes_moved=%d and seconds", m, ok, tagSnapshotSize)
	}
	tag, took := timedTag(t, e, "t1", "src")
	if got := tag.GetStatus().GetSnapshot().GetUuid(); got != key {
		t.Errorf("the tag records snapshot %q; want the actor's own object %q", got, key)
	}
	if took >= 2*time.Second {
		t.Errorf("the tag of a %d-byte snapshot took %v; want under 2 s", tagSnapshotSize, took)
	}
	m, ok := measuredLine(t, logs, "tag "+e.Space+"/t1 ")
	if !ok || m.object != tagSnapshotSize || m.moved != 0 {
		t.Errorf("the tag logged %+v (found %v); want object_bytes=%d bytes_moved=0", m, ok, tagSnapshotSize)
	}
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "src")}); err != nil {
		t.Fatalf("DeleteActor src: %v", err)
	}
	wantObject(t, e, key, true, "the source actor's delete, with the tag holding its object")
	req := actorReq(e, "fork", "tpl", nil)
	req.Actor.SourceTag = oref(e, "t1")
	if fork, err := e.Client.CreateActor(ctx, req); err != nil || fork.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("fork from the tag whose source is deleted: %v, %v", fork.GetStatus().GetState(), err)
	}
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "fork")}); err != nil {
		t.Fatalf("DeleteActor fork: %v", err)
	}
	wantObject(t, e, key, true, "the fork's delete")
	if _, err := e.Client.DeleteTag(ctx, &ateapi.DeleteTagRequest{Tag: oref(e, "t1")}); err != nil {
		t.Fatalf("DeleteTag t1: %v", err)
	}
	wantObject(t, e, key, false, "the delete of the tag, its last holder")

	// The other order: the tag goes first and the actor keeps the object; it goes with the actor.
	mkActor(t, e, "src2", "tpl", nil)
	key2 := suspend(t, e, "src2").GetUuid()
	if tag, _ := timedTag(t, e, "t2", "src2"); tag.GetStatus().GetSnapshot().GetUuid() != key2 {
		t.Errorf("the tag records snapshot %q; want the actor's own object %q", tag.GetStatus().GetSnapshot().GetUuid(), key2)
	}
	if m, ok := measuredLine(t, logs, "tag "+e.Space+"/t2 "); !ok || m.moved != 0 {
		t.Errorf("the tag logged %+v (found %v); want bytes_moved=0", m, ok)
	}
	if _, err := e.Client.DeleteTag(ctx, &ateapi.DeleteTagRequest{Tag: oref(e, "t2")}); err != nil {
		t.Fatalf("DeleteTag t2: %v", err)
	}
	wantObject(t, e, key2, true, "the tag's delete, with the actor holding its object")
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "src2")}); err != nil {
		t.Fatalf("DeleteActor src2: %v", err)
	}
	wantObject(t, e, key2, false, "the delete of the actor, its last holder")

	// The Stat must find the object with the digest the actor recorded: bytes other than those, or none, make no tag.
	mkActor(t, e, "src3", "tpl", nil)
	key3 := suspend(t, e, "src3").GetUuid()
	if _, err := e.Snapshots.Put(ctx, key3, bytes.NewReader([]byte("other bytes"))); err != nil {
		t.Fatal(err)
	}
	_, err := e.Client.CreateTag(ctx, &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "t3"}, SourceActor: oref(e, "src3"),
	}})
	wantCode(t, "CreateTag of an actor whose object holds other bytes than it recorded", err, codes.Unavailable)
	if err := e.Snapshots.Delete(ctx, key3); err != nil {
		t.Fatal(err)
	}
	_, err = e.Client.CreateTag(ctx, &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "t3"}, SourceActor: oref(e, "src3"),
	}})
	wantCode(t, "CreateTag of an actor whose object is gone", err, codes.Unavailable)
}

// failingLists is a record store whose List can be made to fail.
type failingLists struct {
	store.Store
	fail atomic.Bool
}

func (f *failingLists) List(ctx context.Context, k store.Kind, space string) ([]store.Record, error) {
	if f.fail.Load() {
		return nil, errors.New("test: List refused")
	}
	return f.Store.List(ctx, k, space)
}

// TestFailedReferenceReadDeletesNothing (DEFAUL-1610): herd-api deletes a snapshot object only once it has read every
// record that could name it; a List that fails, or a record it cannot parse, leaves the object where it is and the
// delete undone (the tag's refused, the actor's owed). Its counterexample is a delete that does not look: the tag's
// or the actor's own delete removes an object another record still restores from.
func TestFailedReferenceReadDeletesNothing(t *testing.T) {
	recs := &failingLists{Store: memstore.New()}
	prev := conformance.NewStore
	conformance.NewStore = func(testing.TB) store.Store { return recs }
	defer func() { conformance.NewStore = prev }()
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES)
	mkActor(t, e, "a", "tpl", nil)
	akey := suspend(t, e, "a").GetUuid()
	tag, _ := timedTag(t, e, "t1", "a")
	tkey := tag.GetStatus().GetSnapshot().GetUuid()

	recs.fail.Store(true)
	_, err := e.Client.DeleteTag(ctx, &ateapi.DeleteTagRequest{Tag: oref(e, "t1")})
	wantCode(t, "DeleteTag while List fails", err, codes.Unavailable)
	wantObject(t, e, tkey, true, "a tag delete whose List failed")
	_, err = e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "a")})
	wantCode(t, "DeleteActor while List fails", err, codes.Unavailable)
	wantObject(t, e, akey, true, "an actor delete whose List failed")
	recs.fail.Store(false)

	// With the records read again, the actor's owed delete converges; the tag still names the object, which stays.
	if _, err := e.Server.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := e.Client.GetActor(ctx, &ateapi.GetActorRequest{Actor: oref(e, "a")}); status.Code(err) != codes.NotFound {
		t.Errorf("actor a after Reconcile: %v; want its delete converged", err)
	}
	wantObject(t, e, akey, true, "the actor's delete, with the tag naming its object")

	// The tag is the object's last holder now, unless a record that cannot be parsed names it too.
	bad := store.Key{Kind: store.KindTag, Space: e.Space, Name: "unreadable"}
	if _, err := recs.Create(ctx, store.Record{Key: bad, State: []byte("{not json")}); err != nil {
		t.Fatal(err)
	}
	_, err = e.Client.DeleteTag(ctx, &ateapi.DeleteTagRequest{Tag: oref(e, "t1")})
	wantCode(t, "DeleteTag beside an unparseable tag record", err, codes.Unavailable)
	wantObject(t, e, tkey, true, "a tag delete beside an unparseable record")
	r, err := recs.Get(ctx, bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := recs.Delete(ctx, bad, r.UID, r.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Client.DeleteTag(ctx, &ateapi.DeleteTagRequest{Tag: oref(e, "t1")}); err != nil {
		t.Fatalf("DeleteTag t1: %v", err)
	}
	wantObject(t, e, tkey, false, "the delete of the tag, its last holder, with every record read")
}
