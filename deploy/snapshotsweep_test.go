package deploy_test

import (
	"context"
	"net"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/everdict/herd/deploy"
	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/snapstore/s3/s3test"
)

// TestHerdAPISweepsItsBucket (PLT-462, A-58): herd-api with -s3 runs the snapshot sweep on its own, after its start
// delay: an object no record names, written two hours ago, is removed from the bucket, a multipart upload begun two
// days ago and never finished is aborted, a fresh unnamed object stays, and the bucket is claimed for herd-api's
// record store. On 8241c2c nothing removed either: the live bucket held 33 GB, an 821 MB object named by nothing among
// it.
func TestHerdAPISweepsItsBucket(t *testing.T) {
	creds := s3.Credentials{AccessKeyID: "herd", SecretAccessKey: "herd-secret"}
	srv := s3test.New("us-east-1", creds)
	ts := s3test.Start(t, srv)
	now := time.Now()
	srv.SetClock(func() time.Time { return now.Add(-48 * time.Hour) })
	srv.Begin("/herd/snapshots/space-1/actors/a/u/7")
	srv.SetClock(func() time.Time { return now.Add(-2 * time.Hour) })
	srv.Plant("/herd/snapshots/space-1/actors/ghost/u/1", []byte("821 MB named by nothing"), "sha256:00", "", 0)
	srv.SetClock(nil)
	srv.Plant("/herd/snapshots/space-1/actors/ghost/u/2", []byte("written a moment ago"), "sha256:00", "", 0)

	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &deploy.API{Identity: apiIdentity(t), Policy: noRules(t), Store: "bolt:" + filepath.Join(dir, "herd.db"), S3: "s3://herd/snapshots",
		S3Endpoint: ts.URL, S3Credentials: creds, CallTimeout: 5 * time.Second, Reconcile: time.Second,
		SnapshotSweep: time.Hour, SnapshotSweepDelay: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("herd-api: %v", err)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for slices.Contains(srv.Keys(), "/herd/snapshots/space-1/actors/ghost/u/1") || srv.Uploads() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("10 s after herd-api started: bucket holds %v and %d unfinished uploads; want the unnamed old object removed and the stale upload aborted", srv.Keys(), srv.Uploads())
		}
		time.Sleep(20 * time.Millisecond)
	}
	keys := srv.Keys()
	if !slices.Contains(keys, "/herd/snapshots/space-1/actors/ghost/u/2") || !slices.Contains(keys, "/herd/snapshots/herd-record-store") {
		t.Errorf("bucket after the sweep holds %v; want the young object and the claim kept", keys)
	}
}
