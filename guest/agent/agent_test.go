package agent_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	herdext "github.com/everdict/herd/api/herdext/v1"
	"github.com/everdict/herd/guest/agent"
	"github.com/everdict/herd/guest/execrec"
	"github.com/everdict/herd/guest/guestclient"
	"github.com/everdict/herd/guest/transport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// The test binary doubles as the in-container shim, as herd-guest does with its `shim` mode.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "shim" {
		if err := execrec.Shim(os.Args[2]); err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// guestUnderTest is one herd-guest serving on a unix socket; restart replaces it with a fresh one on the same
// container roots, as a restarted guest process would be.
type guestUnderTest struct {
	t       *testing.T
	targets []agent.Target
	sock    string
	g       *agent.Guest
	srv     *grpc.Server
	done    chan error
	conn    *guestclient.Conn
}

func newTarget(t *testing.T, name string) *agent.Local {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &agent.Local{ContainerName: name, Root: root, Dir: "/workspace", Shim: []string{self, "shim"}}
}

func startGuest(t *testing.T, targets ...agent.Target) *guestUnderTest {
	t.Helper()
	sock := sockPath(t, "guest.sock")
	h := &guestUnderTest{t: t, targets: targets, sock: sock}
	h.start()
	t.Cleanup(h.stop)
	return h
}

func (h *guestUnderTest) start() {
	g, err := agent.New(h.targets...)
	if err != nil {
		h.t.Fatal(err)
	}
	l, err := transport.ListenUnix(h.sock)
	if err != nil {
		h.t.Fatal(err)
	}
	srv := grpc.NewServer()
	g.Register(srv)
	h.g, h.srv, h.done = g, srv, make(chan error, 1)
	go func() { h.done <- srv.Serve(l) }()
	h.conn = dialGuest(h.t, h.sock)
}

func (h *guestUnderTest) stop() {
	if h.srv == nil {
		return
	}
	h.conn.Close()
	h.srv.Stop()
	<-h.done
	h.g.Close()
	h.srv = nil
}

// restart stops the guest (cutting every open call) and starts a new one that knows nothing but the records.
func (h *guestUnderTest) restart() {
	h.stop()
	h.start()
}

func (h *guestUnderTest) start1(ctx context.Context, req *ateenv.StartProcessRequest) *ateenv.Process {
	h.t.Helper()
	p := new(ateenv.Process)
	if err := h.conn.Invoke(ctx, agent.MethodStartProcess, req, p, nil); err != nil {
		h.t.Fatalf("StartProcess: %v", err)
	}
	return p
}

type output struct {
	stdout, stderr bytes.Buffer
	exit           *ateenv.Process
}

// follow streams output from offsets until the exit message, or until stopAfter stdout bytes have arrived (then it
// abandons the stream, as a client that loses its connection does).
func (h *guestUnderTest) follow(ctx context.Context, id string, outOff, errOff int64, stopAfter int) (*output, error) {
	s, err := h.conn.NewStream(ctx, agent.MethodStreamProcessOutput, nil)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if err := s.Send(&ateenv.StreamProcessOutputRequest{ProcessId: id, StdoutOffset: outOff, StderrOffset: errOff, Follow: true}); err != nil {
		return nil, err
	}
	s.CloseSend()
	o := &output{}
	for {
		m := new(ateenv.ProcessOutput)
		err := s.Recv(m)
		if errors.Is(err, io.EOF) {
			return o, nil
		}
		if err != nil {
			return o, err
		}
		switch v := m.Output.(type) {
		case *ateenv.ProcessOutput_Stdout:
			o.stdout.Write(v.Stdout)
		case *ateenv.ProcessOutput_Stderr:
			o.stderr.Write(v.Stderr)
		case *ateenv.ProcessOutput_Exit:
			o.exit = v.Exit
		}
		if stopAfter > 0 && o.stdout.Len() >= stopAfter && o.exit == nil {
			return o, nil
		}
	}
}

