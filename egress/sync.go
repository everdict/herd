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

// PolicyLease is how long herd-egress decides on a policy it read: a decision whose actor's last read started longer
// ago reads the policy again first. herd-api's Update and DeleteActorEgressPolicy wait as long after the write before
// they answer, so every read a decision can still use started after the write (PLT-229: a write is in force when it
// is acknowledged). A Create needs no wait: it can only widen a deny-all, and a refusal reads again (Syncer).
const PolicyLease = time.Second

// Syncer keeps every actor bound to a Gateway on its current policy: it reads each one from the source every
// Interval and swaps the compiled policy in, so an update reaches the actor's open flows on their next decision.
//   - No policy (ErrNoPolicy): deny all.
//   - A policy that does not compile: deny all (fail closed), reported through OnError.
//   - Could not read (any other error): keep what the actor has; an unknown answer is not an absent policy.
//
// While Run runs, the Gateway also asks it before deciding (PLT-229): a decision on a policy read more than
// PolicyLease ago reads it again first, and a refusal is decided again on a read that started after the flow
// arrived, so a policy herd-api acknowledged is the one that decides. Either read waits at most Wait.
type Syncer struct {
	Gateway  *Gateway
	Source   PolicySource
	Interval time.Duration
	// Wait bounds how long a decision waits for a read of its actor's policy (default 2 s); past it the decision is
	// made on the policy the actor has.
	Wait time.Duration
	// OnError, if set, sees every read or compile error.
	OnError func(Actor, error)

	mu   sync.Mutex
	seen map[actorKey]*actorSync
}

// actorSync is what the Syncer knows of one actor: the policy it applied, and its last read (when it started, what
// it answered) or the read in flight.
type actorSync struct {
	applied
	read    time.Time
	readErr error
	flight  chan struct{}
}

// SyncActor reads and applies one actor's policy now.
func (s *Syncer) SyncActor(ctx context.Context, a Actor) error {
	return s.syncSince(ctx, a, time.Now())
}

// syncSince returns once a read of a's policy that started at or after since has been applied (or failed), joining a
// read in flight that qualifies and starting one otherwise.
func (s *Syncer) syncSince(ctx context.Context, a Actor, since time.Time) error {
	for {
		s.mu.Lock()
		if s.seen == nil {
			s.seen = map[actorKey]*actorSync{}
		}
		st := s.seen[a.key()]
		if st == nil {
			st = &actorSync{}
			s.seen[a.key()] = st
		}
		if !st.read.IsZero() && !st.read.Before(since) && s.Gateway.policyOf(a) == st.compiled {
			err := st.readErr
			s.mu.Unlock()
			return err
		}
		if f := st.flight; f != nil {
			s.mu.Unlock()
			select {
			case <-f:
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		f := make(chan struct{})
		st.flight = f
		start := time.Now()
		s.mu.Unlock()
		err := s.readApply(ctx, a, st)
		s.mu.Lock()
		st.read, st.readErr, st.flight = start, err, nil
		s.mu.Unlock()
		close(f)
		return err
	}
}

func (s *Syncer) readApply(ctx context.Context, a Actor, st *actorSync) error {
	p, err := s.Source.EgressPolicy(ctx, a)
	if errors.Is(err, ErrNoPolicy) {
		p, err = nil, nil
	}
	if err != nil {
		s.report(a, err)
		return err
	}
	s.mu.Lock()
	prev := st.applied
	s.mu.Unlock()
	if prev.compiled != nil && proto.Equal(prev.source, p) && s.Gateway.policyOf(a) == prev.compiled {
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
	st.applied = applied{source: p, compiled: compiled}
	s.mu.Unlock()
	return cerr
}

// fresh is the Gateway's question before a decision: has a's policy been read since then? It waits at most Wait.
func (s *Syncer) fresh(ctx context.Context, a Actor, since time.Time) error {
	w := s.Wait
	if w <= 0 {
		w = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, w)
	defer cancel()
	return s.syncSince(ctx, a, since)
}

func (s *Syncer) report(a Actor, err error) {
	if s.OnError != nil {
		s.OnError(a, err)
	}
}

// Run syncs every bound actor each Interval (default 1 s) until ctx ends, and answers the Gateway's freshness
// questions meanwhile.
func (s *Syncer) Run(ctx context.Context) error {
	iv := s.Interval
	if iv <= 0 {
		iv = time.Second
	}
	s.Gateway.syncer.Store(s)
	defer s.Gateway.syncer.CompareAndSwap(s, nil)
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
