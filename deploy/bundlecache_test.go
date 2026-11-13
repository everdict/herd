package deploy_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/deploy"
	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/snapstore/s3/s3test"
	"google.golang.org/grpc/status"
)

// TestGoldenRestoresOverSlowS3 (criterion restore-local, PLT-227): herd-api keeps snapshots in an S3 bucket behind a
// slow link, as the cluster's minio over the tailnet, and bounds herdlet calls by a call timeout shorter than a cold
// read of the template's golden bundle. node-1 builds the golden; then
//   - a session on node-2, which does not hold the bundle, starts: its restore's call waits for the bundle's size at
//     -restore-bandwidth on top of the call timeout, and the bundle is read from the bucket once;
//   - a second session on node-2 reads no bucket bytes (the bundle node-2 fetched, kept under its digest);
//   - a session on node-1 reads no bucket bytes (the golden it built itself).
//
// Its counterexample is herd at 3086723: every restore reads the bundle from the bucket (twice: Verify, then Open)
// under the fixed call timeout, so the first CreateActor answers Unavailable "boot owed: herdlet answered unknown:
// ... context deadline exceeded", the walk's failure.
func TestGoldenRestoresOverSlowS3(t *testing.T) {
	const callTimeout = time.Second
	creds := s3.Credentials{AccessKeyID: "herd", SecretAccessKey: "herd-secret"}
	srv := s3test.New("us-east-1", creds)
	ts := s3test.Start(t, srv)
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// herd-api expects at least 1 KiB/s; the link gives a little more (below).
	const expect = 1024
	a := &deploy.API{Identity: apiIdentity(t), Policy: noRules(t), Store: "bolt:" + filepath.Join(dir, "herd.db"), S3: "s3://herd/snapshots",
		S3Endpoint: ts.URL, S3Credentials: creds, CallTimeout: callTimeout, Reconcile: time.Hour,
		RestoreBandwidthMiB: float64(expect) / (1 << 20)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("herd-api: %v", err)
		}
	})
	addr := ln.Addr().String()
	tok := "" // a node joins by its SVID
	ctl := ""
	join := func(w string) {
		h := herdletOn(t, w, nil)
		h.API, h.Token, h.Labels, h.Slots, h.BundleCacheMiB = addr, tok, "host="+w, 4, deploy.DefaultBundleCacheMiB
		runHerdlet(t, h)
	}
	join("node-1")
	waitWorkers(t, addr, ctl, 1)

	c := client(t, addr, ctl)
	defer c.Close()
	cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer ccancel()
	if _, err := c.CreateActorTemplate(cctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata:   &ateapi.ResourceMetadata{Atespace: space, Name: "agent"},
		Containers: []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
			GoldenSnapshotConfig: &ateapi.GoldenSnapshotConfig{Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED}},
	}}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	var golden string
	for deadline := time.Now().Add(20 * time.Second); golden == ""; time.Sleep(20 * time.Millisecond) {
		tpl, err := c.GetActorTemplate(cctx, &ateapi.GetActorTemplateRequest{ActorTemplate: &ateapi.ObjectRef{Atespace: space, Name: "agent"}})
		if err != nil {
			t.Fatalf("GetActorTemplate: %v", err)
		}
		gs := tpl.GetStatus().GetGoldenSnapshotStatus()
		if gs.GetErrorMessage() != "" {
			t.Fatalf("golden build: %s", gs.GetErrorMessage())
		}
		if gs.GetGoldenTag() != nil {
			golden = gs.GetGoldenTag().GetName()
		}
		if time.Now().After(deadline) {
			t.Fatal("no golden tag within 20 s")
		}
	}
	tag, err := c.GetTag(cctx, &ateapi.GetTagRequest{Tag: &ateapi.ObjectRef{Atespace: space, Name: golden}})
	if err != nil {
		t.Fatalf("GetTag: %v", err)
	}
	if f := tag.GetStatus().GetSnapshot().GetFidelity(); f != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
		t.Fatalf("golden at %v, want MEMORY", f)
	}
	var size int64
	for _, k := range srv.Keys() {
		if n := srv.Size(k); n > size {
			size = n
		}
	}
	// A cold read of the bundle takes three call timeouts: what a cold S3 fetch of a real golden is to 30 s.
	rate := max(size*int64(time.Second)/int64(3*callTimeout), expect+1)
	if size*int64(time.Second)/rate < int64(2*callTimeout) {
		t.Fatalf("the golden bundle is %d bytes: too small for the link to make its read outlast the call timeout at %d B/s, at least %d B/s", size, rate, expect)
	}
	t.Logf("golden bundle %d bytes; the link sends %d B/s (a cold read takes %v; call timeout %v)", size, rate,
		time.Duration(size*int64(time.Second)/rate), callTimeout)
	srv.SetReadRate(rate)
	join("node-2")
	waitWorkers(t, addr, ctl, 2)

	create := func(name, host string) (*ateapi.Actor, time.Duration, int64) {
		t.Helper()
		before, start := srv.Served(), time.Now()
		act, err := c.CreateActor(cctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
			Metadata:       &ateapi.ResourceMetadata{Atespace: space, Name: name},
			ActorTemplate:  &ateapi.ObjectRef{Atespace: space, Name: "agent"},
			WorkerSelector: &ateapi.Selector{MatchLabels: map[string]string{"host": host}},
		}})
		took, read := time.Since(start), srv.Served()-before
		if err != nil {
			t.Fatalf("CreateActor %s on %s from the golden (%v, %d bucket bytes read): %v (%v)", name, host, took, read, err, status.Code(err))
		}
		if n, s := act.GetStatus().GetAssignedNode(), act.GetStatus().GetState(); n != host || s != ateapi.ActorState_ACTOR_STATE_RUNNING {
			t.Fatalf("%s: %v on %q, want RUNNING on %s", name, s, n, host)
		}
		t.Logf("%s on %s: RUNNING after %v, %d bucket bytes read", name, host, took.Round(time.Millisecond), read)
		return act, took, read
	}
	if _, took, read := create("s1", "node-2"); read != size || took <= callTimeout {
		t.Errorf("cold session on node-2: read %d bucket bytes in %v; want the %d-byte bundle read once, past the call timeout", read, took, size)
	}
	if _, _, read := create("s2", "node-2"); read != 0 {
		t.Errorf("second session on node-2 read %d bucket bytes; node-2 holds the golden it fetched", read)
	}
	if _, _, read := create("s3", "node-1"); read != 0 {
		t.Errorf("session on node-1 read %d bucket bytes; node-1 built the golden", read)
	}
}
