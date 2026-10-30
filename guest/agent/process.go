package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	"github.com/everdict/herd/guest/execrec"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Defaults, as agent-substrate/env's guest applies them.
const (
	DefaultProcessTimeout = time.Hour
	// PollInterval is how often a follower looks at the record for new output or an exit.
	PollInterval = 20 * time.Millisecond
	// StartWait bounds how long StartProcess waits for the shim to report the process group.
	StartWait = 10 * time.Second
)

type container struct {
	t     Target
	store *execrec.Store
	root  *os.Root
	// vols are the target's volumes, longest mount path first.
	vols []volume
}

// volume is a directory bound into the container at path: what lies under path is the volume's, not the rootfs's.
type volume struct {
	path string
	root *os.Root
}

// Guest serves the API for a set of targets.
type Guest struct {
	containers []*container
	byName     map[string]*container

	mu     sync.Mutex
	timers map[string]*time.Timer
	closed bool
}

// New opens the targets' records and re-arms the timeout of every process still running, so a restarted guest
// enforces the deadlines the previous one set.
func New(targets ...Target) (*Guest, error) {
	if len(targets) == 0 {
		return nil, errors.New("agent: no target container")
	}
	g := &Guest{byName: map[string]*container{}, timers: map[string]*time.Timer{}}
	for _, t := range targets {
		if _, dup := g.byName[t.Name()]; dup {
			g.Close()
			return nil, fmt.Errorf("agent: container %q twice", t.Name())
		}
		st, err := execrec.OpenStore(t.Rootfs())
		if err != nil {
			g.Close()
			return nil, err
		}
		root, err := os.OpenRoot(t.Rootfs())
		if err != nil {
			st.Close()
			g.Close()
			return nil, fmt.Errorf("agent: %w", err)
		}
		c := &container{t: t, store: st, root: root}
		g.containers = append(g.containers, c)
		if err := c.openVolumes(); err != nil {
			g.Close()
			return nil, err
		}
		g.byName[t.Name()] = c
	}
	for _, c := range g.containers {
		ids, err := c.store.List()
		if err != nil {
			g.Close()
			return nil, err
		}
		for _, id := range ids {
			if st, err := c.store.Read(id); err == nil && !st.Exited {
				g.armTimeout(c, st.Meta)
			}
		}
	}
	return g, nil
}

// Close stops the timeout timers and releases the stores. Running processes are not touched.
func (g *Guest) Close() {
	g.mu.Lock()
	g.closed = true
	for _, t := range g.timers {
		t.Stop()
	}
	g.mu.Unlock()
	for _, c := range g.containers {
		c.store.Close()
		c.root.Close()
		for _, v := range c.vols {
			v.root.Close()
		}
	}
}

func (g *Guest) target(md func(string) string) (*container, error) {
	name := md(ContainerHeader)
	if name == "" {
		return g.containers[0], nil
	}
	c, ok := g.byName[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "container %q not found", name)
	}
	return c, nil
}

// lookup finds a process by id in any container: ids are unique across the guest.
func (g *Guest) lookup(id string) (*container, execrec.State, error) {
	if id == "" {
		return nil, execrec.State{}, status.Errorf(codes.InvalidArgument, "process_id cannot be empty")
	}
	for _, c := range g.containers {
		if !c.store.Exists(id) {
			continue
		}
		st, err := c.store.Read(id)
		if err != nil {
			return nil, execrec.State{}, status.Errorf(codes.Internal, "%v", err)
		}
		return c, st, nil
	}
	return nil, execrec.State{}, status.Errorf(codes.NotFound, "process %q not found", id)
}

func newID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func toProto(st execrec.State) *ateenv.Process {
	p := &ateenv.Process{
		ProcessId: st.Meta.ID,
		Command:   append([]string(nil), st.Meta.Command...),
		Pid:       int32(st.Pgid),
		State:     ateenv.ProcessState_PROCESS_STATE_RUNNING,
		StartedAt: timestamppb.New(st.Meta.StartedAt),
	}
	if st.Exited {
		p.State = ateenv.ProcessState_PROCESS_STATE_EXITED
		p.ExitCode = int32(st.ExitCode)
		p.FinishedAt = timestamppb.New(st.FinishedAt)
	}
	return p
}

