package conformance

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"

	"github.com/everdict/herd/apiserver"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/snapstore"
	"github.com/everdict/herd/store"
	"google.golang.org/grpc"
)

// errEnded is what a herd-api process that ended gets from every store call it would still make: in the test's
// process its goroutines run on, but a process that ended writes nothing more.
var errEnded = errors.New("conformance: this herd-api process has ended")

// apiProcess is one herd-api process of an Env: its server, hub and gRPC port, and the stores as it reaches them.
type apiProcess struct {
	ended  atomic.Bool
	g      *grpc.Server
	served chan struct{}
}

func (p *apiProcess) stop() {
	p.g.Stop()
	<-p.served
}

// serve starts a herd-api process on ln over the Env's records and snapshot store.
func (e *Env) serve(t testing.TB, ln net.Listener) {
	t.Helper()
	p := &apiProcess{served: make(chan struct{})}
	e.Hub = link.NewHub()
	var err error
	e.Server, err = apiserver.New(apiserver.Config{
		Store: endedRecords{Store: e.Store, p: p}, Snapshots: endedSnapshots{Store: e.Snapshots, p: p}, Hub: e.Hub,
		Authn: auth.StaticKey{Key: e.Key}, Authz: auth.SpaceRoles{},
		CallTimeout:          CallTimeout,
		ReconcileCallTimeout: ReconcileCallTimeout,
		GoldenWarmup:         GoldenWarmup,
		GoldenRetryBackoff:   GoldenRetryBackoff,
		GoldenRebuilds:       GoldenRebuilds,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.g = grpc.NewServer()
	e.Server.Register(p.g)
	go func() { defer close(p.served); _ = p.g.Serve(ln) }()
	e.api = p
}

// RestartAPI ends the herd-api process as a kill does, wherever its work is (a call to a herdlet in flight goes on in
// the test's process, but nothing it does after this reaches the records or the snapshot store), and starts a new one
// on the same address over the same records and snapshot store. Every herdlet links to the new one, as a herdlet
// dials again after its link broke; their VMs stay.
func (e *Env) RestartAPI(t testing.TB) {
	t.Helper()
	old := e.api
	old.ended.Store(true)
	old.stop()
	e.Server.Close()
	t.Cleanup(e.Server.Wait)
	ln, err := net.Listen("tcp", e.Addr)
	if err != nil {
		t.Fatal(err)
	}
	e.serve(t, ln)
	for name := range e.Herdlets {
		e.Reconnect(t, name)
	}
}

// endedRecords is the record store as one herd-api process reaches it.
type endedRecords struct {
	store.Store
	p *apiProcess
}

func (r endedRecords) Get(ctx context.Context, k store.Key) (store.Record, error) {
	if r.p.ended.Load() {
		return store.Record{}, errEnded
	}
	return r.Store.Get(ctx, k)
}

func (r endedRecords) Create(ctx context.Context, rec store.Record) (store.Record, error) {
	if r.p.ended.Load() {
		return store.Record{}, errEnded
	}
	return r.Store.Create(ctx, rec)
}

func (r endedRecords) Update(ctx context.Context, rec store.Record) (store.Record, error) {
	if r.p.ended.Load() {
		return store.Record{}, errEnded
	}
	return r.Store.Update(ctx, rec)
}

func (r endedRecords) Delete(ctx context.Context, k store.Key, uid string, version int64) error {
	if r.p.ended.Load() {
		return errEnded
	}
	return r.Store.Delete(ctx, k, uid, version)
}

func (r endedRecords) List(ctx context.Context, kind store.Kind, space string) ([]store.Record, error) {
	if r.p.ended.Load() {
		return nil, errEnded
	}
	return r.Store.List(ctx, kind, space)
}

// endedSnapshots is the snapshot store as one herd-api process reaches it.
type endedSnapshots struct {
	snapstore.Store
	p *apiProcess
}

func (s endedSnapshots) Put(ctx context.Context, key string, r io.Reader) (snapstore.Object, error) {
	if s.p.ended.Load() {
		return snapstore.Object{}, errEnded
	}
	return s.Store.Put(ctx, key, r)
}

func (s endedSnapshots) Stat(ctx context.Context, key string) (snapstore.Object, error) {
	if s.p.ended.Load() {
		return snapstore.Object{}, errEnded
	}
	return s.Store.Stat(ctx, key)
}

func (s endedSnapshots) Verify(ctx context.Context, key, digest string) (snapstore.Object, error) {
	if s.p.ended.Load() {
		return snapstore.Object{}, errEnded
	}
	return s.Store.Verify(ctx, key, digest)
}

func (s endedSnapshots) Prove(ctx context.Context, key, digest string) (snapstore.Object, error) {
	if s.p.ended.Load() {
		return snapstore.Object{}, errEnded
	}
	return s.Store.Prove(ctx, key, digest)
}

func (s endedSnapshots) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if s.p.ended.Load() {
		return nil, errEnded
	}
	return s.Store.Open(ctx, key)
}

func (s endedSnapshots) Delete(ctx context.Context, key string) error {
	if s.p.ended.Load() {
		return errEnded
	}
	return s.Store.Delete(ctx, key)
}

func (s endedSnapshots) Copy(ctx context.Context, from, to, digest string) (snapstore.Object, error) {
	if s.p.ended.Load() {
		return snapstore.Object{}, errEnded
	}
	return s.Store.Copy(ctx, from, to, digest)
}
