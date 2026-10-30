package agent

import (
	"context"

	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/internal/grpcwire"
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

// Register adds the guest's methods to srv.
func (g *Guest) Register(srv *grpcwire.Server) {
	srv.Handle(MethodStartProcess, func(s *grpcwire.ServerStream) error {
		return grpcwire.Unary(func(ctx context.Context, req *ateenv.StartProcessRequest) (*ateenv.Process, error) {
			return g.StartProcess(ctx, s.Metadata, req)
		})(s)
	})
	srv.Handle(MethodGetProcess, grpcwire.Unary(g.GetProcess))
	srv.Handle(MethodSignalProcess, grpcwire.Unary(g.SignalProcess))
	srv.Handle(MethodStreamProcessOutput, func(s *grpcwire.ServerStream) error {
		req := new(ateenv.StreamProcessOutputRequest)
		if err := s.Recv(req); err != nil {
			return err
		}
		return g.StreamProcessOutput(s.Context(), req, func(m *ateenv.ProcessOutput) error { return s.Send(m) })
	})
	srv.Handle(MethodWriteProcessInput, func(s *grpcwire.ServerStream) error {
		resp, err := g.WriteProcessInput(s.Context(), func() (*ateenv.WriteProcessInputRequest, error) {
			m := new(ateenv.WriteProcessInputRequest)
			return m, s.Recv(m)
		})
		if err != nil {
			return err
		}
		return s.Send(resp)
	})
	srv.Handle(MethodReadFile, func(s *grpcwire.ServerStream) error {
		req := new(ateenv.ReadFileRequest)
		if err := s.Recv(req); err != nil {
			return err
		}
		return g.ReadFile(s.Metadata, req, func(m *ateenv.ReadFileResponse) error { return s.Send(m) })
	})
	srv.Handle(MethodWriteFile, func(s *grpcwire.ServerStream) error {
		resp, err := g.WriteFile(s.Metadata, func() (*ateenv.WriteFileRequest, error) {
			m := new(ateenv.WriteFileRequest)
			return m, s.Recv(m)
		})
		if err != nil {
			return err
		}
		return s.Send(resp)
	})
	srv.Handle(MethodStatFile, func(s *grpcwire.ServerStream) error {
		return grpcwire.Unary(func(_ context.Context, req *herdext.StatFileRequest) (*herdext.FileInfo, error) {
			return g.StatFile(s.Metadata, req)
		})(s)
	})
	srv.Handle(MethodListDirectory, func(s *grpcwire.ServerStream) error {
		return grpcwire.Unary(func(_ context.Context, req *herdext.ListDirectoryRequest) (*herdext.ListDirectoryResponse, error) {
			return g.ListDirectory(s.Metadata, req)
		})(s)
	})
}
