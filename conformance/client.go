// Package conformance is a Go conformance suite for the Substrate Control subset herd serves (ADR 0001, A-4),
// together with the harness that runs herd-api against fake herdlets over real gRPC: the herdlets dial in, and their
// hypervisor (vmm/fake) can be made to hang, to answer unknown, or to leave a VM behind on delete.
package conformance

import (
	"context"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/apiserver"
	"github.com/everdict/herd/auth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// Client is a typed Control client over gRPC.
type Client struct {
	c   *grpc.ClientConn
	err error
}

// NewClient dials addr with a bearer token (empty for none).
func NewClient(addr, token string) *Client {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(auth.Bearer(token)))
	}
	c, err := grpc.NewClient("passthrough:///"+addr, opts...)
	if err != nil {
		// NewClient fails only on a malformed target or options; calls on the nil conn would panic, so keep one
		// that answers every call with the error.
		return &Client{err: err}
	}
	return &Client{c: c}
}

// Close releases the client's connections.
func (c *Client) Close() {
	if c.c != nil {
		_ = c.c.Close()
	}
}

const control = "/substrate.ateapi.v1.Control/"

func call[R proto.Message](ctx context.Context, c *Client, method string, req proto.Message, resp R) (R, error) {
	if err := c.Raw(ctx, method, req, resp); err != nil {
		var zero R
		return zero, err
	}
	return resp, nil
}

// Raw calls any Control method by name.
func (c *Client) Raw(ctx context.Context, method string, req, resp proto.Message, opts ...grpc.CallOption) error {
	if c.err != nil {
		return c.err
	}
	return c.c.Invoke(ctx, control+method, req, resp, opts...)
}

// Conn is the underlying connection (nil if NewClient failed).
func (c *Client) Conn() *grpc.ClientConn { return c.c }

// GetActorOwed returns the actor and its owed operation from the herd-owed trailer ("" when nothing is owed).
func (c *Client) GetActorOwed(ctx context.Context, space, name string) (*ateapi.Actor, string, error) {
	var md metadata.MD
	a := &ateapi.Actor{}
	err := c.Raw(ctx, "GetActor", &ateapi.GetActorRequest{Actor: &ateapi.ObjectRef{Atespace: space, Name: name}}, a, grpc.Trailer(&md))
	owed := ""
	if v := md.Get(apiserver.OwedHeader); len(v) > 0 {
		owed = v[0]
	}
	if err != nil {
		return nil, owed, err
	}
	return a, owed, nil
}

func (c *Client) CreateActor(ctx context.Context, r *ateapi.CreateActorRequest) (*ateapi.Actor, error) {
	return call(ctx, c, "CreateActor", r, &ateapi.Actor{})
}
func (c *Client) GetActor(ctx context.Context, r *ateapi.GetActorRequest) (*ateapi.Actor, error) {
	return call(ctx, c, "GetActor", r, &ateapi.Actor{})
}
func (c *Client) ListActors(ctx context.Context, r *ateapi.ListActorsRequest) (*ateapi.ListActorsResponse, error) {
	return call(ctx, c, "ListActors", r, &ateapi.ListActorsResponse{})
}
func (c *Client) UpdateActor(ctx context.Context, r *ateapi.UpdateActorRequest) (*ateapi.Actor, error) {
	return call(ctx, c, "UpdateActor", r, &ateapi.Actor{})
}
func (c *Client) DeleteActor(ctx context.Context, r *ateapi.DeleteActorRequest) (*ateapi.Actor, error) {
	return call(ctx, c, "DeleteActor", r, &ateapi.Actor{})
}
func (c *Client) SuspendActor(ctx context.Context, r *ateapi.SuspendActorRequest) (*ateapi.SuspendActorResponse, error) {
	return call(ctx, c, "SuspendActor", r, &ateapi.SuspendActorResponse{})
}
func (c *Client) ResumeActor(ctx context.Context, r *ateapi.ResumeActorRequest) (*ateapi.ResumeActorResponse, error) {
	return call(ctx, c, "ResumeActor", r, &ateapi.ResumeActorResponse{})
}
func (c *Client) CreateTag(ctx context.Context, r *ateapi.CreateTagRequest) (*ateapi.Tag, error) {
	return call(ctx, c, "CreateTag", r, &ateapi.Tag{})
}
func (c *Client) GetTag(ctx context.Context, r *ateapi.GetTagRequest) (*ateapi.Tag, error) {
	return call(ctx, c, "GetTag", r, &ateapi.Tag{})
}
func (c *Client) DeleteTag(ctx context.Context, r *ateapi.DeleteTagRequest) (*ateapi.Tag, error) {
	return call(ctx, c, "DeleteTag", r, &ateapi.Tag{})
}
func (c *Client) CreateActorTemplate(ctx context.Context, r *ateapi.CreateActorTemplateRequest) (*ateapi.ActorTemplate, error) {
	return call(ctx, c, "CreateActorTemplate", r, &ateapi.ActorTemplate{})
}
func (c *Client) GetActorTemplate(ctx context.Context, r *ateapi.GetActorTemplateRequest) (*ateapi.ActorTemplate, error) {
	return call(ctx, c, "GetActorTemplate", r, &ateapi.ActorTemplate{})
}
func (c *Client) ListActorTemplates(ctx context.Context, r *ateapi.ListActorTemplatesRequest) (*ateapi.ListActorTemplatesResponse, error) {
	return call(ctx, c, "ListActorTemplates", r, &ateapi.ListActorTemplatesResponse{})
}
func (c *Client) DeleteActorTemplate(ctx context.Context, r *ateapi.DeleteActorTemplateRequest) (*ateapi.ActorTemplate, error) {
	return call(ctx, c, "DeleteActorTemplate", r, &ateapi.ActorTemplate{})
}
func (c *Client) GetActorEgressPolicy(ctx context.Context, r *ateapi.GetActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	return call(ctx, c, "GetActorEgressPolicy", r, &ateapi.EgressPolicy{})
}
func (c *Client) CreateActorEgressPolicy(ctx context.Context, r *ateapi.CreateActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	return call(ctx, c, "CreateActorEgressPolicy", r, &ateapi.EgressPolicy{})
}
func (c *Client) UpdateActorEgressPolicy(ctx context.Context, r *ateapi.UpdateActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	return call(ctx, c, "UpdateActorEgressPolicy", r, &ateapi.EgressPolicy{})
}
func (c *Client) DeleteActorEgressPolicy(ctx context.Context, r *ateapi.DeleteActorEgressPolicyRequest) (*ateapi.EgressPolicy, error) {
	return call(ctx, c, "DeleteActorEgressPolicy", r, &ateapi.EgressPolicy{})
}
func (c *Client) ListTags(ctx context.Context, r *ateapi.ListTagsRequest) (*ateapi.ListTagsResponse, error) {
	return call(ctx, c, "ListTags", r, &ateapi.ListTagsResponse{})
}