func (h *guestUnderTest) writeInput(ctx context.Context, reqs ...*ateenv.WriteProcessInputRequest) (*ateenv.WriteProcessInputResponse, error) {
	s, err := h.conn.NewStream(ctx, agent.MethodWriteProcessInput, nil)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	for _, r := range reqs {
		if err := s.Send(r); err != nil {
			return nil, err
		}
	}
	s.CloseSend()
	resp := new(ateenv.WriteProcessInputResponse)
	if err := s.Recv(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestProcessStreamsStdoutStderrAndExit(t *testing.T) {
	ctx := testCtx(t)
	h := startGuest(t, newTarget(t, "main"))
	p := h.start1(ctx, &ateenv.StartProcessRequest{
		Command: []string{"sh", "-c", `echo "out $FOO"; echo err >&2; pwd; exit 3`},
		Env:     map[string]string{"FOO": "it's"},
	})
	if p.GetProcessId() == "" || p.GetPid() <= 1 || p.GetStartedAt() == nil {
		t.Fatalf("StartProcess = %v, want an id, a pid and a start time", p)
	}
	o, err := h.follow(ctx, p.GetProcessId(), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	root := h.targets[0].Rootfs()
	if got, want := o.stdout.String(), "out it's\n"+filepath.Join(root, "workspace")+"\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got := o.stderr.String(); got != "err\n" {
		t.Errorf("stderr = %q, want %q", got, "err\n")
	}
	if o.exit == nil || o.exit.GetState() != ateenv.ProcessState_PROCESS_STATE_EXITED || o.exit.GetExitCode() != 3 || o.exit.GetFinishedAt() == nil {
		t.Fatalf("exit = %v, want EXITED with code 3 and a finish time", o.exit)
	}
	got := new(ateenv.Process)
	if err := h.conn.Invoke(ctx, agent.MethodGetProcess, &ateenv.GetProcessRequest{ProcessId: p.GetProcessId()}, got, nil); err != nil {
		t.Fatal(err)
	}
	if got.GetExitCode() != 3 || got.GetState() != ateenv.ProcessState_PROCESS_STATE_EXITED {
		t.Errorf("GetProcess = %v", got)
	}
	err = h.conn.Invoke(ctx, agent.MethodGetProcess, &ateenv.GetProcessRequest{ProcessId: "nope"}, got, nil)
	if status.Code(err) != codes.NotFound {
		t.Errorf("GetProcess(unknown) = %v, want NotFound", err)
	}
}

func TestProcessRecordLivesInTheTargetContainer(t *testing.T) {
	ctx := testCtx(t)
	main, side := newTarget(t, "main"), newTarget(t, "side")
	h := startGuest(t, main, side)
	p := new(ateenv.Process)
	err := h.conn.Invoke(ctx, agent.MethodStartProcess, &ateenv.StartProcessRequest{Command: []string{"cat"}, Stdin: true}, p,
		map[string]string{agent.ContainerHeader: "side"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.writeInput(ctx, &ateenv.WriteProcessInputRequest{ProcessId: p.GetProcessId(), Data: []byte("x"), Close: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.follow(ctx, p.GetProcessId(), 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(side.Root, "everdict", "exec", p.GetProcessId())
	for _, f := range []string{"cmd.sh", "pgid", "out", "err", "exit", "in"} {
		if _, err := os.Lstat(filepath.Join(dir, f)); err != nil {
			t.Errorf("record file %s: %v", f, err)
		}
	}
	if fi, err := os.Lstat(filepath.Join(dir, "in")); err == nil && fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("in is %v, want a FIFO", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(main.Root, "everdict", "exec", p.GetProcessId())); !os.IsNotExist(err) {
		t.Errorf("record also in the other container: %v", err)
	}
	err = h.conn.Invoke(ctx, agent.MethodStartProcess, &ateenv.StartProcessRequest{Command: []string{"true"}}, p,
		map[string]string{agent.ContainerHeader: "ghost"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("StartProcess in an unknown container = %v, want NotFound", err)
	}
}

func TestProcessStdin(t *testing.T) {
	ctx := testCtx(t)
	h := startGuest(t, newTarget(t, "main"))
	p := h.start1(ctx, &ateenv.StartProcessRequest{Command: []string{"cat"}, Stdin: true})
	resp, err := h.writeInput(ctx,
		&ateenv.WriteProcessInputRequest{ProcessId: p.GetProcessId(), Data: []byte("hello ")},
		&ateenv.WriteProcessInputRequest{Data: []byte("world")})
	if err != nil || resp.GetBytesWritten() != 11 {
		t.Fatalf("WriteProcessInput = %v, %v; want 11 bytes", resp, err)
	}
	// A second call: stdin stays open across calls until a message closes it.
	if _, err := h.writeInput(ctx, &ateenv.WriteProcessInputRequest{ProcessId: p.GetProcessId(), Data: []byte("!"), Close: true}); err != nil {
		t.Fatal(err)
	}
	o, err := h.follow(ctx, p.GetProcessId(), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if o.stdout.String() != "hello world!" || o.exit.GetExitCode() != 0 {
		t.Fatalf("stdout %q exit %v, want %q and 0", o.stdout.String(), o.exit, "hello world!")
	}
	_, err = h.writeInput(ctx, &ateenv.WriteProcessInputRequest{ProcessId: p.GetProcessId(), Data: []byte("late")})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("input after close = %v, want FailedPrecondition", err)
	}
	q := h.start1(ctx, &ateenv.StartProcessRequest{Command: []string{"true"}})
	_, err = h.writeInput(ctx, &ateenv.WriteProcessInputRequest{ProcessId: q.GetProcessId(), Data: []byte("x")})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("input to a process without stdin = %v, want FailedPrecondition", err)
	}
}

func TestProcessSignalAndTimeout(t *testing.T) {
	ctx := testCtx(t)
	h := startGuest(t, newTarget(t, "main"))
	p := h.start1(ctx, &ateenv.StartProcessRequest{Command: []string{"sh", "-c", "sleep 30 & wait"}})
	got := new(ateenv.Process)
	if err := h.conn.Invoke(ctx, agent.MethodSignalProcess, &ateenv.SignalProcessRequest{ProcessId: p.GetProcessId(), Signal: ateenv.Signal_SIGNAL_TERM}, got, nil); err != nil {
		t.Fatal(err)
	}
	o, err := h.follow(ctx, p.GetProcessId(), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if o.exit.GetExitCode() != 143 {
		t.Errorf("exit after SIGTERM = %v, want 143", o.exit)
	}
	err = h.conn.Invoke(ctx, agent.MethodSignalProcess, &ateenv.SignalProcessRequest{ProcessId: p.GetProcessId(), Signal: ateenv.Signal_SIGNAL_TERM}, got, nil)
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("signal after exit = %v, want FailedPrecondition", err)
	}

	q := h.start1(ctx, &ateenv.StartProcessRequest{Command: []string{"sleep", "30"}, Timeout: durationpb.New(300 * time.Millisecond)})
	o, err = h.follow(ctx, q.GetProcessId(), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if o.exit.GetExitCode() != 137 {
		t.Errorf("exit after timeout = %v, want 137 (SIGKILL)", o.exit)
	}
}

// slowCounter prints n numbered lines, one every 20ms, then exits with 7.
var slowCounter = []string{"sh", "-c", `i=0; while [ $i -lt 40 ]; do i=$((i+1)); echo "line $i"; echo "e$i" >&2; sleep 0.02; done; exit 7`}

func wantCounter(t *testing.T, stdout, stderr string, exit *ateenv.Process) {
	t.Helper()
	var wo, we strings.Builder
	for i := 1; i <= 40; i++ {
		wo.WriteString("line " + itoa(i) + "\n")
		we.WriteString("e" + itoa(i) + "\n")
	}
	if stdout != wo.String() {
		t.Errorf("stdout across the cut:\n got %q\nwant %q", stdout, wo.String())
	}
	if stderr != we.String() {
		t.Errorf("stderr across the cut:\n got %q\nwant %q", stderr, we.String())
	}
	if exit.GetExitCode() != 7 || exit.GetState() != ateenv.ProcessState_PROCESS_STATE_EXITED {
		t.Errorf("exit = %v, want EXITED 7", exit)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// A client that loses its stream mid-turn reconnects with the offsets it holds and gets every byte exactly once.
func TestStreamReconnectFromOffsetsLosesNothing(t *testing.T) {
	ctx := testCtx(t)
	h := startGuest(t, newTarget(t, "main"))
	p := h.start1(ctx, &ateenv.StartProcessRequest{Command: slowCounter})
	var stdout, stderr bytes.Buffer
	var exit *ateenv.Process
	for exit == nil {
		o, err := h.follow(ctx, p.GetProcessId(), int64(stdout.Len()), int64(stderr.Len()), stdout.Len()+30)
		if err != nil {
			t.Fatal(err)
		}
		stdout.Write(o.stdout.Bytes())
		stderr.Write(o.stderr.Bytes())
		exit = o.exit
	}
	wantCounter(t, stdout.String(), stderr.String(), exit)
}

// A guest restarted mid-exec re-follows the running process from byte offsets and reports its exit.
func TestRestartedGuestRefollowsRunningExec(t *testing.T) {
	ctx := testCtx(t)
	h := startGuest(t, newTarget(t, "main"))
	p := h.start1(ctx, &ateenv.StartProcessRequest{Command: slowCounter})
	first, err := h.follow(ctx, p.GetProcessId(), 0, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	h.restart()
	running := new(ateenv.Process)
	if err := h.conn.Invoke(ctx, agent.MethodGetProcess, &ateenv.GetProcessRequest{ProcessId: p.GetProcessId()}, running, nil); err != nil {
		t.Fatalf("GetProcess after restart: %v", err)
	}
	if running.GetPid() != p.GetPid() {
		t.Errorf("pid after restart = %d, want %d", running.GetPid(), p.GetPid())
	}
	rest, err := h.follow(ctx, p.GetProcessId(), int64(first.stdout.Len()), int64(first.stderr.Len()), 0)
	if err != nil {
		t.Fatalf("follow after restart: %v", err)
	}
	wantCounter(t, first.stdout.String()+rest.stdout.String(), first.stderr.String()+rest.stderr.String(), rest.exit)
}

// stdin opened before a restart stays open across it; input written after the restart reaches the same process.
func TestRestartedGuestKeepsStdin(t *testing.T) {
	ctx := testCtx(t)
	h := startGuest(t, newTarget(t, "main"))
	p := h.start1(ctx, &ateenv.StartProcessRequest{Command: []string{"cat"}, Stdin: true})
	if _, err := h.writeInput(ctx, &ateenv.WriteProcessInputRequest{ProcessId: p.GetProcessId(), Data: []byte("before ")}); err != nil {
		t.Fatal(err)
	}
	h.restart()
	if _, err := h.writeInput(ctx, &ateenv.WriteProcessInputRequest{ProcessId: p.GetProcessId(), Data: []byte("after"), Close: true}); err != nil {
		t.Fatalf("input after restart: %v", err)
	}
	o, err := h.follow(ctx, p.GetProcessId(), 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if o.stdout.String() != "before after" || o.exit.GetExitCode() != 0 {
		t.Fatalf("stdout %q exit %v, want %q and 0", o.stdout.String(), o.exit, "before after")
	}
}

func TestFiles(t *testing.T) {
	ctx := testCtx(t)
	tg := newTarget(t, "main")
	h := startGuest(t, tg)
	write := func(reqs ...*ateenv.WriteFileRequest) (*ateenv.WriteFileResponse, error) {
		s, err := h.conn.NewStream(ctx, agent.MethodWriteFile, nil)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		for _, r := range reqs {
			if err := s.Send(r); err != nil {
				return nil, err
			}
		}
		s.CloseSend()
		resp := new(ateenv.WriteFileResponse)
		return resp, s.Recv(resp)
	}
	read := func(p string, mode uint32) ([]byte, error) {
		s, err := h.conn.NewStream(ctx, agent.MethodReadFile, nil)
		if err != nil {
			return nil, err
		}
		defer s.Close()
		s.Send(&ateenv.ReadFileRequest{Path: p, Mode: mode})
		s.CloseSend()
		var b bytes.Buffer
		for {
			m := new(ateenv.ReadFileResponse)
			err := s.Recv(m)
			if errors.Is(err, io.EOF) {
				return b.Bytes(), nil
			}
			if err != nil {
				return nil, err
			}
			b.Write(m.GetChunk())
		}
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), 10000) // 160 KB: several chunks each way
	resp, err := write(&ateenv.WriteFileRequest{Path: "dir/a.txt", Mode: 0o600, Chunk: big[:100000]}, &ateenv.WriteFileRequest{Chunk: big[100000:]})
	if err != nil || resp.GetBytesWritten() != int64(len(big)) {
		t.Fatalf("WriteFile = %v, %v", resp, err)
	}
	if b, err := os.ReadFile(filepath.Join(tg.Root, "workspace", "dir", "a.txt")); err != nil || !bytes.Equal(b, big) {
		t.Fatalf("file on disk: %d bytes, %v", len(b), err)
	}
	if b, err := read("/workspace/dir/a.txt", 0); err != nil || !bytes.Equal(b, big) {
		t.Fatalf("ReadFile: %d bytes, %v", len(b), err)
	}
	if _, err := write(&ateenv.WriteFileRequest{Path: "dir/a.txt", SeekOffset: 4, Chunk: []byte("XY")}); err != nil {
		t.Fatal(err)
	}
	if b, _ := read("dir/a.txt", 0); string(b[:8]) != "0123XY67" || len(b) != len(big) {
		t.Errorf("after seek write: %q... (%d bytes)", b[:8], len(b))
	}
	if _, err := read("missing", 0); status.Code(err) != codes.NotFound {
		t.Errorf("ReadFile(missing) = %v, want NotFound", err)
	}
	if b, err := read("new/empty", 0o640); err != nil || len(b) != 0 {
		t.Errorf("ReadFile(missing, mode) = %q, %v; want created empty", b, err)
	}

	fi := new(herdext.FileInfo)
	if err := h.conn.Invoke(ctx, agent.MethodStatFile, &herdext.StatFileRequest{Path: "dir/a.txt"}, fi, nil); err != nil {
		t.Fatal(err)
	}
	if fi.GetPath() != "/workspace/dir/a.txt" || fi.GetType() != herdext.FileType_FILE_TYPE_REGULAR || fi.GetSize() != int64(len(big)) || fi.GetMode() != 0o600 {
		t.Errorf("StatFile = %v", fi)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(tg.Root, "workspace", "link")); err != nil {
		t.Fatal(err)
	}
	ls := new(herdext.ListDirectoryResponse)
	if err := h.conn.Invoke(ctx, agent.MethodListDirectory, &herdext.ListDirectoryRequest{Path: "/workspace"}, ls, nil); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ls.GetEntries() {
		names = append(names, e.GetName()+":"+e.GetType().String()+":"+e.GetLinkTarget())
	}
	if got, want := strings.Join(names, ","), "dir:FILE_TYPE_DIRECTORY:,link:FILE_TYPE_SYMLINK:/etc/passwd,new:FILE_TYPE_DIRECTORY:"; got != want {
		t.Errorf("ListDirectory = %s, want %s", got, want)
	}

	// Confinement: neither .. nor a symlink reaches outside the container's root.
	outside := filepath.Join(filepath.Dir(tg.Root), "outside-"+filepath.Base(tg.Root))
	if err := os.WriteFile(outside, []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(outside) })
	if err := os.Symlink(outside, filepath.Join(tg.Root, "workspace", "escape")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"escape", "../../" + filepath.Base(outside)} {
		b, err := read(p, 0)
		if err == nil || bytes.Contains(b, []byte("secret")) {
			t.Errorf("ReadFile(%q) = %q, %v; want refused", p, b, err)
		}
	}
	if _, err := write(&ateenv.WriteFileRequest{Path: "escape", Chunk: []byte("pwned")}); err == nil {
		t.Errorf("WriteFile through a symlink out of the root succeeded")
	}
	if b, _ := os.ReadFile(outside); string(b) != "host secret" {
		t.Errorf("file outside the root changed: %q", b)
	}
}

// dialGuest connects to a herd-guest serving on a unix socket.
func dialGuest(t testing.TB, sock string) *guestclient.Conn {
	t.Helper()
	c, err := guestclient.New(transport.DialUnix(sock))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// sockPath returns a unix socket path in a short temporary directory: t.TempDir() on macOS exceeds the 104-byte
// sun_path limit (bind: invalid argument).
func sockPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// TestFilesInVolumes: the container's volumes are bound in its own mount namespace, which the guest's file API does
// not share, so a path at or below a volume's mount path is served from the volume's directory (the share), the
// innermost mount winning; the rootfs underneath the mount point is not touched.
func TestFilesInVolumes(t *testing.T) {
	tg := newTarget(t, "main")
	share := t.TempDir()
	work, cache := filepath.Join(share, "work"), filepath.Join(share, "cache")
	for _, d := range []string{work, cache, filepath.Join(tg.Root, "work")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tg.Dir, tg.Volumes = "/work", map[string]string{"/work": work, "/work/cache": cache}
	g, err := agent.New(tg)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	md := func(string) string { return "" }
	write := func(p, body string) error {
		sent := false
		_, err := g.WriteFile(md, func() (*ateenv.WriteFileRequest, error) {
			if sent {
				return nil, io.EOF
			}
			sent = true
			return &ateenv.WriteFileRequest{Path: p, Chunk: []byte(body)}, nil
		})
		return err
	}
	read := func(p string) (string, error) {
		var b strings.Builder
		err := g.ReadFile(md, &ateenv.ReadFileRequest{Path: p}, func(r *ateenv.ReadFileResponse) error {
			b.Write(r.GetChunk())
			return nil
		})
		return b.String(), err
	}
	for p, want := range map[string]string{"/work/conformance.txt": filepath.Join(work, "conformance.txt"),
		"notes/a.md": filepath.Join(work, "notes", "a.md"), "/work/cache/blob": filepath.Join(cache, "blob")} {
		if err := write(p, p); err != nil {
			t.Fatalf("WriteFile(%s): %v", p, err)
		}
		if b, err := os.ReadFile(want); err != nil || string(b) != p {
			t.Errorf("WriteFile(%s): %s holds %q, %v; want the write in the volume", p, want, b, err)
		}
		if got, err := read(p); err != nil || got != p {
			t.Errorf("ReadFile(%s) = %q, %v", p, got, err)
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(tg.Root, "work")); len(ents) != 0 {
		t.Errorf("the rootfs under the mount point holds %d entries; want the volume's files only in the volume", len(ents))
	}
	fi, err := g.StatFile(md, &herdext.StatFileRequest{Path: "/work"})
	if err != nil || fi.GetPath() != "/work" || fi.GetName() != "work" || fi.GetType() != herdext.FileType_FILE_TYPE_DIRECTORY {
		t.Errorf("StatFile(/work) = %v, %v", fi, err)
	}
	ls, err := g.ListDirectory(md, &herdext.ListDirectoryRequest{Path: "/work/notes"})
	if err != nil || len(ls.GetEntries()) != 1 || ls.GetEntries()[0].GetPath() != "/work/notes/a.md" {
		t.Errorf("ListDirectory(/work/notes) = %v, %v", ls, err)
	}
	// A symlink in a volume does not lead out of it.
	if err := os.Symlink("/etc/passwd", filepath.Join(work, "escape")); err != nil {
		t.Fatal(err)
	}
	if got, err := read("/work/escape"); err == nil {
		t.Errorf("ReadFile through a volume's symlink = %q; want refused", got)
	}
}
