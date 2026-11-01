package apiserver

import (
	"context"
	"errors"
	"log"
	"maps"
	"sort"
	"strconv"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/sched"
	"github.com/everdict/herd/store"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// workers is the scheduler's view: connected herdlets with the slots herd-api holds on each.
func (s *Server) workers(ctx context.Context) ([]sched.Worker, error) {
	recs, err := s.cfg.Store.List(ctx, store.KindActor, "")
	if err != nil {
		return nil, err
	}
	used := map[string]int{}
	for _, r := range recs {
		ar, err := parseActor(r)
		if err != nil {
			return nil, err
		}
		if ar.st.Worker != "" {
			used[ar.st.Worker]++
		}
	}
	var out []sched.Worker
	for _, w := range s.cfg.Hub.Workers() {
		out = append(out, sched.Worker{
			Name: w.Worker, Class: w.Class, Labels: w.Labels, Connected: w.Connected,
			Slots: w.Capacity.Slots, Used: used[w.Worker], Warm: w.Capacity.Warm,
			SuspendMaxFidelity: w.Capabilities.SuspendMaxFidelity, RestoreFamily: w.Capabilities.RestoreFamily,
		})
	}
	return out, nil
}

func (s *Server) workerCaps(name string) (link.Capabilities, bool) {
	for _, w := range s.cfg.Hub.Workers() {
		if w.Worker == name {
			return w.Capabilities, true
		}
	}
	return link.Capabilities{}, false
}

func selectorOf(tpl *ateapi.ActorTemplate, a *ateapi.Actor) map[string]string {
	sel := maps.Clone(tpl.GetWorkerSelector().GetMatchLabels())
	if sel == nil {
		sel = map[string]string{}
	}
	maps.Copy(sel, a.GetWorkerSelector().GetMatchLabels())
	return sel
}

func schedSnapshot(r *link.SnapshotRef) *sched.Snapshot {
	if r == nil {
		return nil
	}
	return &sched.Snapshot{Fidelity: r.Fidelity, RestoreFamily: r.RestoreFamily}
}

// place reserves a worker for ar under schedMu by writing the actor record with the worker set (create when the
// record is new, update otherwise). The slot is taken from the moment the record is durable.
func (s *Server) place(ctx context.Context, ar *actorRec, tpl *ateapi.ActorTemplate, create bool) error {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	ws, err := s.workers(ctx)
	if err != nil {
		return err
	}
	w, err := sched.Place(ws, sched.Request{
		Class: SandboxClass, Template: ar.st.TemplateUID, Selector: selectorOf(tpl, ar.actor), Snapshot: schedSnapshot(ar.st.Restore),
		PreferFidelity: ar.st.Golden,
	})
	if err != nil {
		return err
	}
	ar.st.Worker = w
	if !create {
		return s.save(ctx, ar)
	}
	r, err := ar.encode()
	if err != nil {
		return err
	}
	r.UID, r.Version = "", 0
	created, err := s.cfg.Store.Create(ctx, r)
	if err != nil {
		return err
	}
	ar.rec = created
	return nil
}

// start boots or restores ar's VM on its reserved worker and records what the herdlet answered. failedState is the
// state a definite failure returns the actor to (the VM was not started, so the slot is free again).
func (s *Server) start(ctx context.Context, ar *actorRec, op string, failedState ateapi.ActorState) error {
	tpl, _, err := s.loadTemplate(ctx, ar.rec.Space, &ateapi.ObjectRef{Name: ar.st.TemplateName})
	if err != nil {
		return err
	}
	w := link.Work{Op: link.OpBoot, VM: vmID(ar), Template: ar.st.TemplateUID, Spec: templateSpec(ar.st.TemplateUID, tpl),
		Actor: &link.ActorRef{Space: ar.rec.Key.Space, Name: ar.rec.Key.Name, UID: ar.rec.UID}}
	if ar.st.Restore != nil {
		w.Op, w.Snapshot = link.OpRestore, ar.st.Restore
	}
	o := s.step(ctx, ar, w)
	switch o.Result {
	case link.ResultDone:
		worker := ar.st.Worker
		ar.setState(ateapi.ActorState_ACTOR_STATE_RUNNING)
		ar.actor.Status.AssignedNode = worker
		ar.actor.Status.LastAssignedGeneration++
		ar.actor.Status.WorkerAssignment = &ateapi.WorkerAssignment{
			Worker: &ateapi.ObjectRef{Name: worker}, NodeName: worker, WorkerEpoch: int64(ar.actor.Status.LastAssignedGeneration),
			SandboxRuntime: &ateapi.ActorSandboxRuntime{SandboxClass: SandboxClass},
		}
		ar.st.Owed, ar.st.Restore = nil, nil
		return s.save(ctx, ar)
	case link.ResultFailed:
		ar.st.Worker, ar.st.Owed, ar.st.Restore = "", nil, nil
		ar.setState(failedState)
		if failedState == ateapi.ActorState_ACTOR_STATE_UNSPECIFIED {
			// A create whose VM definitely did not start leaves nothing behind.
			if err := s.cfg.Store.Delete(ctx, ar.rec.Key, ar.rec.UID, ar.rec.Version); err != nil {
				return err
			}
		} else if err := s.save(ctx, ar); err != nil {
			return err
		}
		if o.Code == link.CodeUnavailable {
			// Nothing is wrong with the actor or the host: the same request can succeed later.
			return status.Errorf(codes.Unavailable, "herdlet %s: %s failed: %s", w.Op, op, o.Reason)
		}
		return status.Errorf(codes.FailedPrecondition, "herdlet %s: %s failed: %s", w.Op, op, o.Reason)
	}
	ar.owe(op, o.Reason, time.Now())
	if err := s.save(ctx, ar); err != nil {
		return err
	}
	return status.Errorf(codes.Unavailable, "%s owed: herdlet answered unknown: %s", op, o.Reason)
}

func (s *Server) loadTemplate(ctx context.Context, space string, r *ateapi.ObjectRef) (*ateapi.ActorTemplate, store.Record, error) {
	if r.GetName() == "" {
		return nil, store.Record{}, status.Errorf(codes.InvalidArgument, "actor_template is required")
	}
	if r.GetAtespace() != "" && r.GetAtespace() != space {
		return nil, store.Record{}, status.Errorf(codes.InvalidArgument, "actor_template must be in the actor's atespace %q", space)
	}
	rec, err := s.cfg.Store.Get(ctx, store.Key{Kind: store.KindActorTemplate, Space: space, Name: r.GetName()})
	if err != nil {
		return nil, store.Record{}, err
	}
	t, err := decode(rec, &ateapi.ActorTemplate{})
	return t, rec, err
}

// CreateActor creates an actor from its template, or — with source_tag — from the tag's snapshot (the second half
// of a fork), and starts it on a worker the scheduler picked.
func (s *Server) CreateActor(ctx context.Context, req *ateapi.CreateActorRequest) (*ateapi.Actor, error) {
	in := req.GetActor()
	if err := s.authorize(ctx, in.GetMetadata().GetAtespace(), auth.ActionWrite); err != nil {
		return nil, err
	}
	return s.createActor(ctx, in, false)
}

// createActor is CreateActor once the caller is authorized. Without source_tag the actor restores from its template's
// golden tag when the template has one, and boots cold otherwise; golden marks the template's own golden actor,
// which always boots cold, on the worker keeping the highest fidelity.
func (s *Server) createActor(ctx context.Context, in *ateapi.Actor, golden bool) (*ateapi.Actor, error) {
	space, name := in.GetMetadata().GetAtespace(), in.GetMetadata().GetName()
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "metadata.name is required")
	}
	tpl, tplRec, err := s.loadTemplate(ctx, space, in.GetActorTemplate())
	if err != nil {
		return nil, toStatus(err)
	}
	ar := &actorRec{
		rec: store.Record{Key: actorKey(space, name)},
		actor: &ateapi.Actor{
			ActorTemplate: &ateapi.ObjectRef{Atespace: space, Name: tplRec.Name}, WorkerSelector: in.GetWorkerSelector(),
			SourceTag: in.GetSourceTag(), Status: &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_RESUMING},
		},
		st: actorState{TemplateName: tplRec.Name, TemplateUID: tplRec.UID, Golden: golden},
	}
	fromGolden := false
	if gt := tpl.GetStatus().GetGoldenSnapshotStatus().GetGoldenTag(); in.GetSourceTag() == nil && gt != nil && !golden {
		// Substrate's contract: an actor of a template with a golden snapshot starts from it.
		tag, err := s.loadTag(ctx, space, gt.GetName())
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, status.Errorf(codes.FailedPrecondition, "template %s/%s: golden tag %s not found", space, tplRec.Name, gt.GetName())
			}
			return nil, toStatus(err)
		}
		if tag.st.Snapshot == nil || tag.tag.GetStatus().GetActorTemplateUid() != tplRec.UID {
			return nil, status.Errorf(codes.FailedPrecondition, "template %s/%s: golden tag %s holds no snapshot of this template", space, tplRec.Name, gt.GetName())
		}
		ar.st.Restore, fromGolden = tag.st.Snapshot, true
	}
	if st := in.GetSourceTag(); st != nil {
		tspace, tname, err := ref(st)
		if err != nil {
			return nil, err
		}
		if tspace != space {
			return nil, status.Errorf(codes.InvalidArgument, "source_tag must be in the actor's atespace %q", space)
		}
		tag, err := s.loadTag(ctx, tspace, tname)
		if err != nil {
			return nil, toStatus(err)
		}
		if tag.st.Snapshot == nil {
			return nil, status.Errorf(codes.FailedPrecondition, "tag %s/%s holds no snapshot", tspace, tname)
		}
		if tag.tag.GetStatus().GetActorTemplateUid() != tplRec.UID {
			return nil, status.Errorf(codes.FailedPrecondition, "tag %s/%s was taken from template uid %q, not %s (uid %q)",
				tspace, tname, tag.tag.GetStatus().GetActorTemplateUid(), tplRec.Name, tplRec.UID)
		}
		ar.st.Restore = tag.st.Snapshot
	}
	if err := s.ensureSpace(ctx, space); err != nil {
		return nil, toStatus(err)
	}
	// Hold the actor from placement on, so no other workflow acts on it between the reservation and the start.
	unlock := s.lock(ar.rec.Key)
	defer unlock()
	ar.owe(owedBoot, "placed, not started yet", time.Now())
	err = s.place(ctx, ar, tpl, true)
	var nf *sched.NoFitError
	if fromGolden && errors.As(err, &nf) {
		// The golden snapshot is a start-up shortcut, not the actor's state: where no worker can restore it (a MEMORY
		// snapshot of another restore family), the actor boots cold as it would without one.
		log.Printf("herd-api: actor %s/%s: golden snapshot not placeable, booting cold: %v", space, name, err)
		ar.st.Restore = nil
		err = s.place(ctx, ar, tpl, true)
	}
	if err != nil {
		return nil, toStatus(err)
	}
	logBegun(ar, "create")
	err = s.start(ctx, ar, owedBoot, ateapi.ActorState_ACTOR_STATE_UNSPECIFIED)
	logEnded(ar, "create", err)
	if err != nil {
		return nil, toStatus(err)
	}
	return ar.view(), nil
}

