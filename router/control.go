package router

import (
	"context"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Control is what the router asks of herd-api. Every call carries the client's own bearer token, so herd-api
// authorizes the resume for the client's atespace as it would a direct call.
type Control interface {
	GetActor(ctx context.Context, token, space, name string) (*ateapi.Actor, error)
	ResumeActor(ctx context.Context, token, space, name string) error
	// CheckActorWrite asks herd-api whether the token's caller may write the actor (herd-api's own policy and the
	// actor's creator): the decision, for one actor uid, or the refusal as a gRPC status.
	CheckActorWrite(ctx context.Context, token, space, name string) (ActorWriteDecision, error)
}

// ActorWriteDecision is herd-api's answer to CheckActorWrite: the actor uid decided for, and when the caller's
// credential ends (zero: no end known).
type ActorWriteDecision struct {
	UID     string
	Expires time.Time
}

// CheckActorWriteMethod is herd-api's apiserver.CheckActorWriteMethod (router_test holds the two equal).
const CheckActorWriteMethod = "/herd.ext.v1.Authz/CheckActorWrite"

// jsonCodec is the content-subtype of herd's own JSON services (link.CodecName).
const jsonCodec = "json"

func (c GRPCControl) CheckActorWrite(ctx context.Context, token, space, name string) (ActorWriteDecision, error) {
	var resp struct {
		UID     string `json:"uid"`
		Expires int64  `json:"expires,omitempty"`
	}
	req := struct {
		Atespace string `json:"atespace"`
		Name     string `json:"name"`
	}{space, name}
	if err := c.CC.Invoke(withToken(ctx, token), CheckActorWriteMethod, &req, &resp, grpc.CallContentSubtype(jsonCodec)); err != nil {
		return ActorWriteDecision{}, err
	}
	d := ActorWriteDecision{UID: resp.UID}
	if resp.Expires > 0 {
		d.Expires = time.Unix(resp.Expires, 0)
	}
	return d, nil
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
