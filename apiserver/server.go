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
	"log"
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

// StoreIDHeader is the response metadata (gRPC header, and trailer: an error answer has no header of its own) that
// names the record store herd-api answers from on every Control and Capabilities call (StoreID, PLT-458).
const StoreIDHeader = "herd-store-id"

// RestoredFidelityHeader is the response metadata (gRPC header, and trailer) that names the fidelity a running actor's VM
// was really restored at, in Substrate's SnapshotFidelity names (SNAPSHOT_FIDELITY_MEMORY, _ROOTFS, _VOLUMES): on a
// ResumeActor, on a CreateActor that restored from a tag, and on GetActor, so a resume whose answer was owed is read
// there once it is settled. A MEMORY snapshot the herdlet cold-booted from its volume tree is VOLUMES. Absent for a
// cold boot, and where the herdlet did not say (one older than PLT-496).
const RestoredFidelityHeader = "herd-restored-fidelity"

// Config wires a Server.
type Config struct {
	Store     store.Store
	Snapshots snapstore.Store
	Hub       *link.Hub
	Authn     auth.Authenticator
	Authz     auth.Authorizer
	// Meter counts the writes on actors with no creator recorded (auth.MetricUnownedActorWrites); nil counts nothing.
	Meter *auth.Meter
	// CallTimeout bounds each call to a herdlet (default 30 s), and each snapshot store call of a workflow that moves no
	// object bytes (a resume's Stat, a delete's Delete: storeCall).
	CallTimeout time.Duration
	// ReconcileCallTimeout bounds each call to a herdlet that Reconcile makes (default 4 × CallTimeout). A retry has
	// no client waiting on it, and work that outlasts CallTimeout (a large snapshot, a loaded disk) would time out the
	// same way on every pass and never converge; the herdlet also runs the retry only after an earlier exchange on
	// the same VM that it is still finishing, which this bound covers too.
	ReconcileCallTimeout time.Duration
	// RestoreBandwidth is the slowest rate, in bytes per second, at which a herdlet is expected to read a snapshot
	// object it restores from the snapshot store, or to write one it takes (default DefaultRestoreBandwidth). A
	// restore's call waits its usual timeout plus the object's size at this rate, so a node that does not hold the
	// object yet (a golden fetched from S3 for the first time) is not cut off by a timeout sized for work that moves no
	// bytes (PLT-227). A snapshot's call does the same with the size the herdlet reports as its object grows
	// (link.Progress), so a large golden stores without a longer CallTimeout (DEFAUL-1601).
	RestoreBandwidth int64
	// GoldenWarmup is how long a template's golden actor runs before its snapshot is taken. Substrate waits 20 s
	// (herd-api's -golden-warmup default); zero takes it as soon as the actor runs.
	GoldenWarmup time.Duration
	// GoldenRetries is how many times Reconcile builds a template's failed golden snapshot again, with no change to the
	// template (default DefaultGoldenRetries; negative: never). A failure that a retry can heal (a golden actor lost
	// when herd-api restarted, a herdlet that could not store the snapshot) heals; one that cannot ends after them.
	GoldenRetries int
	// GoldenRetryBackoff is how long after a failed golden build the first retry waits; each retry after it waits
	// twice as long as the one before (default DefaultGoldenRetryBackoff).
	GoldenRetryBackoff time.Duration
	// GoldenRebuilds is how many templates' golden snapshots Reconcile rebuilds at a time because the guest that built
	// them no longer runs on any connected worker (DEFAUL-1606; default DefaultGoldenRebuilds, negative: none).
	GoldenRebuilds int
	// SnapshotStore names Snapshots as herdlets declare theirs (link.Register.SnapshotStore): a herdlet that declares
	// another store is refused its link, since herd-api would find none of its snapshots (PLT-382). Empty: no check.
	SnapshotStore string
	// SweepGrace is how old an object no record names must be before SweepSnapshots removes it (default
	// DefaultSweepGrace; negative: no grace), so a snapshot written a moment ago, whose record is not saved yet, stays.
	SweepGrace time.Duration
}