// GetActor returns the actor; an owed operation is shown in the Herd-Owed response header.
func (s *Server) GetActor(ctx context.Context, req *ateapi.GetActorRequest) (*ateapi.Actor, error) {
	space, name, err := ref(req.GetActor())
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, space, auth.ActionRead); err != nil {
		return nil, err
	}
	ar, err := s.loadActor(ctx, space, name)
	if err != nil {
		return nil, toStatus(err)
	}
	showOwed(ctx, ar)
	return ar.view(), nil
}

// ListActors lists an atespace's actors by name; the page token is the last name returned.
func (s *Server) ListActors(ctx context.Context, req *ateapi.ListActorsRequest) (*ateapi.ListActorsResponse, error) {
	space := req.GetAtespace()
	if err := s.authorize(ctx, space, auth.ActionRead); err != nil {
		return nil, err
	}
	recs, err := s.cfg.Store.List(ctx, store.KindActor, space)
	if err != nil {
		return nil, toStatus(err)
	}
	size := int(req.GetOptions().GetPageSize())
	if size <= 0 || size > 1000 {
		size = 100
	}
	after := req.GetOptions().GetPageToken()
	i := sort.Search(len(recs), func(i int) bool { return recs[i].Name > after })
	resp := &ateapi.ListActorsResponse{}
	for ; i < len(recs) && len(resp.Actors) < size; i++ {
		ar, err := parseActor(recs[i])
		if err != nil {
			return nil, toStatus(err)
		}
		resp.Actors = append(resp.Actors, ar.view())
	}
	if i < len(recs) && len(resp.Actors) > 0 {
		resp.NextPageToken = resp.Actors[len(resp.Actors)-1].GetMetadata().GetName()
	}
	return resp, nil
}

