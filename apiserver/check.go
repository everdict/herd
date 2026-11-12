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
	if p.Herdlet || p.Join {
		return nil, status.Errorf(codes.PermissionDenied, "%q: a herdlet or join credential cannot reach actors", p.Subject)
	}
	if req.Atespace == "" || req.Name == "" {
		return nil, status.Errorf(codes.InvalidArgument, "an actor reference needs an atespace and a name")
	}
	wctx, err := s.authorizeActorWrite(ctx, req.Atespace, req.Name)
	if err != nil {
		return nil, err
	}
	ar, err := s.loadActorToWrite(wctx, req.Atespace, req.Name)
	if err != nil {
		return nil, toStatus(err)
	}
	if p.Scheme == auth.SchemeHS256 {
		auth.CountBreakGlass(s.cfg.Meter, "herd-router", p.Subject)
	}
	resp := &CheckActorWriteResponse{UID: ar.rec.UID}
	if !p.Expires.IsZero() {
		resp.Expires = p.Expires.Unix()
	}
	return resp, nil
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
	}}},
}

// CallCheckActorWrite asks herd-api over cc, as the bearer token's caller.
func CallCheckActorWrite(ctx context.Context, cc grpc.ClientConnInterface, token, space, name string) (*CheckActorWriteResponse, error) {
	var resp CheckActorWriteResponse
	err := cc.Invoke(ctx, CheckActorWriteMethod, &CheckActorWriteRequest{Atespace: space, Name: name}, &resp,
		grpc.CallContentSubtype(link.CodecName), grpc.PerRPCCredentials(auth.Bearer(token)))
	return &resp, err
}
