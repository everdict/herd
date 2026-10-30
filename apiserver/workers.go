package apiserver

import (
	"context"
	"errors"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/link"
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
	return s.cfg.Hub.Serve(context.WithValue(ctx, herdletKey{}, p.Subject), link.NewGRPCStream(ss))
}