// StartProcess writes the record, starts its shim in the target container and returns once the process group
// runs (or has already ended).
func (g *Guest) StartProcess(ctx context.Context, md func(string) string, req *ateenv.StartProcessRequest) (*ateenv.Process, error) {
	if len(req.GetCommand()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "command cannot be empty")
	}
	if req.GetTimeout() != nil && !req.GetTimeout().IsValid() {
		return nil, status.Errorf(codes.InvalidArgument, "timeout is invalid")
	}
	c, err := g.target(md)
	if err != nil {
		return nil, err
	}
	for k := range req.GetEnv() {
		if !validEnvName(k) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid environment variable name %q", k)
		}
	}
	id, err := newID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	timeout := req.GetTimeout().AsDuration()
	if timeout <= 0 {
		timeout = DefaultProcessTimeout
	}
	cwd := req.GetCwd()
	if cwd == "" {
		cwd = c.t.Workdir()
	}
	if pm, ok := c.t.(interface{ ProcessPath(string) string }); ok && cwd != "" {
		cwd = pm.ProcessPath(cwd)
	}
	m := execrec.Meta{
		ID: id, Command: req.GetCommand(), Cwd: cwd, Env: req.GetEnv(), Stdin: req.GetStdin(),
		Timeout: timeout, StartedAt: time.Now().UTC(),
	}
	if err := c.store.Create(m); err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if err := c.t.StartShim(ctx, execrec.Dir+"/"+id); err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	g.armTimeout(c, m)
	deadline := time.Now().Add(StartWait)
	for {
		st, err := c.store.Read(id)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "%v", err)
		}
		if st.Pgid != 0 || st.Exited {
			return toProto(st), nil
		}
		if time.Now().After(deadline) {
			return nil, status.Errorf(codes.DeadlineExceeded, "process %q: the shim did not report a process group within %v", id, StartWait)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(PollInterval / 4):
		}
	}
}

