package apiserver

import (
	"context"
	"errors"
	"sort"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/sched"
	"github.com/everdict/herd/store"
	"github.com/everdict/herd/vmm"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func workerCapabilities(r link.Register) *herdext.WorkerCapabilities {
	p := herdext.Platform_PLATFORM_UNSPECIFIED
	switch r.Capabilities.Platform {
	case vmm.PlatformCloudHypervisor:
		p = herdext.Platform_PLATFORM_LINUX_CLOUD_HYPERVISOR
	case vmm.PlatformVZ:
		p = herdext.Platform_PLATFORM_DARWIN_VZ
	}
	return &herdext.WorkerCapabilities{
		Worker: r.Worker, Platform: p, Arch: r.Capabilities.Arch,
		SuspendMaxFidelity:    ateapi.SnapshotFidelity(r.Capabilities.SuspendMaxFidelity),
		RestoreFamily:         r.Capabilities.RestoreFamily,
		PortableRestoreFamily: r.Capabilities.PortableRestoreFamily,
		HypervisorVersion:     r.Capabilities.HypervisorVersion,
		Reason:                r.Capabilities.Reason,
		EgressAllowPrefixes:   r.Capabilities.EgressAllowPrefixes,
	}
}

type herdletKey struct{}

// recordWorker keeps a Worker record of what each herdlet declared, so the capabilities survive the link.
func (s *Server) recordWorker(ctx context.Context, r link.Register) error {
	// A herdlet credential names one worker: it may register only itself.
	if sub, ok := ctx.Value(herdletKey{}).(string); ok && sub != r.Worker {
		return status.Errorf(codes.PermissionDenied, "herdlet credential %q may not register worker %q", sub, r.Worker)
	}
	if want := s.cfg.SnapshotStore; want != "" && r.SnapshotStore != "" && r.SnapshotStore != want {
		return status.Errorf(codes.FailedPrecondition, "herdlet %s keeps snapshots in %s, herd-api in %s, where it looks for them: "+
			"the herdlet's join grant names another snapshot store than herd-api uses now; join that host again", r.Worker, r.SnapshotStore, want)
	}
	data, err := proto.Marshal(workerCapabilities(r))
	if err != nil {
		return err
	}
	k := store.Key{Kind: store.KindWorker, Name: r.Worker}
	for {
		cur, err := s.cfg.Store.Get(ctx, k)
		if errors.Is(err, store.ErrNotFound) {
			_, err = s.cfg.Store.Create(ctx, store.Record{Key: k, Data: data})
			if errors.Is(err, store.ErrExists) {
				continue
			}
			return err
		}
		if err != nil {
			return err
		}
		cur.Data = data
		_, err = s.cfg.Store.Update(ctx, cur)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		return err
	}
}

// GetWorkerCapabilities returns one worker's declared capabilities.
func (s *Server) GetWorkerCapabilities(ctx context.Context, req *herdext.GetWorkerCapabilitiesRequest) (*herdext.WorkerCapabilities, error) {
	if _, err := s.principal(ctx); err != nil {
		return nil, err
	}
	r, err := s.cfg.Store.Get(ctx, store.Key{Kind: store.KindWorker, Name: req.GetWorker()})
	if err != nil {
		return nil, toStatus(err)
	}
	c, err := decode(r, &herdext.WorkerCapabilities{})
	return c, toStatus(err)
}

// ListWorkerCapabilities returns every worker's declared capabilities.
func (s *Server) ListWorkerCapabilities(ctx context.Context, _ *herdext.ListWorkerCapabilitiesRequest) (*herdext.ListWorkerCapabilitiesResponse, error) {
	if _, err := s.principal(ctx); err != nil {
		return nil, err
	}
	recs, err := s.cfg.Store.List(ctx, store.KindWorker, "")
	if err != nil {
		return nil, toStatus(err)
	}
	resp := &herdext.ListWorkerCapabilitiesResponse{}
	for _, r := range recs {
		c, err := decode(r, &herdext.WorkerCapabilities{})
		if err != nil {
			return nil, toStatus(err)
		}
		resp.Workers = append(resp.Workers, c)
	}
	return resp, nil
}

// serveLink accepts one herdlet's outbound link. Only a herdlet credential may open it.
func (s *Server) serveLink(ss grpc.ServerStream) error {
	ctx := ss.Context()
	p, err := s.principal(ctx)
	if err != nil {
		return err
	}
	if !p.Herdlet {
		return status.Errorf(codes.PermissionDenied, "%q is not a herdlet credential", p.Subject)
	}
	return s.cfg.Hub.Serve(context.WithValue(ctx, herdletKey{}, p.HerdletWorker()), link.NewGRPCStream(ss))
}

// CompatSchema is the schema_version of the compat attributes ListWorkers reports for a worker's sandbox runtime:
// architecture, platform, suspend_max_fidelity (a SnapshotFidelity name), restore_family, portable_restore_family
// and hypervisor_version, each as the herdlet declared it; an empty one is left out.
const CompatSchema = "herd-v1"

// substrateWorker is Substrate's view of a worker record: what its herdlet declared (c), and, while it is connected
// to this herd-api, its class, labels, its slot ceiling and cpu and memory, and what is held of them (live).
func substrateWorker(r store.Record, c *herdext.WorkerCapabilities, live *sched.Worker) *ateapi.Worker {
	var attrs []*ateapi.AttributeEntry
	for _, kv := range [][2]string{
		{"architecture", c.GetArch()},
		{"hypervisor_version", c.GetHypervisorVersion()},
		{"platform", platformName(c.GetPlatform())},
		{"portable_restore_family", c.GetPortableRestoreFamily()},
		{"restore_family", c.GetRestoreFamily()},
		{"suspend_max_fidelity", c.GetSuspendMaxFidelity().String()},
	} {
		if kv[1] != "" {
			attrs = append(attrs, &ateapi.AttributeEntry{Key: kv[0], Value: kv[1]})
		}
	}
	st := &ateapi.WorkerStatus{State: ateapi.WorkerState_WORKER_STATE_UNAVAILABLE}
	class := SandboxClass
	w := &ateapi.Worker{Metadata: meta(r), Status: st}
	if live != nil {
		st.State = ateapi.WorkerState_WORKER_STATE_ACTIVE
		// cpu and memory as the herdlet reported them and as herd-api's actors on it are sized (PLT-452); actors is the
		// slot ceiling (0: none) and the actors held.
		st.Capacity = &ateapi.WorkerResources{Actors: int32(live.Slots), CpuMillicores: live.MilliCPU, MemoryBytes: live.MemoryMiB << 20}
		st.Allocated = &ateapi.WorkerResources{Actors: int32(live.Used), CpuMillicores: live.UsedMilliCPU, MemoryBytes: live.UsedMemoryMiB << 20}
		w.Labels = live.Labels
		if live.Class != "" {
			class = live.Class
		}
	}
	st.SandboxClasses = []*ateapi.WorkerSandboxClass{{SandboxClass: class, DefaultRuntime: &ateapi.WorkerSandboxRuntime{
		Version: &ateapi.VersionedSandboxCompat{SchemaVersion: CompatSchema, Attributes: attrs}}}}
	return w
}

// platformName is the vmm.Platform a herdext.Platform stands for ("" for none).
func platformName(p herdext.Platform) string {
	switch p {
	case herdext.Platform_PLATFORM_LINUX_CLOUD_HYPERVISOR:
		return string(vmm.PlatformCloudHypervisor)
	case herdext.Platform_PLATFORM_DARWIN_VZ:
		return string(vmm.PlatformVZ)
	}
	return ""
}

// ListWorkers is Substrate's Control.ListWorkers over herd's worker records, ordered by name and paged like
// ListActors. Workers are global: any caller the Authorizer lets read in some atespace may list them (a herdlet or
// join credential, which holds no atespace, may not).
func (s *Server) ListWorkers(ctx context.Context, req *ateapi.ListWorkersRequest) (*ateapi.ListWorkersResponse, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	if !s.readsSomeSpace(p) {
		return nil, status.Errorf(codes.PermissionDenied, "%q may not read in any atespace, so it may not list workers", p.Subject)
	}
	recs, err := s.cfg.Store.List(ctx, store.KindWorker, "")
	if err != nil {
		return nil, toStatus(err)
	}
	ws, err := s.workers(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	live := map[string]*sched.Worker{}
	for i := range ws {
		live[ws[i].Name] = &ws[i]
	}
	size := int(req.GetOptions().GetPageSize())
	if size <= 0 || size > 1000 {
		size = 100
	}
	after := req.GetOptions().GetPageToken()
	i := sort.Search(len(recs), func(i int) bool { return recs[i].Name > after })
	resp := &ateapi.ListWorkersResponse{}
	for ; i < len(recs) && len(resp.Workers) < size; i++ {
		c, err := decode(recs[i], &herdext.WorkerCapabilities{})
		if err != nil {
			return nil, toStatus(err)
		}
		resp.Workers = append(resp.Workers, substrateWorker(recs[i], c, live[recs[i].Name]))
	}
	if i < len(recs) && len(resp.Workers) > 0 {
		resp.NextPageToken = resp.Workers[len(resp.Workers)-1].GetMetadata().GetName()
	}
	return resp, nil
}

// readsSomeSpace is whether the Authorizer lets p read in at least one of the atespaces it names.
func (s *Server) readsSomeSpace(p auth.Principal) bool {
	if p.Herdlet || p.Join {
		return false
	}
	for space := range p.Spaces {
		if s.cfg.Authz.Authorize(p, space, auth.ActionRead) == nil {
			return true
		}
	}
	return false
}
