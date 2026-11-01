package apiserver

import (
	"context"
	"errors"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/egress"
	"github.com/everdict/herd/egress/policy"
	"github.com/everdict/herd/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// An actor's egress policy is a singleton child record named after the actor. herd-egress (R4) enforces it; herd-api
// keeps it.

func egressKey(space, actor string) store.Key {
	return store.Key{Kind: store.KindEgressPolicy, Space: space, Name: actor}
}

func egressView(r store.Record) (*ateapi.EgressPolicy, error) {
	p, err := decode(r, &ateapi.EgressPolicy{})
	if err != nil {
		return nil, err
	}
	p.Metadata = meta(r)
	return p, nil
}

// encodePolicy admits a policy as Substrate does (the rule syntax, no two rules tying; package egress/policy, which
// herd-egress enforces) and encodes it.
func encodePolicy(in *ateapi.EgressPolicy) ([]byte, error) {
	if _, err := policy.Compile(in); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	p := proto.Clone(in).(*ateapi.EgressPolicy)
	p.Metadata = nil
	return proto.Marshal(p)
}

func (s *Server) egressTarget(ctx context.Context, actor *ateapi.ObjectRef, a auth.Action) (store.Key, error) {
	space, name, err := ref(actor)
	if err != nil {
		return store.Key{}, err
	}
	if err := s.authorize(ctx, space, a); err != nil {
		return store.Key{}, err
	}
	if _, err := s.cfg.Store.Get(ctx, actorKey(space, name)); err != nil {
		return store.Key{}, toStatus(err)
	}
	return egressKey(space, name), nil
}

// GetActorEgressPolicy returns an actor's egress policy.
func (s *Server) GetActorEgressPolicy(ctx context.Context, req *ateapi.GetActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	k, err := s.egressTarget(ctx, req.GetActor(), auth.ActionRead)
	if err != nil {
		return nil, err
	}
	r, err := s.cfg.Store.Get(ctx, k)
	if err != nil {
		return nil, toStatus(err)
	}
	v, err := egressView(r)
	return v, toStatus(err)
}

// CreateActorEgressPolicy sets an actor's egress policy.
func (s *Server) CreateActorEgressPolicy(ctx context.Context, req *ateapi.CreateActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	k, err := s.egressTarget(ctx, req.GetActor(), auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	data, err := encodePolicy(req.GetEgressPolicy())
	if err != nil {
		return nil, toStatus(err)
	}
	r, err := s.cfg.Store.Create(ctx, store.Record{Key: k, Data: data})
	if err != nil {
		return nil, toStatus(err)
	}
	v, err := egressView(r)
	return v, toStatus(err)
}

// UpdateActorEgressPolicy replaces an actor's egress rules; the request must carry the uid and version it read.
func (s *Server) UpdateActorEgressPolicy(ctx context.Context, req *ateapi.UpdateActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	k, err := s.egressTarget(ctx, req.GetActor(), auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	in := req.GetEgressPolicy()
	cur, err := s.cfg.Store.Get(ctx, k)
	if err != nil {
		return nil, toStatus(err)
	}
	if err := precondition(cur, in.GetMetadata().GetUid(), in.GetMetadata().GetVersion(), true); err != nil {
		return nil, err
	}
	data, err := encodePolicy(in)
	if err != nil {
		return nil, toStatus(err)
	}
	cur.Data = data
	r, err := s.cfg.Store.Update(ctx, cur)
	if err != nil {
		return nil, toStatus(err)
	}
	if err := inForce(ctx); err != nil {
		return nil, err
	}
	v, err := egressView(r)
	return v, toStatus(err)
}

// DeleteActorEgressPolicy removes an actor's egress policy.
func (s *Server) DeleteActorEgressPolicy(ctx context.Context, req *ateapi.DeleteActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	k, err := s.egressTarget(ctx, req.GetActor(), auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	cur, err := s.cfg.Store.Get(ctx, k)
	if err != nil {
		return nil, toStatus(err)
	}
	if err := precondition(cur, req.GetOptions().GetUid(), req.GetOptions().GetVersion(), false); err != nil {
		return nil, err
	}
	v, err := egressView(cur)
	if err != nil {
		return nil, toStatus(err)
	}
	if err := s.cfg.Store.Delete(ctx, k, cur.UID, cur.Version); err != nil {
		return nil, toStatus(err)
	}
	if err := inForce(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *Server) deleteEgressOf(ctx context.Context, space, actor string) error {
	cur, err := s.cfg.Store.Get(ctx, egressKey(space, actor))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.cfg.Store.Delete(ctx, cur.Key, cur.UID, cur.Version)
}

// inForce waits until a policy write that may narrow what an actor is allowed is in force: herd-egress decides only on
// a policy it read within egress.PolicyLease, so once that long has passed since the write, every herd-egress serving
// the actor decides by it (PLT-229). A Create needs no wait: it widens a deny-all, and herd-egress reads again before
// it refuses.
func inForce(ctx context.Context) error {
	t := time.NewTimer(egress.PolicyLease)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}
