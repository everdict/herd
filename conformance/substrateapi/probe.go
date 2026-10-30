package substrateapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TargetActorHeader names the actor a router request is for, as "<atespace>/<actor>" (Substrate's atenet header;
// herd-router reads the same).
const TargetActorHeader = "ate-target-actor"

// Router is the target's actor ingress.
type Router struct {
	Endpoint
	// Token is sent as a bearer token (herd-router requires one; Substrate's demo ingress ignores it).
	Token string
}

// Probe is one request through the router to an actor of the template under test. What it sends depends on what
// the template's containers serve, so it is chosen per target (Config.Probe).
type Probe interface {
	// Name is how the probe is named on the command line.
	Name() string
	// Request sends one request to actor ("<atespace>/<name>") and checks the answer.
	Request(ctx context.Context, r Router, actor string) error
}

// FileProbe is a probe that can also write and read a file in the actor, so a case can check that a volume
// survived a suspend or a move.
type FileProbe interface {
	Probe
	WriteFile(ctx context.Context, r Router, actor, path, body string) error
	ReadFile(ctx context.Context, r Router, actor, path string) (string, error)
}

// ParseProbe reads a probe name: "http-get:<path>" (any HTTP server, e.g. Substrate's counter demo: http-get:/),
// "sandbox-process" (Substrate's sandbox demo, POST /process) or "env-exec" (agent-substrate/env's guest API over
// gRPC, which herd-guest serves on herd-router's default port).
func ParseProbe(s string) (Probe, error) {
	switch {
	case strings.HasPrefix(s, "http-get:"):
		p := strings.TrimPrefix(s, "http-get:")
		if !strings.HasPrefix(p, "/") {
			return nil, fmt.Errorf("probe %q: the path must start with /", s)
		}
		return HTTPGet{Path: p}, nil
	case s == "sandbox-process":
		return SandboxProcess{}, nil
	case s == "env-exec":
		return EnvExec{}, nil
	}
	return nil, fmt.Errorf("unknown probe %q (http-get:<path>, sandbox-process, env-exec)", s)
}

func (r Router) http() (*http.Client, string, error) {
	scheme := "http"
	tr := &http.Transport{}
	if r.TLS {
		cfg, err := r.tlsConfig()
		if err != nil {
			return nil, "", err
		}
		tr.TLSClientConfig, scheme = cfg, "https"
	}
	return &http.Client{Transport: tr}, scheme + "://" + r.Addr, nil
}

func (r Router) do(ctx context.Context, method, path, actor string, body []byte) ([]byte, error) {
	c, base, err := r.http()
	if err != nil {
		return nil, err
	}
	defer c.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set(TargetActorHeader, actor)
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return b, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// HTTPGet sends GET Path and wants a 2xx answer.
type HTTPGet struct{ Path string }

func (p HTTPGet) Name() string { return "http-get:" + p.Path }

func (p HTTPGet) Request(ctx context.Context, r Router, actor string) error {
	_, err := r.do(ctx, http.MethodGet, p.Path, actor, nil)
	return err
}

// SandboxProcess speaks Substrate's sandbox demo: POST /process {"command": [...]} answers the command's stdout.
type SandboxProcess struct{}

func (SandboxProcess) Name() string { return "sandbox-process" }

func (SandboxProcess) run(ctx context.Context, r Router, actor string, cmd ...string) (string, error) {
	body, err := json.Marshal(map[string]any{"command": cmd})
	if err != nil {
		return "", err
	}
	b, err := r.do(ctx, http.MethodPost, "/process", actor, body)
	if err != nil {
		return "", err
	}
	var out struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exitCode"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", fmt.Errorf("POST /process: %w", err)
	}
	if out.ExitCode != 0 || out.Error != "" {
		return out.Stdout, fmt.Errorf("POST /process %q: exit %d %s %s", cmd, out.ExitCode, out.Error, out.Stderr)
	}
	return out.Stdout, nil
}

func (p SandboxProcess) Request(ctx context.Context, r Router, actor string) error {
	out, err := p.run(ctx, r, actor, "sh", "-c", "echo conformance")
	if err == nil && out != "conformance\n" {
		err = fmt.Errorf("POST /process: stdout %q, want %q", out, "conformance\n")
	}
	return err
}

func (p SandboxProcess) WriteFile(ctx context.Context, r Router, actor, path, body string) error {
	_, err := p.run(ctx, r, actor, "sh", "-c", `mkdir -p "$(dirname "$1")" && printf %s "$2" > "$1"`, "sh", path, body)
	return err
}

func (p SandboxProcess) ReadFile(ctx context.Context, r Router, actor, path string) (string, error) {
	return p.run(ctx, r, actor, "cat", path)
}

// EnvExec speaks agent-substrate/env's guest API (ateenv.v1alpha) over gRPC through the router: it starts
// `echo conformance` and follows its output to the exit.
type EnvExec struct{}

func (EnvExec) Name() string { return "env-exec" }

const (
	envStartProcess  = "/ateenv.v1alpha.ProcessService/StartProcess"
	envStreamOutput  = "/ateenv.v1alpha.ProcessService/StreamProcessOutput"
	envReadFile      = "/ateenv.v1alpha.FileSystemService/ReadFile"
	envWriteFile     = "/ateenv.v1alpha.FileSystemService/WriteFile"
	maxEnvFileAnswer = 1 << 20
)

func (EnvExec) conn(ctx context.Context, r Router, actor string) (*grpc.ClientConn, context.Context, error) {
	tr, err := r.transport()
	if err != nil {
		return nil, nil, err
	}
	cc, err := grpc.NewClient("passthrough:///"+r.Addr, tr)
	if err != nil {
		return nil, nil, err
	}
	md := metadata.Pairs(TargetActorHeader, actor)
	if r.Token != "" {
		md.Set("authorization", "Bearer "+r.Token)
	}
	return cc, metadata.NewOutgoingContext(ctx, md), nil
}

func (e EnvExec) Request(ctx context.Context, r Router, actor string) error {
	cc, ctx, err := e.conn(ctx, r, actor)
	if err != nil {
		return err
	}
	defer cc.Close()
	p := &ateenv.Process{}
	if err := cc.Invoke(ctx, envStartProcess, &ateenv.StartProcessRequest{Command: []string{"/bin/sh", "-c", "echo conformance"}}, p); err != nil {
		return fmt.Errorf("StartProcess: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cs, err := cc.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, envStreamOutput)
	if err != nil {
		return fmt.Errorf("StreamProcessOutput: %w", err)
	}
	if err := cs.SendMsg(&ateenv.StreamProcessOutputRequest{ProcessId: p.GetProcessId(), Follow: true}); err != nil {
		return fmt.Errorf("StreamProcessOutput: %w", err)
	}
	_ = cs.CloseSend()
	var out strings.Builder
	var exit *ateenv.Process
	for {
		m := &ateenv.ProcessOutput{}
		if err := cs.RecvMsg(m); err != nil {
			if !errors.Is(err, io.EOF) {
				return fmt.Errorf("StreamProcessOutput: %w", err)
			}
			break
		}
		out.Write(m.GetStdout())
		if m.GetExit() != nil {
			exit = m.GetExit()
		}
	}
	if exit == nil || exit.GetExitCode() != 0 || out.String() != "conformance\n" {
		return fmt.Errorf("exec: stdout %q, exit %v; want %q and exit 0", out.String(), exit, "conformance\n")
	}
	return nil
}

func (e EnvExec) WriteFile(ctx context.Context, r Router, actor, path, body string) error {
	cc, ctx, err := e.conn(ctx, r, actor)
	if err != nil {
		return err
	}
	defer cc.Close()
	cs, err := cc.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true}, envWriteFile)
	if err != nil {
		return fmt.Errorf("WriteFile: %w", err)
	}
	if err := cs.SendMsg(&ateenv.WriteFileRequest{Path: path, Mode: 0o644, Chunk: []byte(body)}); err != nil {
		return fmt.Errorf("WriteFile: %w", err)
	}
	if err := cs.CloseSend(); err != nil {
		return fmt.Errorf("WriteFile: %w", err)
	}
	resp := &ateenv.WriteFileResponse{}
	if err := cs.RecvMsg(resp); err != nil {
		return fmt.Errorf("WriteFile: %w", err)
	}
	if resp.GetBytesWritten() != int64(len(body)) {
		return fmt.Errorf("WriteFile: %d bytes written, want %d", resp.GetBytesWritten(), len(body))
	}
	return nil
}

func (e EnvExec) ReadFile(ctx context.Context, r Router, actor, path string) (string, error) {
	cc, ctx, err := e.conn(ctx, r, actor)
	if err != nil {
		return "", err
	}
	defer cc.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cs, err := cc.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, envReadFile)
	if err != nil {
		return "", fmt.Errorf("ReadFile: %w", err)
	}
	if err := cs.SendMsg(&ateenv.ReadFileRequest{Path: path}); err != nil {
		return "", fmt.Errorf("ReadFile: %w", err)
	}
	_ = cs.CloseSend()
	var b strings.Builder
	for {
		m := &ateenv.ReadFileResponse{}
		if err := cs.RecvMsg(m); err != nil {
			if errors.Is(err, io.EOF) {
				return b.String(), nil
			}
			return "", fmt.Errorf("ReadFile: %w", err)
		}
		if b.Len()+len(m.GetChunk()) > maxEnvFileAnswer {
			return "", errors.New("ReadFile: answer too long")
		}
		b.Write(m.GetChunk())
	}
}

// RefusalProbe is a probe that can also provoke the actor's own typed refusal, to check that the router passes it
// through as the actor answered it.
type RefusalProbe interface {
	Probe
	Refusal(ctx context.Context, r Router, actor string) error
}

// Refusal reads a file that does not exist: env answers NotFound, and the client must see NotFound through the
// router, on a streaming call.
func (e EnvExec) Refusal(ctx context.Context, r Router, actor string) error {
	_, err := e.ReadFile(ctx, r, actor, "/conformance-no-such-file")
	if status.Code(err) != codes.NotFound {
		return fmt.Errorf("ReadFile of a missing file through the router: %v, want the guest's NotFound", err)
	}
	return nil
}
