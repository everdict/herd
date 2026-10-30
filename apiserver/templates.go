package apiserver

import (
	"context"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/auth"
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

// CreateActorTemplate stores a template.
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
	t.Metadata, t.Status = nil, nil
	data, err := proto.Marshal(t)
	if err != nil {
		return nil, toStatus(err)
	}
	if err := s.ensureSpace(ctx, space); err != nil {
		return nil, toStatus(err)
	}
	r, err := s.cfg.Store.Create(ctx, store.Record{Key: store.Key{Kind: store.KindActorTemplate, Space: space, Name: name}, Data: data})
	if err != nil {
		return nil, toStatus(err)
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

// DeleteActorTemplate deletes a template no actor uses.
func (s *Server) DeleteActorTemplate(ctx context.Context, req *ateapi.DeleteActorTemplateRequest) (*ateapi.ActorTemplate, error) {
	space, name, err := ref(req.GetActorTemplate())
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, space, auth.ActionWrite); err != nil {
		return nil, err
	}
	r, err := s.cfg.Store.Get(ctx, store.Key{Kind: store.KindActorTemplate, Space: space, Name: name})
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
		if ar.st.TemplateUID == r.UID {
			return nil, status.Errorf(codes.FailedPrecondition, "template %s/%s is used by actor %s", space, name, a.Name)
		}
	}
	v, err := templateView(r)
	if err != nil {
		return nil, toStatus(err)
	}
	if err := s.cfg.Store.Delete(ctx, r.Key, r.UID, r.Version); err != nil {
		return nil, toStatus(err)
	}
	return v, nil
}
