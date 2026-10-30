// interop drives a running herd-guest with grpc-go v1.84.0 and agent-substrate/env's own generated client
// (proto/ateenv/v1alpha/guest.pb.go and guest_grpc.pb.go at env@281e8608, unmodified). Exit 0 = every check passed.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	env "github.com/everdict/herd/test/interop/envclient"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

var failed int
var passed int

func check(name string, ok bool, detail string) {
	if ok {
		passed++
		fmt.Printf("PASS %s\n", name)
		return
	}
	failed++
	fmt.Printf("FAIL %s: %s\n", name, detail)
}

func follow(ctx context.Context, pc env.ProcessServiceClient, id string) (string, string, *env.Process, error) {
	s, err := pc.StreamProcessOutput(ctx, &env.StreamProcessOutputRequest{ProcessId: id, Follow: true})
	if err != nil {
		return "", "", nil, err
	}
	var out, errb strings.Builder
	var exit *env.Process
	for {
		m, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return out.String(), errb.String(), exit, nil
		}
		if err != nil {
			return out.String(), errb.String(), exit, err
		}
		out.Write(m.GetStdout())
		errb.Write(m.GetStderr())
		if m.GetExit() != nil {
			exit = m.GetExit()
		}
	}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cc, err := grpc.NewClient("unix://"+os.Args[1], grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Println("FAIL dial:", err)
		os.Exit(1)
	}
	defer cc.Close()
	pc, fc := env.NewProcessServiceClient(cc), env.NewFileSystemServiceClient(cc)

	p, err := pc.StartProcess(ctx, &env.StartProcessRequest{Command: []string{"sh", "-c", `read x; echo "got $x"; echo oops >&2; exit 3`}, Stdin: true})
	check("StartProcess", err == nil && p.GetProcessId() != "" && p.GetPid() > 0, fmt.Sprint(p, err))
	if err != nil {
		os.Exit(1)
	}
	w, err := pc.WriteProcessInput(ctx)
	if err == nil {
		err = w.Send(&env.WriteProcessInputRequest{ProcessId: p.GetProcessId(), Data: []byte("hello\n")})
	}
	if err == nil {
		err = w.Send(&env.WriteProcessInputRequest{Close: true})
	}
	var wr *env.WriteProcessInputResponse
	if err == nil {
		wr, err = w.CloseAndRecv()
	}
	check("WriteProcessInput (client stream)", err == nil && wr.GetBytesWritten() == 6, fmt.Sprint(wr, err))
	out, errs, exit, err := follow(ctx, pc, p.GetProcessId())
	check("StreamProcessOutput stdout", err == nil && out == "got hello\n", fmt.Sprintf("%q %v", out, err))
	check("StreamProcessOutput stderr", errs == "oops\n", fmt.Sprintf("%q", errs))
	check("StreamProcessOutput exit", exit.GetState() == env.ProcessState_PROCESS_STATE_EXITED && exit.GetExitCode() == 3, fmt.Sprint(exit))
	g, err := pc.GetProcess(ctx, &env.GetProcessRequest{ProcessId: p.GetProcessId()})
	check("GetProcess", err == nil && g.GetExitCode() == 3 && g.GetFinishedAt() != nil, fmt.Sprint(g, err))

	q, err := pc.StartProcess(ctx, &env.StartProcessRequest{Command: []string{"sleep", "30"}})
	if err == nil {
		_, err = pc.SignalProcess(ctx, &env.SignalProcessRequest{ProcessId: q.GetProcessId(), Signal: env.Signal_SIGNAL_INT})
	}
	_, _, exit, err2 := follow(ctx, pc, q.GetProcessId())
	check("SignalProcess SIGINT -> 130", err == nil && err2 == nil && exit.GetExitCode() == 130, fmt.Sprint(exit, err, err2))

	_, err = pc.GetProcess(ctx, &env.GetProcessRequest{ProcessId: "missing"})
	check("NotFound status code", status.Code(err) == codes.NotFound, fmt.Sprint(err))
	_, err = pc.StartProcess(ctx, &env.StartProcessRequest{})
	check("InvalidArgument status code", status.Code(err) == codes.InvalidArgument, fmt.Sprint(err))

	wf, err := fc.WriteFile(ctx)
	if err == nil {
		err = wf.Send(&env.WriteFileRequest{Path: "/interop/a.txt", Mode: 0o644, Chunk: []byte("round ")})
	}
	if err == nil {
		err = wf.Send(&env.WriteFileRequest{Chunk: []byte("trip")})
	}
	var wfr *env.WriteFileResponse
	if err == nil {
		wfr, err = wf.CloseAndRecv()
	}
	check("WriteFile", err == nil && wfr.GetBytesWritten() == 10, fmt.Sprint(wfr, err))
	rf, err := fc.ReadFile(ctx, &env.ReadFileRequest{Path: "/interop/a.txt"})
	var got strings.Builder
	for err == nil {
		var m *env.ReadFileResponse
		m, err = rf.Recv()
		got.Write(m.GetChunk())
	}
	check("ReadFile", errors.Is(err, io.EOF) && got.String() == "round trip", fmt.Sprintf("%q %v", got.String(), err))
	rf, _ = fc.ReadFile(ctx, &env.ReadFileRequest{Path: "/nope"})
	_, err = rf.Recv()
	check("ReadFile NotFound", status.Code(err) == codes.NotFound, fmt.Sprint(err))

	fmt.Printf("interop: %d passed, %d failed\n", passed, failed)
	if failed > 0 {
		os.Exit(1)
	}
}
