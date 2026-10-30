package substrateapi

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// substrateModel answers the Control calls the suspend cases make the way Substrate 5e224278 does (ateapi.proto
// SuspendOptions): a SuspendActor whose mode is unset or ASYNCHRONOUS returns once the snapshot is on the node and
// leaves it PENDING in durable storage (on kind it stayed PENDING for minutes); only SYNCHRONOUS returns it
// COMPLETED. It records the mode of every SuspendActor.
type substrateModel struct {
	mu     sync.Mutex
	actors map[string]*ateapi.Actor
	modes  []ateapi.DurabilityMode
}

func (m *substrateModel) handle(_ any, ss grpc.ServerStream) error {
	method, _ := grpc.MethodFromServerStream(ss)
	m.mu.Lock()
	defer m.mu.Unlock()
	get := func(r *ateapi.ObjectRef) (*ateapi.Actor, error) {
		a, ok := m.actors[r.GetName()]
		if !ok {
			return nil, status.Errorf(codes.NotFound, "Actor %s not found", r.GetName())
		}
		return a, nil
	}
	var resp proto.Message
	switch strings.TrimPrefix(method, controlService) {
	case "GetActorTemplate":
		req := &ateapi.GetActorTemplateRequest{}
		if err := ss.RecvMsg(req); err != nil {
			return err
		}
		resp = &ateapi.ActorTemplate{Metadata: &ateapi.ResourceMetadata{Atespace: "s", Name: req.GetActorTemplate().GetName(), Uid: "tpl-uid"},
			SnapshotConfig: &ateapi.SnapshotConfig{PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY}}
	case "CreateActor":
		req := &ateapi.CreateActorRequest{}
		if err := ss.RecvMsg(req); err != nil {
			return err
		}
		a := proto.CloneOf(req.GetActor())
		a.Metadata.Uid, a.Metadata.Version = "uid-"+a.GetMetadata().GetName(), 1
		a.Status = &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_SUSPENDED}
		m.actors[a.GetMetadata().GetName()] = a
		resp = a
	case "GetActor":
		req := &ateapi.GetActorRequest{}
		if err := ss.RecvMsg(req); err != nil {
			return err
		}
		a, err := get(req.GetActor())
		if err != nil {
			return err
		}
		resp = a
	case "ResumeActor":
		req := &ateapi.ResumeActorRequest{}
		if err := ss.RecvMsg(req); err != nil {
			return err
		}
		a, err := get(req.GetActor())
		if err != nil {
			return err
		}
		a.Status.State, a.Status.WorkerAssignment = ateapi.ActorState_ACTOR_STATE_RUNNING, &ateapi.WorkerAssignment{}
		resp = &ateapi.ResumeActorResponse{Actor: a, Resumed: true}
	case "SuspendActor":
		req := &ateapi.SuspendActorRequest{}
		if err := ss.RecvMsg(req); err != nil {
			return err
		}
		a, err := get(req.GetActor())
		if err != nil {
			return err
		}
		mode := req.GetOptions().GetMode()
		m.modes = append(m.modes, mode)
		storage := ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_PENDING
		if mode == ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS {
			storage = ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED
		}
		gen := int32(len(a.Status.Snapshots) + 1)
		a.Status.Snapshots = append(a.Status.Snapshots, &ateapi.Snapshot{Generation: gen, Owner: ateapi.SnapshotOwner_SNAPSHOT_OWNER_ACTOR,
			Fidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, DurableSnapshot: &ateapi.SnapshotStorage{State: storage}})
		a.Status.State, a.Status.WorkerAssignment = ateapi.ActorState_ACTOR_STATE_SUSPENDED, nil
		resp = &ateapi.SuspendActorResponse{Actor: a}
	case "DeleteActor":
		req := &ateapi.DeleteActorRequest{}
		if err := ss.RecvMsg(req); err != nil {
			return err
		}
		a, err := get(req.GetActor())
		if err != nil {
			return err
		}
		delete(m.actors, req.GetActor().GetName())
		resp = a
	default:
		return status.Errorf(codes.Unimplemented, "the model serves no %s", method)
	}
	return ss.SendMsg(resp)
}

// TestSuspendAsksForTheDurabilityItChecks: against a server with Substrate's durability contract, the suspend case
// asks for DURABILITY_MODE_SYNCHRONOUS (it asserts a COMPLETED snapshot at return) and the suspend-async case asks
// for ASYNCHRONOUS and passes with the snapshot still PENDING. A suite that leaves the mode unset gets Substrate's
// default, ASYNCHRONOUS, and its durability check fails — what the kind run showed.
func TestSuspendAsksForTheDurabilityItChecks(t *testing.T) {
	m := &substrateModel{actors: map[string]*ateapi.Actor{}}
	g := grpc.NewServer(grpc.UnknownServiceHandler(m.handle))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	defer g.Stop()
	c, err := DialControl(Endpoint{Addr: ln.Addr().String()}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := &run{cfg: Config{Atespace: "s", Template: "tpl", Prefix: "cf", Wait: time.Second, Poll: 10 * time.Millisecond}, c: c}
	if r.tpl, err = c.GetActorTemplate(ctx, "s", "tpl"); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		name string
		f    func(context.Context) (string, error)
	}{{"create-from-template", r.create}, {"resume", r.resume}, {"suspend", r.suspend}, {"suspend-async", r.suspendAsync}} {
		if detail, err := step.f(ctx); err != nil {
			t.Errorf("%s: %v", step.name, err)
		} else {
			t.Logf("%s: %s", step.name, detail)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	want := []ateapi.DurabilityMode{ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS, ateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS}
	if len(m.modes) != len(want) || m.modes[0] != want[0] || m.modes[1] != want[1] {
		t.Errorf("SuspendActor modes %v, want %v", m.modes, want)
	}
}
