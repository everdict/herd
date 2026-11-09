package conformance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/store"
)

// spaces reads as an endless run of JSON whitespace.
type spaces struct{}

func (spaces) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

// TestMeasureTagOfALargeSnapshot (DEFAUL-1610, HERD_MEASURE=1): how long CreateTag of a suspended actor takes when its
// snapshot object is HERD_MEASURE_BYTES (default 2 GiB) on herd-api's disk store. The actor's object is replaced by
// its manifest padded to that size and the actor's record given the new digest, as if its herdlet had written a
// snapshot that large; the time is the CreateTag call's, as a client sees it.
func TestMeasureTagOfALargeSnapshot(t *testing.T) {
	if os.Getenv("HERD_MEASURE") != "1" {
		t.Skip("a measurement: HERD_MEASURE=1")
	}
	size := int64(2 << 30)
	if v := os.Getenv("HERD_MEASURE_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		size = n
	}
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	ctx := context.Background()
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES)
	mkActor(t, e, "big", "tpl", nil)
	key := suspend(t, e, "big").GetUuid()

	rc, err := e.Snapshots.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	obj, err := e.Snapshots.Put(ctx, key, io.MultiReader(bytes.NewReader(manifest), io.LimitReader(spaces{}, size-int64(len(manifest)))))
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.Store.Get(ctx, store.Key{Kind: store.KindActor, Space: e.Space, Name: "big"})
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(r.State, &st); err != nil {
		t.Fatal(err)
	}
	st["snapshot"].(map[string]any)["digest"] = obj.Digest
	if r.State, err = json.Marshal(st); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.Update(ctx, r); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	tag, err := e.Client.CreateTag(ctxT(t), &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: e.Space, Name: "t-big"}, SourceActor: oref(e, "big"),
	}})
	took := time.Since(start)
	if err != nil {
		t.Fatalf("CreateTag after %v: %v", took, err)
	}
	t.Logf("measured: CreateTag of a suspended actor's %d-byte snapshot took %.3f s; the tag records %s (the actor's %s)",
		obj.Size, took.Seconds(), tag.GetStatus().GetSnapshot().GetUuid(), key)
	if _, err := e.Client.DeleteTag(ctx, &ateapi.DeleteTagRequest{Tag: oref(e, "t-big")}); err != nil {
		t.Errorf("DeleteTag: %v", err)
	}
	if _, err := e.Client.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: oref(e, "big")}); err != nil {
		t.Errorf("DeleteActor: %v", err)
	}
}
