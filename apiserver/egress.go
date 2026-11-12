package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
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

// egressState binds a policy record to the actor uid it was written for (review 5): the record is keyed by the actor's
// name, and an actor deleted and created again under that name is another actor.
type egressState struct {
	ActorUID string `json:"actorUid,omitempty"`
}

// egressFor is the policy record of the actor uid holds, or ErrNotFound: a record bound to another uid is an earlier
// actor's, left by a write that raced its delete. A record written before the binding (no uid) is the actor's.
func (s *Server) egressFor(ctx context.Context, k store.Key, uid string) (store.Record, error) {
	r, err := s.cfg.Store.Get(ctx, k)
	if err != nil {
		return r, err
	}
	var st egressState
	if len(r.State) > 0 {
		if err := json.Unmarshal(r.State, &st); err != nil {
			return r, fmt.Errorf("apiserver: %v: state: %w", k, err)
		}
	}
	if st.ActorUID != "" && st.ActorUID != uid {
		return r, fmt.Errorf("%w: %v belongs to an earlier actor (uid %s), not %s", store.ErrNotFound, k, st.ActorUID, uid)
	}
	return r, nil
}

// egressTarget authorizes an egress-policy call on actor and answers its policy key and the actor's uid. A write runs
// under the actor's lock (unlock releases it), so it cannot interleave with the actor's delete, which removes the policy
// under the same lock.
func (s *Server) egressTarget(ctx context.Context, actor *ateapi.ObjectRef, a auth.Action) (store.Key, string, func(), error) {
	space, name, err := ref(actor)
	if err != nil {
		return store.Key{}, "", nil, err
	}
	if a != auth.ActionWrite {
		if err := s.authorize(ctx, space, a); err != nil {
			return store.Key{}, "", nil, err
		}
		ar, err := s.loadActor(ctx, space, name)
		if err != nil {
			return store.Key{}, "", nil, toStatus(err)
		}
		return egressKey(space, name), ar.rec.UID, func() {}, nil
	}
	// An actor's egress policy is a write on the actor: its creator's or an operator's (A-60).
	ctx, err = s.authorizeActorWrite(ctx, space, name)
	if err != nil {
		return store.Key{}, "", nil, err
	}
	unlock := s.lock(actorKey(space, name))
	ar, err := s.loadActorToWrite(ctx, space, name)
	if err != nil {
		unlock()
		return store.Key{}, "", nil, toStatus(err)
	}
	var once sync.Once
	return egressKey(space, name), ar.rec.UID, func() { once.Do(unlock) }, nil
}

// GetActorEgressPolicy returns an actor's egress policy.
func (s *Server) GetActorEgressPolicy(ctx context.Context, req *ateapi.GetActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	// Reading an egress policy is herd-egress's whole grant (auth.RoleEgress); every reader of the space may too.
	k, uid, unlock, err := s.egressTarget(ctx, req.GetActor(), auth.ActionReadEgress)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := s.egressFor(ctx, k, uid)
	if err != nil {
		return nil, toStatus(err)
	}
	v, err := egressView(r)
	return v, toStatus(err)
}

// CreateActorEgressPolicy sets an actor's egress policy, bound to the actor's uid; a record an earlier actor of the
// name left is replaced.
func (s *Server) CreateActorEgressPolicy(ctx context.Context, req *ateapi.CreateActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	k, uid, unlock, err := s.egressTarget(ctx, req.GetActor(), auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	defer unlock()
	data, err := encodePolicy(req.GetEgressPolicy())
	if err != nil {
		return nil, toStatus(err)
	}
	if old, err := s.cfg.Store.Get(ctx, k); err == nil {
		if _, err := s.egressFor(ctx, k, uid); errors.Is(err, store.ErrNotFound) {
			if err := s.cfg.Store.Delete(ctx, k, old.UID, old.Version); err != nil {
				return nil, toStatus(err)
			}
		}
	}
	st, _ := json.Marshal(egressState{ActorUID: uid})
	r, err := s.cfg.Store.Create(ctx, store.Record{Key: k, Data: data, State: st})
	if err != nil {
		return nil, toStatus(err)
	}
	v, err := egressView(r)
	return v, toStatus(err)
}

// UpdateActorEgressPolicy replaces an actor's egress rules; the request must carry the uid and version it read.
func (s *Server) UpdateActorEgressPolicy(ctx context.Context, req *ateapi.UpdateActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	k, uid, unlock, err := s.egressTarget(ctx, req.GetActor(), auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	defer unlock()
	in := req.GetEgressPolicy()
	cur, err := s.egressFor(ctx, k, uid)
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
	cur.State, _ = json.Marshal(egressState{ActorUID: uid})
	r, err := s.cfg.Store.Update(ctx, cur)
	if err != nil {
		return nil, toStatus(err)
	}
	unlock() // the wait for the policy to be in force holds no actor lock
	if err := inForce(ctx); err != nil {
		return nil, err
	}
	v, err := egressView(r)
	return v, toStatus(err)
}

// DeleteActorEgressPolicy removes an actor's egress policy.
func (s *Server) DeleteActorEgressPolicy(ctx context.Context, req *ateapi.DeleteActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	k, uid, unlock, err := s.egressTarget(ctx, req.GetActor(), auth.ActionWrite)
	if err != nil {
		return nil, err
	}
	defer unlock()
	cur, err := s.egressFor(ctx, k, uid)
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
	unlock() // the wait for the policy to be in force holds no actor lock
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
