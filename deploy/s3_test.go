package deploy_test

import (
	"context"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"github.com/everdict/herd/deploy"
	"github.com/everdict/herd/snapstore/s3"
	"github.com/everdict/herd/snapstore/s3/s3test"
	"github.com/everdict/herd/vmm"
	"github.com/everdict/herd/vmm/fake"
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

// memoryRestores is a node's fake hypervisor that records the VM ids of its MEMORY restores.
type memoryRestores struct {
	*fake.Fake
	mu  sync.Mutex
	ids []string
}

func (m *memoryRestores) Restore(ctx context.Context, s vmm.Snapshot, spec vmm.Spec) (vmm.VM, error) {
	vm, err := m.Fake.Restore(ctx, s, spec)
	if err == nil && s.Fidelity == vmm.FidelityMemory {
		m.mu.Lock()
		m.ids = append(m.ids, spec.ID)
		m.mu.Unlock()
	}
	return vm, err
}

func (m *memoryRestores) restored() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.ids)
}

// TestJoinedHostsForkAndMoveAtMemoryThroughS3 (criteria cross-node, loss): two joined Linux hosts of one restore
// family, each with its own hypervisor snapshots (nothing shared but herd-api's S3 bucket), fork and move actors at
// MEMORY. A tag of a MEMORY snapshot taken on node-1 forks twice onto node-2: the VMs have no link, so both forks resume
// the memory there, each under its own VM id (ADR A-28). Then node-1 suspends an actor at MEMORY and leaves the
// pool for good, and the actor resumes at MEMORY on node-2. Its counterexample is the bundle before PLT-212, which
// left the memory in node-1's vm-snapshots: the fork placed on node-2 fails (`snapshot ...: not found`), as fork2 did
// on the supervisor's two-VM Nomad walk, and the moved actor cannot resume once node-1 is gone.
func TestJoinedHostsForkAndMoveAtMemoryThroughS3(t *testing.T) {
	_, _, addr, key, tok := s3HerdAPI(t)
	nodes := map[string]*memoryRestores{}
	stop := map[string]context.CancelFunc{}
	for _, w := range []string{"node-1", "node-2"} {
		h := herdletOn(t, w, nil) // a hypervisor snapshot store of its own, as a host's vm-snapshots directory
		nodes[w] = &memoryRestores{Fake: h.VMM.(*fake.Fake)}
		h.VMM, h.API, h.Token, h.Labels, h.Slots = nodes[w], addr, tok, "host="+w, 4
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = h.Run(ctx) }()
		stop[w] = func() { cancel(); <-done }
		t.Cleanup(stop[w])
	}
	ctl := writeToken(t, key)
	waitWorkers(t, addr, ctl, 2)

	c := conformance.NewClient(addr, ctl)
	defer c.Close()
	cctx := ctxT(t)
	if _, err := c.CreateActorTemplate(cctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: &ateapi.ActorTemplate{
		Metadata:       &ateapi.ResourceMetadata{Atespace: space, Name: "agent"},
		Containers:     []*ateapi.Container{{Name: "main", Image: "ghcr.io/everdict/agent:1"}},
		SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY},
	}}); err != nil {
		t.Fatalf("CreateActorTemplate: %v", err)
	}
	ref := func(name string) *ateapi.ObjectRef { return &ateapi.ObjectRef{Atespace: space, Name: name} }
	create := func(t *testing.T, name, host, tag string) *ateapi.Actor {
		t.Helper()
		a := &ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: name}, ActorTemplate: ref("agent"),
			WorkerSelector: &ateapi.Selector{MatchLabels: map[string]string{"host": host}}}
		if tag != "" {
			a.SourceTag = ref(tag)
		}
		act, err := c.CreateActor(cctx, &ateapi.CreateActorRequest{Actor: a})
		if err != nil {
			t.Fatalf("CreateActor %s on %s: %v", name, host, err)
		}
		if n, s := act.GetStatus().GetAssignedNode(), act.GetStatus().GetState(); n != host || s != ateapi.ActorState_ACTOR_STATE_RUNNING {
			t.Fatalf("%s: %v on %q, want RUNNING on %s", name, s, n, host)
		}
		return act
	}

	t.Run("fork-via-tag", func(t *testing.T) {
		create(t, "src", "node-1", "")
		tag, err := c.CreateTag(cctx, &ateapi.CreateTagRequest{Tag: &ateapi.Tag{Metadata: &ateapi.ResourceMetadata{Atespace: space, Name: "t1"}, SourceActor: ref("src")}})
		if err != nil {
			t.Fatalf("CreateTag: %v", err)
		}
		if f := tag.GetStatus().GetSnapshot().GetFidelity(); f != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
			t.Fatalf("tag of a %v snapshot, want MEMORY", f)
		}
		f1 := create(t, "fork1", "node-2", "t1")
		f2 := create(t, "fork2", "node-2", "t1")
		if got := nodes["node-2"].restored(); !slices.Equal(got, []string{f1.GetMetadata().GetUid(), f2.GetMetadata().GetUid()}) {
			t.Errorf("node-2's MEMORY restores: %v; want both forks', each under its own VM id", got)
		}
	})
	t.Run("move-after-node-loss", func(t *testing.T) {
		before := len(nodes["node-2"].restored())
		mover := create(t, "mover", "node-1", "")
		sr, err := c.SuspendActor(cctx, &ateapi.SuspendActorRequest{Actor: ref("mover"), Options: &ateapi.SuspendOptions{Mode: ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS}})
		if err != nil {
			t.Fatalf("SuspendActor on node-1: %v", err)
		}
		snaps := sr.GetActor().GetStatus().GetSnapshots()
		if s := sr.GetActor().GetStatus().GetState(); s != ateapi.ActorState_ACTOR_STATE_SUSPENDED || len(snaps) == 0 ||
			snaps[len(snaps)-1].GetFidelity() != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
			t.Fatalf("after suspend: %v, snapshots %v; want SUSPENDED at MEMORY", s, snaps)
		}
		stop["node-1"]()
		nodes["node-1"].Fake = fake.New(chCaps(), nil) // and its snapshots with it
		act := sr.GetActor()
		act.WorkerSelector = &ateapi.Selector{MatchLabels: map[string]string{"host": "node-2"}}
		if _, err := c.UpdateActor(cctx, &ateapi.UpdateActorRequest{Actor: act}); err != nil {
			t.Fatalf("UpdateActor: %v", err)
		}
		rr, err := c.ResumeActor(cctx, &ateapi.ResumeActorRequest{Actor: ref("mover")})
		if err != nil {
			t.Fatalf("ResumeActor onto node-2 with node-1 gone: %v", err)
		}
		if n, s := rr.GetActor().GetStatus().GetAssignedNode(), rr.GetActor().GetStatus().GetState(); n != "node-2" || s != ateapi.ActorState_ACTOR_STATE_RUNNING {
			t.Errorf("resumed: %v on %q, want RUNNING on node-2", s, n)
		}
		if got := nodes["node-2"].restored(); len(got) != before+1 || got[before] != mover.GetMetadata().GetUid() {
			t.Errorf("node-2's MEMORY restores: %v; want the mover's %s after the %d before", got, mover.GetMetadata().GetUid(), before)
		}
	})
}
