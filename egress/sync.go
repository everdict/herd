package egress

import (
	"context"
	"errors"
	"sync"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/egress/policy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ErrNoPolicy is a PolicySource's answer for an actor that has no egress policy: it is refused everything.
var ErrNoPolicy = errors.New("egress: actor has no egress policy")

// PolicySource is where herd-egress reads an actor's EgressPolicy: herd-api's Control service, or a fake.
type PolicySource interface {
	// EgressPolicy answers the actor's current policy, ErrNoPolicy when it has none, or another error when it could
	// not find out.
	EgressPolicy(ctx context.Context, actor Actor) (*ateapi.EgressPolicy, error)
}

// ControlSource reads policies from herd-api over substrate.ateapi.v1.Control/GetActorEgressPolicy.
type ControlSource struct {
	Conn grpc.ClientConnInterface
}

// EgressPolicy implements PolicySource.
func (c ControlSource) EgressPolicy(ctx context.Context, a Actor) (*ateapi.EgressPolicy, error) {
	out := &ateapi.EgressPolicy{}
	err := c.Conn.Invoke(ctx, "/substrate.ateapi.v1.Control/GetActorEgressPolicy",
		&ateapi.GetActorEgressPolicyRequest{Actor: &ateapi.ObjectRef{Atespace: a.Space, Name: a.Name}}, out)
	if status.Code(err) == codes.NotFound {
		return nil, ErrNoPolicy
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Syncer keeps every actor bound to a Gateway on its current policy: it reads each one from the source every
// Interval and swaps the compiled policy in, so an update reaches the actor's open flows on their next decision.
//   - No policy (ErrNoPolicy): deny all.
//   - A policy that does not compile: deny all (fail closed), reported through OnError.
//   - Could not read (any other error): keep what the actor has; an unknown answer is not an absent policy.
type Syncer struct {
	Gateway  *Gateway
	Source   PolicySource
	Interval time.Duration
	// OnError, if set, sees every read or compile error.
	OnError func(Actor, error)

	mu   sync.Mutex
	seen map[actorKey]applied
}

// SyncActor reads and applies one actor's policy now.
func (s *Syncer) SyncActor(ctx context.Context, a Actor) error {
	p, err := s.Source.EgressPolicy(ctx, a)
	if errors.Is(err, ErrNoPolicy) {
		p, err = nil, nil
	}
	if err != nil {
		s.report(a, err)
		return err
	}
	s.mu.Lock()
	if s.seen == nil {
		s.seen = map[actorKey]applied{}
	}
	prev, had := s.seen[a.key()]
	s.mu.Unlock()
	if had && proto.Equal(prev.source, p) && s.Gateway.policyOf(a) == prev.compiled {
		return nil
	}
	compiled, cerr := policy.Compile(p)
	if cerr != nil {
		s.report(a, cerr)
		compiled = policy.Deny
	}
	if err := s.Gateway.SetPolicy(a, compiled); err != nil {
		return err
	}
	s.mu.Lock()
	s.seen[a.key()] = applied{source: p, compiled: compiled}
	s.mu.Unlock()
	return cerr
}

func (s *Syncer) report(a Actor, err error) {
	if s.OnError != nil {
		s.OnError(a, err)
	}
}

// Run syncs every bound actor each Interval (default 1 s) until ctx ends.
func (s *Syncer) Run(ctx context.Context) error {
	iv := s.Interval
	if iv <= 0 {
		iv = time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		for _, a := range s.Gateway.Actors() {
			_ = s.SyncActor(ctx, a)
		}
		s.forgetUnbound()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (s *Syncer) forgetUnbound() {
	bound := map[actorKey]bool{}
	for _, a := range s.Gateway.Actors() {
		bound[a.key()] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.seen {
		if !bound[k] {
			delete(s.seen, k)
		}
	}
}

type applied struct {
	source   *ateapi.EgressPolicy
	compiled *policy.Policy
}
