package router

import (
	"context"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Control is what the router asks of herd-api. Every call carries the client's own bearer token, so herd-api
// authorizes the resume for the client's atespace as it would a direct call.
type Control interface {
	GetActor(ctx context.Context, token, space, name string) (*ateapi.Actor, error)
	ResumeActor(ctx context.Context, token, space, name string) error
}

// GRPCControl is Control over a connection to herd-api (the router dials herd-api, never a node).
type GRPCControl struct{ CC grpc.ClientConnInterface }

const controlService = "/substrate.ateapi.v1.Control/"

func withToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func (c GRPCControl) GetActor(ctx context.Context, token, space, name string) (*ateapi.Actor, error) {
	a := new(ateapi.Actor)
	err := c.CC.Invoke(withToken(ctx, token), controlService+"GetActor",
		&ateapi.GetActorRequest{Actor: &ateapi.ObjectRef{Atespace: space, Name: name}}, a)
	return a, err
}

func (c GRPCControl) ResumeActor(ctx context.Context, token, space, name string) error {
	return c.CC.Invoke(withToken(ctx, token), controlService+"ResumeActor",
		&ateapi.ResumeActorRequest{Actor: &ateapi.ObjectRef{Atespace: space, Name: name}}, new(ateapi.ResumeActorResponse))
}
