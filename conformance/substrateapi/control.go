// Package substrateapi is a client-side conformance suite for Substrate's Control API (api/ateapi/v1), the one
// runtime integration Everdict keeps for real Substrate and for herd (spec R8, R-D2). It drives only the pinned
// Control subset over gRPC and the actor's own traffic through the target's router (Substrate's atenet-router or
// herd-router, both addressed by the ate-target-actor header); it reads no herd extension and no store, so the same
// run is meaningful against either. `herd conformance` is its command line.
//
// A case skips only when the server declared why: it answered the call codes.Unimplemented, and its status message
// is the reason. Every other refusal fails the case.
package substrateapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

// Endpoint is how to reach a gRPC or HTTP server of the target.
type Endpoint struct {
	// Addr is host:port.
	Addr string
	// TLS dials with TLS (system roots, or CAFile).
	TLS    bool
	CAFile string
	// ServerName is the name the server's certificate is checked for (a port-forward dials 127.0.0.1, but
	// Substrate's certificates name the Service, e.g. api.ate-system.svc).
	ServerName string
	// CertFile and KeyFile, if set, are the client certificate presented (atenet-router requires one).
	CertFile, KeyFile string
}

func (e Endpoint) transport() (grpc.DialOption, error) {
	if !e.TLS {
		return grpc.WithTransportCredentials(insecure.NewCredentials()), nil
	}
	cfg, err := e.tlsConfig()
	if err != nil {
		return nil, err
	}
	return grpc.WithTransportCredentials(credentials.NewTLS(cfg)), nil
}