// UpdateActor changes an actor's worker selector. The request must carry the uid and version it was read at; a stale
// one is refused with Aborted and changes nothing.
func (s *Server) UpdateActor(ctx context.Context, req *ateapi.UpdateActorRequest) (*ateapi.Actor, error) {
	in := req.GetActor()
	space, name := in.GetMetadata().GetAtespace(), in.GetMetadata().GetName()
	if err := s.authorize(ctx, space, auth.ActionWrite); err != nil {
		return nil, err
	}
	unlock := s.lock(actorKey(space, name))
	defer unlock()
	ar, err := s.loadActor(ctx, space, name)
	if err != nil {
		return nil, toStatus(err)
	}
	if err := precondition(ar.rec, in.GetMetadata().GetUid(), in.GetMetadata().GetVersion(), true); err != nil {
		return nil, err
	}
	if t := in.GetActorTemplate(); t != nil && t.GetName() != ar.actor.GetActorTemplate().GetName() {
		return nil, status.Errorf(codes.InvalidArgument, "actor_template cannot change")
	}
	if t := in.GetSourceTag(); t != nil && !proto.Equal(t, ar.actor.GetSourceTag()) {
		return nil, status.Errorf(codes.InvalidArgument, "source_tag cannot change")
	}
	ar.actor.WorkerSelector = in.GetWorkerSelector()
	if err := s.save(ctx, ar); err != nil {
		return nil, toStatus(err)
	}
	return ar.view(), nil
}

