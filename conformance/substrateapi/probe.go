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

// ExecResult is what a command run in the actor left.
type ExecResult struct {
	Stdout, Stderr string
	ExitCode       int
}

// ExecProbe is a probe that can also run a command in the actor (the egress cases run busybox wget in the guest
// through it). A command that ran and failed is a result with its exit code, not an error.
type ExecProbe interface {
	Probe
	Exec(ctx context.Context, r Router, actor string, argv ...string) (ExecResult, error)
}

// ContainerProbe is a file probe that can address one named container of a multi-container actor, for a target
// whose guest picks the container by a request metadata key (header).
type ContainerProbe interface {
	FileProbe
	InContainer(header, container string) FileProbe
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

// Exec implements ExecProbe: the demo answers the command's exit code with its output.
func (SandboxProcess) Exec(ctx context.Context, r Router, actor string, argv ...string) (ExecResult, error) {
	body, err := json.Marshal(map[string]any{"command": argv})
	if err != nil {
		return ExecResult{}, err
	}
	b, err := r.do(ctx, http.MethodPost, "/process", actor, body)
	if err != nil {
		return ExecResult{}, err
	}
	var out struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exitCode"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return ExecResult{}, fmt.Errorf("POST /process: %w", err)
	}
	if out.Error != "" && out.ExitCode == 0 {
		return ExecResult{}, fmt.Errorf("POST /process %q: %s", argv, out.Error)
	}
	return ExecResult{Stdout: out.Stdout, Stderr: out.Stderr, ExitCode: out.ExitCode}, nil
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
type EnvExec struct {
	// Header and Container, if both set, send Header: Container as request metadata, for a target whose guest picks
	// the container a call acts on by a metadata key (env's API names none; ContainerProbe).
	Header, Container string
}

// InContainer implements ContainerProbe.
func (e EnvExec) InContainer(header, container string) FileProbe {
	e.Header, e.Container = header, container
	return e
}

func (EnvExec) Name() string { return "env-exec" }

const (
	envStartProcess  = "/ateenv.v1alpha.ProcessService/StartProcess"
	envStreamOutput  = "/ateenv.v1alpha.ProcessService/StreamProcessOutput"
	envReadFile      = "/ateenv.v1alpha.FileSystemService/ReadFile"
	envWriteFile     = "/ateenv.v1alpha.FileSystemService/WriteFile"
	maxEnvFileAnswer = 1 << 20
)

func (e EnvExec) conn(ctx context.Context, r Router, actor string) (*grpc.ClientConn, context.Context, error) {
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
	if e.Header != "" && e.Container != "" {
		md.Set(e.Header, e.Container)
	}
	return cc, metadata.NewOutgoingContext(ctx, md), nil
}

func (e EnvExec) Request(ctx context.Context, r Router, actor string) error {
	res, err := e.Exec(ctx, r, actor, "/bin/sh", "-c", "echo conformance")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 || res.Stdout != "conformance\n" {
		return fmt.Errorf("exec: stdout %q, exit %d; want %q and exit 0", res.Stdout, res.ExitCode, "conformance\n")
	}
	return nil
}

// Exec implements ExecProbe: StartProcess, then StreamProcessOutput followed to the exit.
func (e EnvExec) Exec(ctx context.Context, r Router, actor string, argv ...string) (ExecResult, error) {
	cc, ctx, err := e.conn(ctx, r, actor)
	if err != nil {
		return ExecResult{}, err
	}
	defer cc.Close()
	p := &ateenv.Process{}
	if err := cc.Invoke(ctx, envStartProcess, &ateenv.StartProcessRequest{Command: argv}, p); err != nil {
		return ExecResult{}, fmt.Errorf("StartProcess: %w", err)
	}
	var stdout, stderr strings.Builder
	exit, err := e.follow(ctx, cc, p.GetProcessId(), 0, func(m *ateenv.ProcessOutput) {
		stdout.Write(m.GetStdout())
		stderr.Write(m.GetStderr())
	})
	if err != nil {
		return ExecResult{}, err
	}
	if exit == nil {
		return ExecResult{}, fmt.Errorf("exec %q: the output stream ended without the exit (stdout %q)", argv, stdout.String())
	}
	return ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: int(exit.GetExitCode())}, nil
}

// follow streams process id's output from stdout offset, following it, and hands each message to each until the
// stream ends; it answers the exit if the stream carried it. A stream that ends with an error answers the error.
func (EnvExec) follow(ctx context.Context, cc *grpc.ClientConn, id string, offset int64, each func(*ateenv.ProcessOutput)) (*ateenv.Process, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cs, err := cc.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, envStreamOutput)
	if err != nil {
		return nil, fmt.Errorf("StreamProcessOutput: %w", err)
	}
	if err := cs.SendMsg(&ateenv.StreamProcessOutputRequest{ProcessId: id, StdoutOffset: offset, Follow: true}); err != nil {
		return nil, fmt.Errorf("StreamProcessOutput: %w", err)
	}
	_ = cs.CloseSend()
	var exit *ateenv.Process
	for {
		m := &ateenv.ProcessOutput{}
		if err := cs.RecvMsg(m); err != nil {
			if errors.Is(err, io.EOF) {
				return exit, nil
			}
			return exit, fmt.Errorf("StreamProcessOutput: %w", err)
		}
		each(m)
		if m.GetExit() != nil {
			exit = m.GetExit()
		}
	}
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
