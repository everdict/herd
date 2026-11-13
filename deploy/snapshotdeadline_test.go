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
)

// TestGoldenSnapshotsOverSlowS3 (DEFAUL-1601): a template's golden bundle uploads to an S3 bucket across a slow link
// for three call timeouts, as a 4 GiB golden took 3 minutes under the fixed 30 s call. The snapshot call's deadline
// follows the bundle's size at -restore-bandwidth, which the herdlet reports as the bundle grows, so the golden is
// built with no longer -call-timeout; the call timeout stays the bound of a call that moves no bytes.
//
// Its counterexample is herd at 0012185: the snapshot call waits the fixed call timeout, answers unknown ("no answer
// to snapshot: context deadline exceeded"), and the golden is never built.
func TestGoldenSnapshotsOverSlowS3(t *testing.T) {
	const callTimeout = time.Second
	creds := s3.Credentials{AccessKeyID: "herd", SecretAccessKey: "herd-secret"}
	srv := s3test.New("us-east-1", creds)
	ts := s3test.Start(t, srv)
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// herd-api expects at least 1 KiB/s; the link takes a little more, so a bundle of a few KiB uploads for seconds.
	const expect = 1024
	srv.SetWriteRate(expect + expect/6)
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
	h := herdletOn(t, "node-1", nil)
	h.API, h.Token, h.Slots = addr, tok, 4
	runHerdlet(t, h)
	waitWorkers(t, addr, ctl, 1)

	c := client(t, addr, ctl)
	defer c.Close()
	cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer ccancel()
	start := time.Now()
	if _, err := c.CreateActorTemplate(cctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata:   &ateapi.ResourceMetadata{Atespace: space, Name: "agent"},
		Containers: []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
			GoldenSnapshotConfig: &ateapi.GoldenSnapshotConfig{Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED}},
	}}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	// The golden pass that CreateActorTemplate starts is the only one: Reconcile runs hourly.
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		tpl, err := c.GetActorTemplate(cctx, &ateapi.GetActorTemplateRequest{ActorTemplate: &ateapi.ObjectRef{Atespace: space, Name: "agent"}})
		if err != nil {
			t.Fatalf("GetActorTemplate: %v", err)
		}
		gs := tpl.GetStatus().GetGoldenSnapshotStatus()
		if gs.GetErrorMessage() != "" {
			t.Fatalf("golden build: %s", gs.GetErrorMessage())
		}
		if gs.GetGoldenTag() != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no golden tag within 20 s (call timeout %v)", callTimeout)
		}
	}
	var size int64
	for _, k := range srv.Keys() {
		size = max(size, srv.Size(k))
	}
	took := time.Since(start)
	t.Logf("golden bundle %d bytes uploaded at %d B/s; built after %v (call timeout %v)", size, expect+expect/6, took.Round(time.Millisecond), callTimeout)
	if up := time.Duration(size * int64(time.Second) / (expect + expect/6)); up < 2*callTimeout {
		t.Fatalf("the bundle's upload took %v: too short to outlast the call timeout %v, so this proves nothing", up, callTimeout)
	}
}