// Server is herd-api.
type Server struct {
	cfg Config
	// schedMu serializes placement so two placements never take the same free slot.
	schedMu sync.Mutex
	locks   sync.Map // store.Key → *sync.Mutex: one workflow per actor (or template) at a time in this process
	bg      sync.WaitGroup

	// refsMu serializes the releases of snapshot objects (dropObject) with the records that name them: it is held
	// from a release's read of the records until the releasing record is written, and from a tag's check of the
	// object it references until the tag is recorded (A-45).
	refsMu sync.Mutex

	// rebuildMu serializes Reconcile's template loop, which alone begins golden rebuilds and bounds how many run.
	rebuildMu    sync.Mutex
	goldenMu     sync.Mutex
	goldenTimers map[store.Key]*time.Timer // golden passes waiting out a warmup
	// pulls are the image_pull_auth and image_plain_http_hosts of templates whose golden build has not ended, by
	// template uid: in memory
	// only, never stored (PLT-330).
	pulls  map[string]goldenPull
	closed bool

	// storeID names cfg.Store (StoreID).
	storeID string
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
	if cfg.RestoreBandwidth <= 0 {
		cfg.RestoreBandwidth = DefaultRestoreBandwidth
	}
	if cfg.GoldenRetries == 0 {
		cfg.GoldenRetries = DefaultGoldenRetries
	}
	if cfg.GoldenRetryBackoff <= 0 {
		cfg.GoldenRetryBackoff = DefaultGoldenRetryBackoff
	}
	if cfg.GoldenRebuilds == 0 {
		cfg.GoldenRebuilds = DefaultGoldenRebuilds
	}
	id, err := storeIdentity(context.Background(), cfg.Store)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, goldenTimers: map[store.Key]*time.Timer{}, pulls: map[string]goldenPull{}, storeID: id}
	cfg.Hub.StoreID = id
	cfg.Hub.OnRegister = s.recordWorker
	cfg.Hub.OnExit = s.vmExited
	cfg.Hub.OnLinked = s.herdletLinked
	return s, nil
}

// storeIdentity is the id of st: the uid of its identity record (store.StoreKey), which the first herd-api to use the
// store creates and no one changes. A store older than PLT-458 gets its id from the first herd-api that runs on it.
func storeIdentity(ctx context.Context, st store.Store) (string, error) {
	for {
		r, err := st.Get(ctx, store.StoreKey)
		if err == nil {
			return r.UID, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("apiserver: reading the store's id: %w", err)
		}
		r, err = st.Create(ctx, store.Record{Key: store.StoreKey})
		if err == nil {
			return r.UID, nil
		}
		if !errors.Is(err, store.ErrExists) {
			return "", fmt.Errorf("apiserver: naming the store: %w", err)
		}
	}
}

// StoreID names the record store this herd-api answers from: a uuid made with the store and never changed (PLT-458).
// Every API answer carries it (StoreIDHeader) and every herdlet's link learns it (link.Registered.StoreID), so a caller
// can tell an answer from another store — where every actor it knows is NOT_FOUND — from an answer about its actors.
func (s *Server) StoreID() string { return s.storeID }

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

// DefaultRestoreBandwidth is Config.RestoreBandwidth's default: 8 MiB/s, so a 512 MiB MEMORY bundle adds 64 s to its
// restore's call.
const DefaultRestoreBandwidth = 8 << 20

// reconciling marks a context as Reconcile's: its herdlet calls are bounded by ReconcileCallTimeout.
type reconciling struct{}

// moving carries the size of the snapshot object a herdlet call reads: the call waits for it at RestoreBandwidth on
// top of its timeout.
type moving struct{}

// call sends w to the worker and waits its timeout plus the bytes it moves at RestoreBandwidth: the size of the object
// a restore reads (moving), or the most a snapshot's herdlet reported its object has grown to (link.Progress).
func (s *Server) call(ctx context.Context, worker string, w link.Work) link.Outcome {
	timeout := s.callTimeout(ctx)
	known, _ := ctx.Value(moving{}).(int64)
	grow := func(n int64) time.Duration {
		return time.Duration(float64(max(known, n, 0)) / float64(s.cfg.RestoreBandwidth) * float64(time.Second))
	}
	return s.cfg.Hub.CallSized(ctx, worker, w, timeout, grow)
}

