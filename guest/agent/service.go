package agent

import (
	"context"

	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	herdext "github.com/everdict/herd/api/herdext/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Full method names, exactly as agent-substrate/env's generated gRPC code names them.
const (
	MethodStartProcess        = "/ateenv.v1alpha.ProcessService/StartProcess"
	MethodGetProcess          = "/ateenv.v1alpha.ProcessService/GetProcess"
	MethodStreamProcessOutput = "/ateenv.v1alpha.ProcessService/StreamProcessOutput"
	MethodWriteProcessInput   = "/ateenv.v1alpha.ProcessService/WriteProcessInput"
	MethodSignalProcess       = "/ateenv.v1alpha.ProcessService/SignalProcess"
	MethodReadFile            = "/ateenv.v1alpha.FileSystemService/ReadFile"
	MethodWriteFile           = "/ateenv.v1alpha.FileSystemService/WriteFile"
	MethodStatFile            = "/herd.ext.v1.FileInfoService/StatFile"
	MethodListDirectory       = "/herd.ext.v1.FileInfoService/ListDirectory"
)

// The service descriptors are written by hand (ADR 0001, A-12): protoc-gen-go-grpc is not available where herd is
// built. TestServesEveryMethodUnderItsWireName checks them against the protos' service descriptors.

// mdOf reads the call's request metadata (first value per key).
func mdOf(ctx context.Context) func(string) string {
	md, _ := metadata.FromIncomingContext(ctx)
	return func(k string) string {
		if v := md.Get(k); len(v) > 0 {
			return v[0]
		}
		return ""
	}
}

func unary[Req any, PReq interface{ *Req }, Resp any](svc, name string, h func(*Guest, context.Context, PReq) (Resp, error)) grpc.MethodDesc {
	return grpc.MethodDesc{MethodName: name, Handler: func(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
		in := PReq(new(Req))
		if err := dec(in); err != nil {
			return nil, err
		}
		g := srv.(*Guest)
		if ic == nil {
			return h(g, ctx, in)
		}
		info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + svc + "/" + name}
		return ic(ctx, in, info, func(ctx context.Context, req any) (any, error) { return h(g, ctx, req.(PReq)) })
	}}
}

const (
	processService    = "ateenv.v1alpha.ProcessService"
	fileSystemService = "ateenv.v1alpha.FileSystemService"
	fileInfoService   = "herd.ext.v1.FileInfoService"
)

var processServiceDesc = grpc.ServiceDesc{
	ServiceName: processService,
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{
		unary(processService, "StartProcess", func(g *Guest, ctx context.Context, req *ateenv.StartProcessRequest) (*ateenv.Process, error) {
			return g.StartProcess(ctx, mdOf(ctx), req)
		}),
		unary(processService, "GetProcess", (*Guest).GetProcess),
		unary(processService, "SignalProcess", (*Guest).SignalProcess),
	},
	Streams: []grpc.StreamDesc{
		{StreamName: "StreamProcessOutput", ServerStreams: true, Handler: func(srv any, ss grpc.ServerStream) error {
			req := new(ateenv.StreamProcessOutputRequest)
			if err := ss.RecvMsg(req); err != nil {
				return err
			}
			return srv.(*Guest).StreamProcessOutput(ss.Context(), req, func(m *ateenv.ProcessOutput) error { return ss.SendMsg(m) })
		}},
		{StreamName: "WriteProcessInput", ClientStreams: true, Handler: func(srv any, ss grpc.ServerStream) error {
			resp, err := srv.(*Guest).WriteProcessInput(ss.Context(), func() (*ateenv.WriteProcessInputRequest, error) {
				m := new(ateenv.WriteProcessInputRequest)
				return m, ss.RecvMsg(m)
			})
			if err != nil {
				return err
			}
			return ss.SendMsg(resp)
		}},
	},
	Metadata: "ateenv/v1alpha/guest.proto",
}

var fileSystemServiceDesc = grpc.ServiceDesc{
	ServiceName: fileSystemService,
	HandlerType: (*any)(nil),
	Streams: []grpc.StreamDesc{
		{StreamName: "ReadFile", ServerStreams: true, Handler: func(srv any, ss grpc.ServerStream) error {
			req := new(ateenv.ReadFileRequest)
			if err := ss.RecvMsg(req); err != nil {
				return err
			}
			return srv.(*Guest).ReadFile(mdOf(ss.Context()), req, func(m *ateenv.ReadFileResponse) error { return ss.SendMsg(m) })
		}},
		{StreamName: "WriteFile", ClientStreams: true, Handler: func(srv any, ss grpc.ServerStream) error {
			resp, err := srv.(*Guest).WriteFile(mdOf(ss.Context()), func() (*ateenv.WriteFileRequest, error) {
				m := new(ateenv.WriteFileRequest)
				return m, ss.RecvMsg(m)
			})
			if err != nil {
				return err
			}
			return ss.SendMsg(resp)
		}},
	},
	Metadata: "ateenv/v1alpha/guest.proto",
}

var fileInfoServiceDesc = grpc.ServiceDesc{
	ServiceName: fileInfoService,
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{
		unary(fileInfoService, "StatFile", func(g *Guest, ctx context.Context, req *herdext.StatFileRequest) (*herdext.FileInfo, error) {
			return g.StatFile(mdOf(ctx), req)
		}),
		unary(fileInfoService, "ListDirectory", func(g *Guest, ctx context.Context, req *herdext.ListDirectoryRequest) (*herdext.ListDirectoryResponse, error) {
			return g.ListDirectory(mdOf(ctx), req)
		}),
	},
	Metadata: "herdext/v1/files.proto",
}

// Register adds the guest's services to srv.
func (g *Guest) Register(srv grpc.ServiceRegistrar) {
	srv.RegisterService(&processServiceDesc, g)
	srv.RegisterService(&fileSystemServiceDesc, g)
	srv.RegisterService(&fileInfoServiceDesc, g)
}
