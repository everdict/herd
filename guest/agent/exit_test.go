package agent_test

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	"github.com/everdict/herd/guest/agent"
	"github.com/everdict/herd/guest/oci"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// deadOnExec is crun for a container whose init ends as the exec arrives: crun refuses the exec, and the init's
// report (status 1, su-exec's complaint) reaches herd-guest a moment later.
type deadOnExec struct {
	outW, stW *os.File
}

func (d *deadOnExec) Run(context.Context, string, string) (*oci.Watch, error) { return nil, nil }
func (d *deadOnExec) Exec(_ context.Context, id string, _ oci.Process, _ bool) error {
	io.WriteString(d.outW, "su-exec: setgroups: Operation not permitted\n")
	io.WriteString(d.stW, oci.StatusLine(1, 0))
	d.outW.Close()
	d.stW.Close()
	return errors.New("oci: crun [exec --detach " + id + " /.herd/herd-guest shim /.herd/exec/x]: exit status 255: the container `" + id + "` is not running")
}
func (d *deadOnExec) Kill(context.Context, string, syscall.Signal) error { return nil }

// TestExecIntoAnExitedContainerNamesTheExit (PLT-230): crun answers an exec into a container whose init is gone with
// a bare "the container `box` is not running"; herd-guest answers FAILED_PRECONDITION naming how the container's main
// process ended and its last output, also when crun refuses before herd-guest has read the init's report.
func TestExecIntoAnExitedContainerNamesTheExit(t *testing.T) {
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stR, stW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { outW.Close(); stW.Close() })
	rt := &deadOnExec{outW: outW, stW: stW}
	tg := &oci.Target{ContainerName: "box", RootfsDir: t.TempDir(), Dir: "/", Runtime: rt, Process: &oci.Process{}, Watch: oci.NewWatch("box", outR, stR)}
	g, err := agent.New(tg)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	md := func(string) string { return "" }
	for i, what := range []string{"refused by crun before the report is read", "known to have exited"} {
		_, err := g.StartProcess(context.Background(), md, &ateenv.StartProcessRequest{Command: []string{"redis-cli", "ping"}})
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "container box exited with status 1") ||
			!strings.Contains(err.Error(), "su-exec: setgroups: Operation not permitted") {
			t.Errorf("exec %d (%s): %v, want FAILED_PRECONDITION naming the exit and its last output", i, what, err)
		}
	}
}