func validEnvName(k string) bool {
	if k == "" {
		return false
	}
	for i, c := range k {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// armTimeout kills the process group with SIGKILL when the process outlives its timeout, counted from its start.
func (g *Guest) armTimeout(c *container, m execrec.Meta) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	left := time.Until(m.StartedAt.Add(m.Timeout))
	g.timers[m.ID] = time.AfterFunc(left, func() {
		g.mu.Lock()
		delete(g.timers, m.ID)
		g.mu.Unlock()
		// The shim may not have reported the group yet when the timeout is tiny.
		for i := 0; i < int(StartWait/PollInterval); i++ {
			st, err := c.store.Read(m.ID)
			if err != nil || st.Exited {
				return
			}
			if st.Pgid != 0 {
				_ = c.t.Signal(context.Background(), st.Pgid, syscall.SIGKILL)
				return
			}
			time.Sleep(PollInterval)
		}
	})
}

// GetProcess returns what the record says.
func (g *Guest) GetProcess(_ context.Context, req *ateenv.GetProcessRequest) (*ateenv.Process, error) {
	_, st, err := g.lookup(req.GetProcessId())
	if err != nil {
		return nil, err
	}
	return toProto(st), nil
}

// StreamProcessOutput sends out and err from the requested byte offsets. The exit message comes only after the
// record's exit is written and the output up to it is sent; exit is written after the process ended, so reading it
// before the last flush means no output is left behind.
func (g *Guest) StreamProcessOutput(ctx context.Context, req *ateenv.StreamProcessOutputRequest, send func(*ateenv.ProcessOutput) error) error {
	c, _, err := g.lookup(req.GetProcessId())
	if err != nil {
		return err
	}
	if req.GetStdoutOffset() < 0 || req.GetStderrOffset() < 0 {
		return status.Errorf(codes.InvalidArgument, "offsets cannot be negative")
	}
	id := req.GetProcessId()
	offOut, offErr := req.GetStdoutOffset(), req.GetStderrOffset()
	flush := func() error {
		for {
			moved := false
			for _, s := range []struct {
				name string
				off  *int64
				wrap func([]byte) *ateenv.ProcessOutput
			}{
				{execrec.FileOut, &offOut, func(b []byte) *ateenv.ProcessOutput {
					return &ateenv.ProcessOutput{Output: &ateenv.ProcessOutput_Stdout{Stdout: b}}
				}},
				{execrec.FileErr, &offErr, func(b []byte) *ateenv.ProcessOutput {
					return &ateenv.ProcessOutput{Output: &ateenv.ProcessOutput_Stderr{Stderr: b}}
				}},
			} {
				b, next, err := c.store.ReadOutput(id, s.name, *s.off)
				if err != nil {
					return status.Errorf(codes.Internal, "reading %s: %v", s.name, err)
				}
				if len(b) == 0 {
					continue
				}
				if err := send(s.wrap(b)); err != nil {
					return err
				}
				*s.off, moved = next, true
			}
			if !moved {
				return nil
			}
		}
	}
	tick := time.NewTicker(PollInterval)
	defer tick.Stop()
	for {
		st, err := c.store.Read(id)
		if err != nil {
			return status.Errorf(codes.Internal, "%v", err)
		}
		if err := flush(); err != nil {
			return err
		}
		if st.Exited {
			return send(&ateenv.ProcessOutput{Output: &ateenv.ProcessOutput_Exit{Exit: toProto(st)}})
		}
		if !req.GetFollow() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// WriteProcessInput feeds stdin from a stream of requests; recv returns io.EOF at the end of the client stream.
func (g *Guest) WriteProcessInput(ctx context.Context, recv func() (*ateenv.WriteProcessInputRequest, error)) (*ateenv.WriteProcessInputResponse, error) {
	var (
		c     *container
		id    string
		in    *os.File
		total int64
	)
	defer func() {
		if in != nil {
			in.Close()
		}
	}()
	for {
		req, err := recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if c == nil {
			id = req.GetProcessId()
			if id == "" {
				return nil, status.Errorf(codes.InvalidArgument, "process_id is required on the first message")
			}
			var st execrec.State
			if c, st, err = g.lookup(id); err != nil {
				return nil, err
			}
			if !st.Meta.Stdin {
				return nil, status.Errorf(codes.FailedPrecondition, "process %q was started without stdin", id)
			}
		}
		if len(req.GetData()) > 0 {
			if in == nil {
				if in, err = c.store.OpenInput(ctx, id); err != nil {
					return nil, inputError(err, id)
				}
			}
			n, err := in.Write(req.GetData())
			total += int64(n)
			if err != nil {
				if errors.Is(err, syscall.EPIPE) {
					return nil, status.Errorf(codes.FailedPrecondition, "process %q has exited", id)
				}
				return nil, status.Errorf(codes.Internal, "writing stdin of %q: %v", id, err)
			}
		}
		if req.GetClose() {
			if in != nil {
				in.Close()
				in = nil
			}
			if err := c.store.CloseInput(ctx, id); err != nil {
				return nil, inputError(err, id)
			}
		}
	}
	if c == nil {
		return nil, status.Errorf(codes.InvalidArgument, "process_id is required on the first message")
	}
	return &ateenv.WriteProcessInputResponse{BytesWritten: total}, nil
}

func inputError(err error, id string) error {
	switch {
	case errors.Is(err, execrec.ErrExited):
		return status.Errorf(codes.FailedPrecondition, "process %q has exited", id)
	case errors.Is(err, execrec.ErrStdinClosed):
		return status.Errorf(codes.FailedPrecondition, "stdin of process %q is closed", id)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return status.Errorf(codes.Internal, "stdin of %q: %v", id, err)
}

// SignalProcess delivers a signal to the process group and returns the state right after.
func (g *Guest) SignalProcess(ctx context.Context, req *ateenv.SignalProcessRequest) (*ateenv.Process, error) {
	c, st, err := g.lookup(req.GetProcessId())
	if err != nil {
		return nil, err
	}
	sig, ok := toSyscallSignal(req.GetSignal())
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported signal %v", req.GetSignal())
	}
	if st.Exited {
		return nil, status.Errorf(codes.FailedPrecondition, "process %q has exited", st.Meta.ID)
	}
	if st.Pgid == 0 {
		return nil, status.Errorf(codes.Unavailable, "process %q has not reported its process group yet", st.Meta.ID)
	}
	if err := c.t.Signal(ctx, st.Pgid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil, status.Errorf(codes.FailedPrecondition, "process %q has exited", st.Meta.ID)
		}
		return nil, status.Errorf(codes.Internal, "signalling %q: %v", st.Meta.ID, err)
	}
	if st, err = c.store.Read(st.Meta.ID); err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return toProto(st), nil
}
