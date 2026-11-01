package apiserver

import (
	"context"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/link"
	"github.com/everdict/herd/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func templateView(r store.Record) (*ateapi.ActorTemplate, error) {
	t, err := decode(r, &ateapi.ActorTemplate{})
	if err != nil {
		return nil, err
	}
	t.Metadata = meta(r)
	return t, nil
}

// CreateActorTemplate stores a template and, unless its golden snapshot mode is DISABLED, starts building its golden
// snapshot (golden.go); status.golden_snapshot_status reports the build.
func (s *Server) CreateActorTemplate(ctx context.Context, req *ateapi.CreateActorTemplateRequest) (*ateapi.ActorTemplate, error) {
	in := req.GetActorTemplate()
	space, name := in.GetMetadata().GetAtespace(), in.GetMetadata().GetName()
	if err := s.authorize(ctx, space, auth.ActionWrite); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "metadata.name is required")
	}
	if len(in.GetContainers()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "a template needs at least one container")
	}
	t := proto.Clone(in).(*ateapi.ActorTemplate)
	// Status is server-owned and starts empty, as Substrate's does.
	t.Metadata, t.Status = nil, &ateapi.ActorTemplateStatus{}
	defaultGolden(t)
	data, err := proto.Marshal(t)
	if err != nil {
		return nil, toStatus(err)
	}
	if err := s.ensureSpace(ctx, space); err != nil {
		return nil, toStatus(err)
	}
	r, err := s.cfg.Store.Create(ctx, store.Record{Key: templateKey(space, name), Data: data})
	if err != nil {
		return nil, toStatus(err)
	}
	if goldenEnabled(t) {
		s.kickGolden(r.Key, 0)
	}
	v, err := templateView(r)
	return v, toStatus(err)
}

// GetActorTemplate returns a template.
func (s *Server) GetActorTemplate(ctx context.Context, req *ateapi.GetActorTemplateRequest) (*ateapi.ActorTemplate, error) {
	space, name, err := ref(req.GetActorTemplate())
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, space, auth.ActionRead); err != nil {
		return nil, err
	}
	r, err := s.cfg.Store.Get(ctx, store.Key{Kind: store.KindActorTemplate, Space: space, Name: name})
	if err != nil {
		return nil, toStatus(err)
	}
	v, err := templateView(r)
	return v, toStatus(err)
}

// DeleteActorTemplate deletes a template no actor uses, after its golden actor and golden tag, as Substrate does.
func (s *Server) DeleteActorTemplate(ctx context.Context, req *ateapi.DeleteActorTemplateRequest) (*ateapi.ActorTemplate, error) {
	space, name, err := ref(req.GetActorTemplate())
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, space, auth.ActionWrite); err != nil {
		return nil, err
	}
	// Serializes against the golden pass, which creates the golden actor and tag.
	unlock := s.lock(templateKey(space, name))
	defer unlock()
	r, err := s.cfg.Store.Get(ctx, templateKey(space, name))
	if err != nil {
		return nil, toStatus(err)
	}
	if err := precondition(r, req.GetOptions().GetUid(), req.GetOptions().GetVersion(), false); err != nil {
		return nil, err
	}
	actors, err := s.cfg.Store.List(ctx, store.KindActor, space)
	if err != nil {
		return nil, toStatus(err)
	}
	for _, a := range actors {
		ar, err := parseActor(a)
		if err != nil {
			return nil, toStatus(err)
		}
		if ar.st.TemplateUID == r.UID && !(ar.st.Golden && a.Name == r.UID) {
			return nil, status.Errorf(codes.FailedPrecondition, "template %s/%s is used by actor %s", space, name, a.Name)
		}
	}
	v, err := templateView(r)
	if err != nil {
		return nil, toStatus(err)
	}
	if err := s.deleteGolden(ctx, space, r.UID); err != nil {
		return nil, err
	}
	if err := s.cfg.Store.Delete(ctx, r.Key, r.UID, r.Version); err != nil {
		return nil, toStatus(err)
	}
	return v, nil
}

// templateSpec is the template as a herdlet needs it to boot a VM (link.Template). Only DurableDir volumes are
// herd's; a mount of any other volume kind is left for the herdlet to refuse.
func templateSpec(uid string, t *ateapi.ActorTemplate) *link.Template {
	out := &link.Template{UID: uid}
	for _, c := range t.GetContainers() {
		lc := link.Container{Name: c.GetName(), Image: c.GetImage(), Command: c.GetCommand(), Args: c.GetArgs()}
		for _, e := range c.GetEnv() {
			lc.Env = append(lc.Env, link.EnvVar{Name: e.GetName(), Value: e.GetValue()})
		}
		for _, m := range c.GetVolumeMounts() {
			lc.VolumeMounts = append(lc.VolumeMounts, link.VolumeMount{Name: m.GetName(), MountPath: m.GetMountPath()})
		}
		out.Containers = append(out.Containers, lc)
	}
	for _, v := range t.GetVolumes() {
		if v.GetDurableDir() != nil {
			out.Volumes = append(out.Volumes, v.GetName())
		}
	}
	if l := t.GetResources().GetLimits(); l != nil {
		out.VCPUs = int((l.GetCpuMillicores() + 999) / 1000)
		out.MemoryMiB = int(l.GetMemoryBytes() >> 20)
	}
	return out
}