// callTimeout is the bound of one call that moves no snapshot bytes: CallTimeout, or ReconcileCallTimeout in
// Reconcile.
func (s *Server) callTimeout(ctx context.Context) time.Duration {
	if ctx.Value(reconciling{}) != nil {
		return s.cfg.ReconcileCallTimeout
	}
	return s.cfg.CallTimeout
}

// storeCall bounds one snapshot store call that moves no object bytes (a Stat, a Delete) as a herdlet call is bounded
// (callTimeout). A store that stops answering (an S3 endpoint frozen) then fails the call within the bound, and the
// workflow answers unknown, owed to Reconcile, instead of waiting for the store (PLT-375).
func (s *Server) storeCall(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.callTimeout(ctx))
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
	if (!ok || tok == "") && len(auth.PeerChain(ctx)) == 0 {
		return auth.Principal{}, status.Errorf(codes.Unauthenticated, "no bearer token and no client certificate")
	}
	// No bearer: the caller is its mTLS client certificate, an X.509-SVID the authenticator verifies (ADR 0001 A-60).
	p, err := s.cfg.Authn.Authenticate(ctx, tok)
	if err != nil {
		return auth.Principal{}, status.Errorf(codes.Unauthenticated, "%v", err)
	}
	return p, nil
}

// authorize authenticates the caller and checks the action in space: authority before effect.
func (s *Server) authorize(ctx context.Context, space string, a auth.Action) error {
	_, err := s.authorizeAs(ctx, space, a)
	return err
}

// authorizeAs is authorize answering the principal it authorized.
func (s *Server) authorizeAs(ctx context.Context, space string, a auth.Action) (auth.Principal, error) {
	if space == "" {
		return auth.Principal{}, status.Errorf(codes.InvalidArgument, "no atespace named")
	}
	p, err := s.principal(ctx)
	if err != nil {
		return auth.Principal{}, err
	}
	if err := s.cfg.Authz.Authorize(p, space, a); err != nil {
		return auth.Principal{}, status.Errorf(codes.PermissionDenied, "%v", err)
	}
	return p, nil
}

// writerKey carries the caller authorized to write one actor (asWriter).
type writerKey struct{}

type writer struct {
	p   auth.Principal
	key store.Key
}

// authorizeActorWrite authorizes a write on the existing actor space/name (W4, ADR 0001 A-60): the space must admit
// the write, and the returned context carries the caller so that the workflow, loading the actor under its lock
// (loadActorToWrite), allows it only to the actor's creator or an operator. A write role on the space no longer
// suffices.
func (s *Server) authorizeActorWrite(ctx context.Context, space, name string) (context.Context, error) {
	p, err := s.authorizeAs(ctx, space, auth.ActionWrite)
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, writerKey{}, writer{p: p, key: actorKey(space, name)}), nil
}

// loadActorToWrite loads the actor a write acts on. When ctx carries the caller authorized to write this actor
// (authorizeActorWrite), the caller must be its creator or an operator, and a write on an actor with no creator
// recorded is counted under its allowance; herd-api's own workflows (Reconcile, golden builds, a template's delete
// of its golden actor) carry none for the actor they load.
func (s *Server) loadActorToWrite(ctx context.Context, space, name string) (*actorRec, error) {
	ar, err := s.loadActor(ctx, space, name)
	if err != nil {
		return nil, err
	}
	w, ok := ctx.Value(writerKey{}).(writer)
	if !ok || w.key != ar.rec.Key {
		return ar, nil
	}
	creator := ar.actor.GetStatus().GetCreator()
	a, err := auth.ActorWriter(w.p, space, creator)
	if err != nil {
		return nil, status.Errorf(codes.PermissionDenied, "actor %s/%s: %v", space, name, err)
	}
	if a.Counted() {
		s.cfg.Meter.Inc(auth.MetricUnownedActorWrites, "allowance", string(a), "caller", w.p.Creator())
		log.Printf("herd-api: actor %s/%s has no creator recorded; %s writes it under the %s allowance (ADR 0001 A-60)", space, name, w.p.Creator(), a)
	}
	return ar, nil
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
