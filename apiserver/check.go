package apiserver

import (
	"context"

	"github.com/everdict/herd/auth"
	"github.com/everdict/herd/link"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// herd-api is the one decider of a write on an actor (ADR 0001 A-60, review 7): herd-router asks it, with its client's
// own bearer, before it carries a request to an actor, and keeps no policy of its own.
const (
	AuthzService          = "herd.ext.v1.Authz"
	CheckActorWriteMethod = "/" + AuthzService + "/CheckActorWrite"
)

// CheckActorWriteRequest names the actor a request is for.
type CheckActorWriteRequest struct {
	Atespace string `json:"atespace"`
	Name     string `json:"name"`
}

// CheckActorWriteResponse is the decision: the actor uid it was made for (a re-created actor of the name is decided
// again) and when the caller's credential ends (a stream it opened ends then), in Unix seconds (0: no end).
type CheckActorWriteResponse struct {
	UID     string `json:"uid"`
	Expires int64  `json:"expires,omitempty"`
}

// CheckActorWrite decides whether the caller may write the actor: authenticated, admitted to the space for a write,
// not a herdlet's or a join's credential, and the actor's creator or an operator. A write on an actor with no creator
// is counted under its allowance, and a break-glass bearer is counted once here for the component the client called,
// herd-router, whose forwarded calls are not counted again.
func (s *Server) CheckActorWrite(ctx context.Context, req *CheckActorWriteRequest) (*CheckActorWriteResponse, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	// The call herd-router forwards for a break-glass bearer is counted here, once and before the decision (a
	// refused one too), for herd-router; a direct caller's was counted by herd-api's authenticator (re-review 6a, 6c).
	if p.Scheme == auth.SchemeHS256 && s.cfg.IsRouter != nil && s.cfg.IsRouter(ctx) {
		auth.CountBreakGlass(s.cfg.Meter, "herd-router", p.Subject)
	}
	if p.Herdlet || p.Join {
		return nil, status.Errorf(codes.PermissionDenied, "%q: a herdlet or join credential cannot reach actors", p.Subject)
	}
	if req.Atespace == "" || req.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "an actor reference needs an atespace and a name")
	}
	// A decision writes nothing: the write it allows is counted where it happens (re-review 6b).
	wctx, err := s.actorWriteAs(ctx, p, req.Atespace, req.Name, true)
	if err != nil {
		return nil, err
	}
	ar, err := s.loadActorToWrite(wctx, req.Atespace, req.Name)
	if err != nil {
		return nil, toStatus(err)
	}
	resp := &CheckActorWriteResponse{UID: ar.rec.UID}
	// A stream the router carries is bounded only by an HS256 bearer's break-glass end; a JWT-SVID authorizes the
	// stream at its open, and its exp (SPIRE's default is 5 minutes) does not cut an exec or an attach.
	if !p.HardEnd.IsZero() {
		resp.Expires = p.HardEnd.Unix()
	}
	return resp, nil
}

// WorkerIdentityMethod answers herd-router the node SVID that registered a worker (re-review 5).
const WorkerIdentityMethod = "/" + AuthzService + "/WorkerIdentity"

// WorkerIdentityRequest names a worker; WorkerIdentityResponse is the node SVID that registered it ("" if none did).
type WorkerIdentityRequest struct {
	Worker string `json:"worker"`
}

type WorkerIdentityResponse struct {
	Identity string `json:"identity,omitempty"`
}

// WorkerIdentityCall answers herd-router alone (IsRouter): its tunnel server refuses an HS256 tunnel for a worker a
// node SVID registered.
func (s *Server) WorkerIdentityCall(ctx context.Context, req *WorkerIdentityRequest) (*WorkerIdentityResponse, error) {
	if s.cfg.IsRouter == nil || !s.cfg.IsRouter(ctx) {
		return nil, status.Errorf(codes.PermissionDenied, "only herd-router asks a worker's identity")
	}
	id, err := s.WorkerIdentity(ctx, req.Worker)
	if err != nil {
		return nil, toStatus(err)
	}
	return &WorkerIdentityResponse{Identity: id}, nil
}

var authzServiceDesc = grpc.ServiceDesc{
	ServiceName: AuthzService,
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{MethodName: "CheckActorWrite", Handler: func(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
		var req CheckActorWriteRequest
		if err := dec(&req); err != nil {
			return nil, err
		}
		return srv.(*Server).CheckActorWrite(ctx, &req)
	}}, {MethodName: "WorkerIdentity", Handler: func(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
		var req WorkerIdentityRequest
		if err := dec(&req); err != nil {
			return nil, err
		}
		return srv.(*Server).WorkerIdentityCall(ctx, &req)
	}}},
}

// CallCheckActorWrite asks herd-api over cc, as the bearer token's caller.
func CallCheckActorWrite(ctx context.Context, cc grpc.ClientConnInterface, token, space, name string) (*CheckActorWriteResponse, error) {
	var resp CheckActorWriteResponse
	err := cc.Invoke(ctx, CheckActorWriteMethod, &CheckActorWriteRequest{Atespace: space, Name: name}, &resp,
		grpc.CallContentSubtype(link.CodecName), grpc.PerRPCCredentials(auth.Bearer(token)))
	return &resp, err
}