// fidelityFor is the fidelity a suspend takes on worker: the template's preference capped by what the worker
// declared. ROOTFS is not a herd fidelity and is kept as VOLUMES.
func fidelityFor(tpl *ateapi.ActorTemplate, caps link.Capabilities) vmm.Fidelity {
	f := vmm.FidelityMemory
	switch tpl.GetSnapshotConfig().GetPreferredFidelity() {
	case ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES, ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS:
		f = vmm.FidelityVolumes
	}
	if caps.SuspendMaxFidelity < f {
		f = caps.SuspendMaxFidelity
	}
	if f < vmm.FidelityVolumes {
		f = vmm.FidelityVolumes
	}
	return f
}

func snapshotProto(r *link.SnapshotRef, owner ateapi.SnapshotOwner, gen int32, templateUID string) *ateapi.Snapshot {
	return &ateapi.Snapshot{
		Uuid: r.Key, Generation: gen, Owner: owner, Fidelity: ateapi.SnapshotFidelity(r.Fidelity),
		ActorTemplateUid: templateUID, Locality: r.RestoreFamily,
		DurableSnapshot: &ateapi.SnapshotStorage{
			State:  ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED,
			Object: &ateapi.ObjectSnapshot{SnapshotUri: "herd-snapshot:///" + r.Key + "?digest=" + r.Digest},
		},
		SandboxRuntime: &ateapi.ActorSandboxRuntime{SandboxClass: SandboxClass},
	}
}

func snapshotKey(kind, space, name, uid string, gen int) string {
	return space + "/" + kind + "/" + name + "/" + uid + "/" + strconv.Itoa(gen)
}

var errNotDurable = errors.New("apiserver: snapshot not durable")
