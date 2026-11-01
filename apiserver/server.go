// Package apiserver is herd-api: it serves the pinned Substrate Control subset (ADR 0001, A-4) and herd's
// Capabilities extension (A-5), keeps the records, places actors, runs the suspend/resume/delete workflows, and
// accepts herdlets' outbound links.
//
// Everdict's laws hold here (R-D6):
//   - authority before effect: a call is authenticated and authorized for its atespace before anything changes, and
//     every record write names the uid+version it was read at;
//   - unknown is a third value: a herdlet that answers unknown, breaks its link or does not answer in time leaves
//     the workflow owed (SUSPENDING, RESUMING, DELETING) with the reason, never done;
//   - durable before release: a suspend records the snapshot's key+digest, verified against the snapshot store,
//     before it releases the worker;
//   - verified delete: an actor record goes only once its herdlet answered the VM verified gone.
package apiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/sched"
	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SandboxClass is the one class herd runs: a Linux microVM (R-D7).
const SandboxClass = "microvm"

// OwedHeader is the response trailer (gRPC metadata) that carries an actor's owed operation and its reason ("<op>: <reason>").
const OwedHeader = "herd-owed"

// Config wires a Server.
type Config struct {
	Store     store.Store
	Snapshots snapstore.Store
	Hub       *link.Hub
	Authn     auth.Authenticator
	Authz     auth.Authorizer
	// CallTimeout bounds each call to a herdlet (default 30 s).
	CallTimeout time.Duration
	// ReconcileCallTimeout bounds each call to a herdlet that Reconcile makes (default 4 × CallTimeout). A retry has
	// no client waiting on it, and work that outlasts CallTimeout (a large snapshot, a loaded disk) would time out the
	// same way on every pass and never converge; the herdlet also runs the retry only after an earlier exchange on
	// the same VM that it is still finishing, which this bound covers too.
	ReconcileCallTimeout time.Duration
	// GoldenWarmup is how long a template's golden actor runs before its snapshot is taken. Substrate waits 20 s
	// (herd-api's -golden-warmup default); zero takes it as soon as the actor runs.
	GoldenWarmup time.Duration
}

// Server is herd-api.
type Server struct {
	cfg Config
	// schedMu serializes placement so two placements never take the same free slot.
	schedMu sync.Mutex
	locks   sync.Map // store.Key → *sync.Mutex: one workflow per actor (or template) at a time in this process
	bg      sync.WaitGroup

	goldenMu     sync.Mutex
	goldenTimers map[store.Key]*time.Timer // golden passes waiting out a warmup
	closed       bool
}

// New returns a server. It registers itself as the hub's registration sink.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil || cfg.Snapshots == nil || cfg.Hub == nil || cfg.Authn == nil || cfg.Authz == nil {
		return nil, errors.New("apiserver: Store, Snapshots, Hub, Authn and Authz are required")
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 30 * time.Second
	}
	if cfg.ReconcileCallTimeout <= 0 {
		cfg.ReconcileCallTimeout = 4 * cfg.CallTimeout
	}
	s := &Server{cfg: cfg, goldenTimers: map[store.Key]*time.Timer{}}
	cfg.Hub.OnRegister = s.recordWorker
	cfg.Hub.OnExit = s.vmExited
	return s, nil
}

// Wait blocks until asynchronous workflows started by this server have finished.
func (s *Server) Wait() { s.bg.Wait() }

// Close stops starting background work (golden passes waiting out a warmup); Reconcile still runs what is owed.
func (s *Server) Close() {
	s.goldenMu.Lock()
	defer s.goldenMu.Unlock()
	s.closed = true
	for k, t := range s.goldenTimers {
		t.Stop()
		delete(s.goldenTimers, k)
	}
}

