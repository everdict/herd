package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Owed operations: a workflow step that has not converged and is retried by Reconcile.
const (
	owedBoot    = "boot"
	owedResume  = "resume"
	owedSuspend = "suspend"
	owedRelease = "release"
	owedDelete  = "delete"
)

// owed is a workflow step not yet converged, and why.
type owed struct {
	Op       string    `json:"op"`
	Reason   string    `json:"reason,omitempty"`
	Attempts int       `json:"attempts"`
	Since    time.Time `json:"since"`
}

// actorState is herd-api's bookkeeping beside an Actor record.
type actorState struct {
	// Worker holds (or may hold) the actor's VM; its slot stays taken until the VM is verified gone.
	Worker       string `json:"worker,omitempty"`
	TemplateName string `json:"templateName"`
	TemplateUID  string `json:"templateUid"`
	Owed         *owed  `json:"owed,omitempty"`
	// Snapshot is the latest durable snapshot.
	Snapshot *link.SnapshotRef `json:"snapshot,omitempty"`
	// Restore is what a pending start restores from (resume, fork); nil for a cold boot.
	Restore *link.SnapshotRef `json:"restore,omitempty"`
	// DeleteTime is when the delete was asked for.
	DeleteTime *time.Time `json:"deleteTime,omitempty"`
	// Keys are the snapshot objects this actor owns; a delete removes them.
	Keys []string `json:"keys,omitempty"`
	// Golden marks a template's golden actor: herd-api made it to build the template's golden snapshot.
	Golden bool `json:"golden,omitempty"`
	// Instance is the herdlet process (link.Register.Instance) that answered the last start of the actor's VM done:
	// the process that holds it. Empty: not known (a herdlet older than PLT-371), and then no restart is concluded.
	Instance string `json:"instance,omitempty"`
}

type actorRec struct {
	rec   store.Record
	actor *ateapi.Actor
	st    actorState
	// pull is a create's one-shot pull grant for its cold boot (PLT-405): in this create's memory only, never
	// encoded, so an actor loaded from the store has none.
	pull goldenPull
}

func actorKey(space, name string) store.Key {
	return store.Key{Kind: store.KindActor, Space: space, Name: name}
}

func parseActor(r store.Record) (*actorRec, error) {
	a, err := decode(r, &ateapi.Actor{})
	if err != nil {
		return nil, err
	}
	ar := &actorRec{rec: r, actor: a}
	if len(r.State) > 0 {
		if err := json.Unmarshal(r.State, &ar.st); err != nil {
			return nil, fmt.Errorf("apiserver: %v: state: %w", r.Key, err)
		}
	}
	if ar.actor.Status == nil {
		ar.actor.Status = &ateapi.ActorStatus{}
	}
	return ar, nil
}

func (s *Server) loadActor(ctx context.Context, space, name string) (*actorRec, error) {
	r, err := s.cfg.Store.Get(ctx, actorKey(space, name))
	if err != nil {
		return nil, err
	}
	return parseActor(r)
}

func (ar *actorRec) encode() (store.Record, error) {
	r := ar.rec
	a := proto.Clone(ar.actor).(*ateapi.Actor)
	a.Metadata = nil
	data, err := proto.Marshal(a)
	if err != nil {
		return store.Record{}, fmt.Errorf("apiserver: %v: encode: %w", r.Key, err)
	}
	st, err := json.Marshal(ar.st)
	if err != nil {
		return store.Record{}, fmt.Errorf("apiserver: %v: encode state: %w", r.Key, err)
	}
	r.Data, r.State = data, st
	return r, nil
}

// save writes ar back at the version it was read at; a concurrent writer makes it fail with ErrConflict.
func (s *Server) save(ctx context.Context, ar *actorRec) error {
	r, err := ar.encode()
	if err != nil {
		return err
	}
	next, err := s.cfg.Store.Update(ctx, r)
	if err != nil {
		return err
	}
	ar.rec = next
	return nil
}

func (ar *actorRec) setState(st ateapi.ActorState) { ar.actor.Status.State = st }

func (ar *actorRec) owe(op, reason string, now time.Time) {
	if ar.st.Owed != nil && ar.st.Owed.Op == op {
		ar.st.Owed.Reason = reason
		ar.st.Owed.Attempts++
		return
	}
	ar.st.Owed = &owed{Op: op, Reason: reason, Attempts: 1, Since: now.UTC()}
}

// view is the Actor as the API shows it.
func (ar *actorRec) view() *ateapi.Actor {
	a := proto.Clone(ar.actor).(*ateapi.Actor)
	md := meta(ar.rec)
	if t := ar.st.DeleteTime; t != nil {
		md.DeleteTime = timestamppb.New(*t)
	}
	a.Metadata = md
	return a
}

// showOwed puts the actor's owed operation on the response, so a caller sees why it has not converged.
func showOwed(ctx context.Context, ar *actorRec) {
	if o := ar.st.Owed; o != nil {
		_ = grpc.SetTrailer(ctx, metadata.Pairs(OwedHeader, o.Op+": "+o.Reason))
	}
}

// vmID is the VM's id on its herdlet: the actor's uid.
func vmID(ar *actorRec) string { return ar.rec.UID }