func (e Endpoint) tlsConfig() (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: e.ServerName}
	if e.CertFile != "" || e.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(e.CertFile, e.KeyFile)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	if e.CAFile != "" {
		pem, err := os.ReadFile(e.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no certificate", e.CAFile)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// bearer is a per-call bearer token.
type bearer struct {
	token  string
	secure bool
}

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	if b.token == "" {
		return nil, nil
	}
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b bearer) RequireTransportSecurity() bool { return b.secure }

// Control is a client of the Control subset, by full method name only.
type Control struct{ cc *grpc.ClientConn }

const controlService = "/substrate.ateapi.v1.Control/"

// DialControl connects to the Control service at e with token.
func DialControl(e Endpoint, token string) (*Control, error) {
	if e.Addr == "" {
		return nil, errors.New("substrateapi: no Control endpoint")
	}
	tr, err := e.transport()
	if err != nil {
		return nil, err
	}
	cc, err := grpc.NewClient("passthrough:///"+e.Addr, tr, grpc.WithPerRPCCredentials(bearer{token: token, secure: e.TLS}))
	if err != nil {
		return nil, fmt.Errorf("substrateapi: %w", err)
	}
	return &Control{cc: cc}, nil
}

// Close closes the connection.
func (c *Control) Close() error { return c.cc.Close() }

// Conn is the connection, for a caller that asks the target more than the suite does (herd conformance).
func (c *Control) Conn() grpc.ClientConnInterface { return c.cc }

func invoke[R proto.Message](ctx context.Context, c *Control, method string, req proto.Message, resp R) (R, error) {
	if err := c.cc.Invoke(ctx, controlService+method, req, resp); err != nil {
		var zero R
		return zero, err
	}
	return resp, nil
}

func (c *Control) GetActorTemplate(ctx context.Context, space, name string) (*ateapi.ActorTemplate, error) {
	return invoke(ctx, c, "GetActorTemplate", &ateapi.GetActorTemplateRequest{ActorTemplate: ref(space, name)}, &ateapi.ActorTemplate{})
}

func (c *Control) CreateActorTemplate(ctx context.Context, t *ateapi.ActorTemplate) (*ateapi.ActorTemplate, error) {
	return invoke(ctx, c, "CreateActorTemplate", &ateapi.CreateActorTemplateRequest{ActorTemplate: t}, &ateapi.ActorTemplate{})
}

func (c *Control) DeleteActorTemplate(ctx context.Context, space, name string) (*ateapi.ActorTemplate, error) {
	return invoke(ctx, c, "DeleteActorTemplate", &ateapi.DeleteActorTemplateRequest{ActorTemplate: ref(space, name)}, &ateapi.ActorTemplate{})
}

func (c *Control) CreateActor(ctx context.Context, a *ateapi.Actor) (*ateapi.Actor, error) {
	return invoke(ctx, c, "CreateActor", &ateapi.CreateActorRequest{Actor: a}, &ateapi.Actor{})
}

func (c *Control) GetActor(ctx context.Context, space, name string) (*ateapi.Actor, error) {
	return invoke(ctx, c, "GetActor", &ateapi.GetActorRequest{Actor: ref(space, name)}, &ateapi.Actor{})
}

func (c *Control) ListActors(ctx context.Context, space string) (*ateapi.ListActorsResponse, error) {
	return invoke(ctx, c, "ListActors", &ateapi.ListActorsRequest{Atespace: space}, &ateapi.ListActorsResponse{})
}

func (c *Control) UpdateActor(ctx context.Context, a *ateapi.Actor) (*ateapi.Actor, error) {
	return invoke(ctx, c, "UpdateActor", &ateapi.UpdateActorRequest{Actor: a}, &ateapi.Actor{})
}

// SuspendActor always names its durability mode: Substrate defaults an unset one to ASYNCHRONOUS.
func (c *Control) SuspendActor(ctx context.Context, space, name string, mode ateapi.DurabilityMode) (*ateapi.SuspendActorResponse, error) {
	return invoke(ctx, c, "SuspendActor", &ateapi.SuspendActorRequest{Actor: ref(space, name), Options: &ateapi.SuspendOptions{Mode: mode}},
		&ateapi.SuspendActorResponse{})
}

func (c *Control) ResumeActor(ctx context.Context, space, name string) (*ateapi.ResumeActorResponse, error) {
	return invoke(ctx, c, "ResumeActor", &ateapi.ResumeActorRequest{Actor: ref(space, name)}, &ateapi.ResumeActorResponse{})
}

func (c *Control) DeleteActor(ctx context.Context, space, name string) (*ateapi.Actor, error) {
	return invoke(ctx, c, "DeleteActor", &ateapi.DeleteActorRequest{Actor: ref(space, name)}, &ateapi.Actor{})
}

func (c *Control) CreateTag(ctx context.Context, t *ateapi.Tag) (*ateapi.Tag, error) {
	return invoke(ctx, c, "CreateTag", &ateapi.CreateTagRequest{Tag: t}, &ateapi.Tag{})
}

func (c *Control) GetTag(ctx context.Context, space, name string) (*ateapi.Tag, error) {
	return invoke(ctx, c, "GetTag", &ateapi.GetTagRequest{Tag: ref(space, name)}, &ateapi.Tag{})
}

func (c *Control) DeleteTag(ctx context.Context, space, name string) (*ateapi.Tag, error) {
	return invoke(ctx, c, "DeleteTag", &ateapi.DeleteTagRequest{Tag: ref(space, name)}, &ateapi.Tag{})
}

func ref(space, name string) *ateapi.ObjectRef { return &ateapi.ObjectRef{Atespace: space, Name: name} }

func (c *Control) CreateActorEgressPolicy(ctx context.Context, space, actor string, p *ateapi.EgressPolicy) (*ateapi.EgressPolicy, error) {
	return invoke(ctx, c, "CreateActorEgressPolicy", &ateapi.CreateActorEgressPolicyRequest{Actor: ref(space, actor), EgressPolicy: p}, &ateapi.EgressPolicy{})
}

func (c *Control) UpdateActorEgressPolicy(ctx context.Context, space, actor string, p *ateapi.EgressPolicy) (*ateapi.EgressPolicy, error) {
	return invoke(ctx, c, "UpdateActorEgressPolicy", &ateapi.UpdateActorEgressPolicyRequest{Actor: ref(space, actor), EgressPolicy: p}, &ateapi.EgressPolicy{})
}

func (c *Control) ListWorkers(ctx context.Context, opts *ateapi.ListOptions) (*ateapi.ListWorkersResponse, error) {
	return invoke(ctx, c, "ListWorkers", &ateapi.ListWorkersRequest{Options: opts}, &ateapi.ListWorkersResponse{})
}

func (c *Control) ListActorTemplates(ctx context.Context, space string, opts *ateapi.ListOptions) (*ateapi.ListActorTemplatesResponse, error) {
	return invoke(ctx, c, "ListActorTemplates", &ateapi.ListActorTemplatesRequest{Atespace: space, Options: opts}, &ateapi.ListActorTemplatesResponse{})
}

func (c *Control) ListTags(ctx context.Context, space string, opts *ateapi.ListOptions) (*ateapi.ListTagsResponse, error) {
	return invoke(ctx, c, "ListTags", &ateapi.ListTagsRequest{Atespace: space, Options: opts}, &ateapi.ListTagsResponse{})
}