func (s *Server) lock(k store.Key) func() {
	m, _ := s.locks.LoadOrStore(k, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// reconciling marks a context as Reconcile's: its herdlet calls are bounded by ReconcileCallTimeout.
type reconciling struct{}

func (s *Server) call(ctx context.Context, worker string, w link.Work) link.Outcome {
	timeout := s.cfg.CallTimeout
	if ctx.Value(reconciling{}) != nil {
		timeout = s.cfg.ReconcileCallTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.cfg.Hub.Call(ctx, worker, w)
}

// principal authenticates the caller of ctx.
func (s *Server) principal(ctx context.Context) (auth.Principal, error) {
	var h string
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("authorization"); len(v) > 0 {
			h = v[0]
		}
	}
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || tok == "" {
		return auth.Principal{}, status.Errorf(codes.Unauthenticated, "no bearer token")
	}
	p, err := s.cfg.Authn.Authenticate(ctx, tok)
	if err != nil {
		return auth.Principal{}, status.Errorf(codes.Unauthenticated, "%v", err)
	}
	return p, nil
}

// authorize authenticates the caller and checks the action in space: authority before effect.
func (s *Server) authorize(ctx context.Context, space string, a auth.Action) error {
	if space == "" {
		return status.Errorf(codes.InvalidArgument, "no atespace named")
	}
	p, err := s.principal(ctx)
	if err != nil {
		return err
	}
	if err := s.cfg.Authz.Authorize(p, space, a); err != nil {
		return status.Errorf(codes.PermissionDenied, "%v", err)
	}
	return nil
}

// status maps a domain error to a gRPC status.
func toStatus(err error) error {
	var st interface{ GRPCStatus() *status.Status }
	var nf *sched.NoFitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &st):
		return err
	case errors.Is(err, store.ErrNotFound):
		return status.Errorf(codes.NotFound, "%v", err)
	case errors.Is(err, store.ErrExists):
		return status.Errorf(codes.AlreadyExists, "%v", err)
	case errors.Is(err, store.ErrConflict):
		return status.Errorf(codes.Aborted, "%v", err)
	case errors.As(err, &nf):
		return status.Errorf(codes.ResourceExhausted, "%v", err)
	case errors.Is(err, context.DeadlineExceeded):
		return status.Errorf(codes.DeadlineExceeded, "%v", err)
	case errors.Is(err, context.Canceled):
		return status.Errorf(codes.Canceled, "%v", err)
	}
	return status.Errorf(codes.Internal, "%v", err)
}

func ref(r *ateapi.ObjectRef) (space, name string, err error) {
	if r.GetAtespace() == "" || r.GetName() == "" {
		return "", "", status.Errorf(codes.InvalidArgument, "object reference needs an atespace and a name, got %q/%q", r.GetAtespace(), r.GetName())
	}
	return r.GetAtespace(), r.GetName(), nil
}

// meta writes the record's identity into a resource's metadata: the store is the authority for uid and version.
func meta(r store.Record) *ateapi.ResourceMetadata {
	return &ateapi.ResourceMetadata{
		Atespace: r.Space, Name: r.Name, Uid: r.UID, Version: r.Version,
		CreateTime: timestamppb.New(r.CreateTime), UpdateTime: timestamppb.New(r.UpdateTime),
	}
}

func decode[M proto.Message](r store.Record, m M) (M, error) {
	if err := proto.Unmarshal(r.Data, m); err != nil {
		return m, fmt.Errorf("apiserver: %v: decode: %w", r.Key, err)
	}
	return m, nil
}

// precondition checks a client's uid+version against the record. Updates must name both.
func precondition(r store.Record, uid string, version int64, required bool) error {
	if required && (uid == "" || version == 0) {
		return status.Errorf(codes.InvalidArgument, "%v: an update must carry metadata.uid and metadata.version", r.Key)
	}
	if (uid != "" && uid != r.UID) || (version != 0 && version != r.Version) {
		return toStatus(&store.ConflictError{Key: r.Key, WantUID: uid, WantVersion: version, HaveUID: r.UID, HaveV: r.Version})
	}
	return nil
}

// ensureSpace records an atespace the first time something is created in it.
func (s *Server) ensureSpace(ctx context.Context, space string) error {
	_, err := s.cfg.Store.Create(ctx, store.Record{Key: store.Key{Kind: store.KindSpace, Name: space}})
	if err != nil && !errors.Is(err, store.ErrExists) {
		return err
	}
	return nil
}
