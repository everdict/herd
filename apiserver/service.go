package apiserver

import (
	"context"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/link"
	"google.golang.org/grpc"
)

// The service descriptors are written by hand (ADR 0001, A-12): protoc-gen-go-grpc is not available where herd is
// built. TestServiceDescsMatchProto checks them against the protos' own service descriptors.

// ControlServer is the pinned Control subset (A-4).
type ControlServer interface {
	CreateActor(context.Context, *ateapi.CreateActorRequest) (*ateapi.Actor, error)
	GetActor(context.Context, *ateapi.GetActorRequest) (*ateapi.Actor, error)
	ListActors(context.Context, *ateapi.ListActorsRequest) (*ateapi.ListActorsResponse, error)
	ListWorkers(context.Context, *ateapi.ListWorkersRequest) (*ateapi.ListWorkersResponse, error)
	UpdateActor(context.Context, *ateapi.UpdateActorRequest) (*ateapi.Actor, error)
	DeleteActor(context.Context, *ateapi.DeleteActorRequest) (*ateapi.Actor, error)
	SuspendActor(context.Context, *ateapi.SuspendActorRequest) (*ateapi.SuspendActorResponse, error)
	ResumeActor(context.Context, *ateapi.ResumeActorRequest) (*ateapi.ResumeActorResponse, error)
	CreateTag(context.Context, *ateapi.CreateTagRequest) (*ateapi.Tag, error)
	GetTag(context.Context, *ateapi.GetTagRequest) (*ateapi.Tag, error)
	ListTags(context.Context, *ateapi.ListTagsRequest) (*ateapi.ListTagsResponse, error)
	DeleteTag(context.Context, *ateapi.DeleteTagRequest) (*ateapi.Tag, error)
	CreateActorTemplate(context.Context, *ateapi.CreateActorTemplateRequest) (*ateapi.ActorTemplate, error)
	GetActorTemplate(context.Context, *ateapi.GetActorTemplateRequest) (*ateapi.ActorTemplate, error)
	ListActorTemplates(context.Context, *ateapi.ListActorTemplatesRequest) (*ateapi.ListActorTemplatesResponse, error)
	DeleteActorTemplate(context.Context, *ateapi.DeleteActorTemplateRequest) (*ateapi.ActorTemplate, error)
	GetActorEgressPolicy(context.Context, *ateapi.GetActorEgressPolicyRequest) (*ateapi.EgressPolicy, error)
	CreateActorEgressPolicy(context.Context, *ateapi.CreateActorEgressPolicyRequest) (*ateapi.EgressPolicy, error)
	UpdateActorEgressPolicy(context.Context, *ateapi.UpdateActorEgressPolicyRequest) (*ateapi.EgressPolicy, error)
	DeleteActorEgressPolicy(context.Context, *ateapi.DeleteActorEgressPolicyRequest) (*ateapi.EgressPolicy, error)
}

// CapabilitiesServer is herd's capability extension (A-5).
type CapabilitiesServer interface {
	GetWorkerCapabilities(context.Context, *herdext.GetWorkerCapabilitiesRequest) (*herdext.WorkerCapabilities, error)
	ListWorkerCapabilities(context.Context, *herdext.ListWorkerCapabilitiesRequest) (*herdext.ListWorkerCapabilitiesResponse, error)
}

var (
	_ ControlServer      = (*Server)(nil)
	_ CapabilitiesServer = (*Server)(nil)
)

// unary adapts a method expression such as (*Server).GetActor to a grpc.MethodDesc of service svc.
func unary[S any, Req any, PReq interface{ *Req }, Resp any](svc, name string, h func(S, context.Context, PReq) (Resp, error)) grpc.MethodDesc {
	return grpc.MethodDesc{MethodName: name, Handler: func(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
		in := PReq(new(Req))
		if err := dec(in); err != nil {
			return nil, err
		}
		s := srv.(S)
		if ic == nil {
			return h(s, ctx, in)
		}
		info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + svc + "/" + name}
		return ic(ctx, in, info, func(ctx context.Context, req any) (any, error) { return h(s, ctx, req.(PReq)) })
	}}
}

const (
	controlService      = "substrate.ateapi.v1.Control"
	capabilitiesService = "herd.ext.v1.Capabilities"
)

// ControlServiceDesc is substrate.ateapi.v1.Control restricted to the pinned subset.
var ControlServiceDesc = grpc.ServiceDesc{
	ServiceName: controlService,
	HandlerType: (*ControlServer)(nil),
	Methods: []grpc.MethodDesc{
		unary(controlService, "GetActor", ControlServer.GetActor),
		unary(controlService, "CreateActor", ControlServer.CreateActor),
		unary(controlService, "UpdateActor", ControlServer.UpdateActor),
		unary(controlService, "SuspendActor", ControlServer.SuspendActor),
		unary(controlService, "ResumeActor", ControlServer.ResumeActor),
		unary(controlService, "DeleteActor", ControlServer.DeleteActor),
		unary(controlService, "GetActorEgressPolicy", ControlServer.GetActorEgressPolicy),
		unary(controlService, "CreateActorEgressPolicy", ControlServer.CreateActorEgressPolicy),
		unary(controlService, "UpdateActorEgressPolicy", ControlServer.UpdateActorEgressPolicy),
		unary(controlService, "DeleteActorEgressPolicy", ControlServer.DeleteActorEgressPolicy),
		unary(controlService, "CreateTag", ControlServer.CreateTag),
		unary(controlService, "GetTag", ControlServer.GetTag),
		unary(controlService, "ListTags", ControlServer.ListTags),
		unary(controlService, "DeleteTag", ControlServer.DeleteTag),
		unary(controlService, "ListActors", ControlServer.ListActors),
		unary(controlService, "ListWorkers", ControlServer.ListWorkers),
		unary(controlService, "CreateActorTemplate", ControlServer.CreateActorTemplate),
		unary(controlService, "GetActorTemplate", ControlServer.GetActorTemplate),
		unary(controlService, "ListActorTemplates", ControlServer.ListActorTemplates),
		unary(controlService, "DeleteActorTemplate", ControlServer.DeleteActorTemplate),
	},
	Metadata: "ateapi/v1/ateapi.proto",
}

// CapabilitiesServiceDesc is herd.ext.v1.Capabilities.
var CapabilitiesServiceDesc = grpc.ServiceDesc{
	ServiceName: capabilitiesService,
	HandlerType: (*CapabilitiesServer)(nil),
	Methods: []grpc.MethodDesc{
		unary(capabilitiesService, "GetWorkerCapabilities", CapabilitiesServer.GetWorkerCapabilities),
		unary(capabilitiesService, "ListWorkerCapabilities", CapabilitiesServer.ListWorkerCapabilities),
	},
	Metadata: "herdext/v1/capabilities.proto",
}

// linkServiceDesc carries the herdlet link (A-13); its messages are JSON (link.Codec).
var linkServiceDesc = grpc.ServiceDesc{
	ServiceName: link.ServiceName,
	HandlerType: (*any)(nil),
	Streams: []grpc.StreamDesc{{
		StreamName: link.StreamName, ServerStreams: true, ClientStreams: true,
		Handler: func(srv any, ss grpc.ServerStream) error { return srv.(*Server).serveLink(ss) },
	}},
}

// Register serves the Control subset, the Capabilities extension and the herdlet link on g.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	g.RegisterService(&ControlServiceDesc, s)
	g.RegisterService(&CapabilitiesServiceDesc, s)
	g.RegisterService(&linkServiceDesc, s)
}
