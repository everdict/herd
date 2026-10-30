package deploy_test

import (
	"context"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/deploy"
	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/snapstore/s3/s3test"
)

// s3HerdAPI runs herd-api with its snapshots in a fake S3 bucket (-s3 s3://herd/snapshots) and mints a join token.
func s3HerdAPI(t *testing.T) (srv *s3test.Server, ts *httptest.Server, addr string, key []byte, tok string) {
	creds := s3.Credentials{AccessKeyID: "herd", SecretAccessKey: "herd-secret"}
	srv = s3test.New("us-east-1", creds)
	ts = s3test.Start(t, srv)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "jwt.key")
	key, err := deploy.EnsureKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &deploy.API{KeyFile: keyFile, Store: "bolt:" + filepath.Join(dir, "herd.db"), S3: "s3://herd/snapshots",
		S3Endpoint: ts.URL, S3Credentials: creds, CallTimeout: 5 * time.Second, Reconcile: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("herd-api: %v", err)
		}
	})
	addr = ln.Addr().String()
	tok, err = (&deploy.JoinToken{KeyFile: keyFile, TTL: time.Hour}).Mint()
	if err != nil {
		t.Fatal(err)
	}
	return srv, ts, addr, key, tok
}

// TestJoinedHostsMoveActorsThroughS3: herd-api keeps snapshots in an S3 bucket (-s3) and hands the store to every
// herdlet in its join grant; two joined hosts, each with only its own state directory and no -snapshots, move an
// actor between them: suspended on node-1 (herd-api verifies the object node-1 wrote) and resumed on node-2 from the
// bundle node-2 reads back. Its counterexample is a herdlet that ignores the grant's store and keeps snapshots in a
// directory of its own: herd-api cannot verify the object and the suspend fails, as a join without a shared mount
// did before the S3 store.
func TestJoinedHostsMoveActorsThroughS3(t *testing.T) {
	srv, ts, addr, key, tok := s3HerdAPI(t)
	for _, w := range []string{"node-1", "node-2"} {
		h := herdletOn(t, w, nil)
		h.API, h.Token, h.Labels = addr, tok, "host="+w
		runHerdlet(t, h)
		t.Cleanup(func() {
			// The grant names the bucket; no snapshot directory was made on the host.
			st, err := os.ReadFile(deploy.StatePath(h.StateDir))
			if err != nil || !strings.Contains(string(st), `"bucket": "herd"`) || !strings.Contains(string(st), ts.URL) {
				t.Errorf("%s: join state names no S3 store: %v %s", w, err, st)
			}
			if _, err := os.Stat(filepath.Join(h.StateDir, "snapshots")); !os.IsNotExist(err) {
				t.Errorf("%s: a snapshot directory exists on the host: %v", w, err)
			}
		})
	}
	ctl := writeToken(t, key)
	waitWorkers(t, addr, ctl, 2)

	c := conformance.NewClient(addr, ctl)
	defer c.Close()
	cctx := ctxT(t)
	if _, err := c.CreateActorTemplate(cctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata:       &ateapi.ResourceMetadata{Atespace: space, Name: "agent"},
		Containers:     []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES},
	}}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	ref := &ateapi.ObjectRef{Atespace: space, Name: "a1"}
	act, err := c.CreateActor(cctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata:       &ateapi.ResourceMetadata{Atespace: space, Name: "a1"},
		ActorTemplate:  &ateapi.ObjectRef{Atespace: space, Name: "agent"},
		WorkerSelector: &ateapi.Selector{MatchLabels: map[string]string{"host": "node-1"}},
	}})
	if err != nil {
		t.Fatalf("CreateActor: %v", err)
	}
	if n := act.GetStatus().GetAssignedNode(); n != "node-1" {
		t.Fatalf("created on %q, want node-1", n)
	}
	sr, err := c.SuspendActor(cctx, &ateapi.SuspendActorRequest{Actor: ref, Options: &ateapi.SuspendOptions{Mode: ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS}})
	if err != nil {
		t.Fatalf("SuspendActor on node-1: %v", err)
	}
	if s := sr.GetActor().GetStatus().GetState(); s != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("state %v after suspend", s)
	}
	if keys := srv.Keys(); len(keys) == 0 || !strings.HasPrefix(keys[0], "/herd/snapshots/") {
		t.Errorf("the bucket holds %v, want the snapshot under /herd/snapshots/", keys)
	}
	act = sr.GetActor()
	act.WorkerSelector = &ateapi.Selector{MatchLabels: map[string]string{"host": "node-2"}}
	if _, err := c.UpdateActor(cctx, &ateapi.UpdateActorRequest{Actor: act}); err != nil {
		t.Fatalf("UpdateActor: %v", err)
	}
	rr, err := c.ResumeActor(cctx, &ateapi.ResumeActorRequest{Actor: ref})
	if err != nil {
		t.Fatalf("ResumeActor onto node-2: %v", err)
	}
	if n, s := rr.GetActor().GetStatus().GetAssignedNode(), rr.GetActor().GetStatus().GetState(); n != "node-2" || s != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("resumed: %v on %q, want RUNNING on node-2", s, n)
	}
}

// TestJoinedHerdletIgnoresSnapshotsFlag (criterion no-local-override): a joined herdlet started as the R2c-era units
// started it, with -snapshots naming a directory of its own, still keeps snapshots in the S3 store its join handed it:
// a synchronous suspend succeeds (herd-api verifies the object in the bucket) and nothing is written to the
// directory. Its counterexample is a herdlet that lets -snapshots win over the grant: the snapshot lands in the
// directory, and herd-api answers "snapshot not durable: ... snapstore: not found".
func TestJoinedHerdletIgnoresSnapshotsFlag(t *testing.T) {
	srv, _, addr, key, tok := s3HerdAPI(t)
	h := herdletOn(t, "node-1", nil)
	local := filepath.Join(t.TempDir(), "snapshots")
	h.API, h.Token, h.Snapshots = addr, tok, local
	runHerdlet(t, h)
	ctl := writeToken(t, key)
	waitWorkers(t, addr, ctl, 1)

	c := conformance.NewClient(addr, ctl)
	defer c.Close()
	cctx := ctxT(t)
	if _, err := c.CreateActorTemplate(cctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata:       &ateapi.ResourceMetadata{Atespace: space, Name: "agent"},
		Containers:     []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES},
	}}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	ref := &ateapi.ObjectRef{Atespace: space, Name: "a1"}
	if _, err := c.CreateActor(cctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: space, Name: "a1"},
		ActorTemplate: &ateapi.ObjectRef{Atespace: space, Name: "agent"},
	}}); err != nil {
		t.Fatalf("CreateActor: %v", err)
	}
	sr, err := c.SuspendActor(cctx, &ateapi.SuspendActorRequest{Actor: ref, Options: &ateapi.SuspendOptions{Mode: ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS}})
	if err != nil {
		t.Fatalf("SuspendActor with -snapshots %s: %v", local, err)
	}
	if s := sr.GetActor().GetStatus().GetState(); s != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("state %v after suspend", s)
	}
	if keys := srv.Keys(); len(keys) == 0 || !strings.HasPrefix(keys[0], "/herd/snapshots/") {
		t.Errorf("the bucket holds %v, want the snapshot under /herd/snapshots/", keys)
	}
	if ents, err := os.ReadDir(local); err == nil && len(ents) > 0 {
		t.Errorf("-snapshots %s holds %d entries; a joined herdlet's snapshots go to the join's store", local, len(ents))
	}
}
