//go:build linux || darwin

package main

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/everdict/herd/guest/oci"
)

// TestContainerInitReports (PLT-230): with -report, cinit sends its command's stdout and stderr to descriptor 3 and,
// once the command ends, its status to descriptor 4, which only cinit holds: a child the command left running keeps
// the output open, not the status, so herd-guest learns the exit when cinit is gone.
func TestContainerInitReports(t *testing.T) {
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stR, stW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := herdGuest("cinit", oci.ReportFlag, "--", "sh", "-c", `echo to-stdout; echo to-stderr >&2; sleep 10 & exit 3`)
	cmd.ExtraFiles = []*os.File{outW, stW}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	outW.Close()
	stW.Close()
	w := oci.NewWatch("box", outR, stR)
	t.Cleanup(func() { _ = cmd.Wait() })
	select {
	case <-w.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the status descriptor stayed open after the command exited (inherited by the child it left running?)")
	}
	e := w.Exited()
	if e == nil || !e.Known || e.Code != 3 {
		t.Fatalf("exit = %v, want status 3", e)
	}
	if got := strings.Join(e.Output, "|"); got != "to-stdout|to-stderr" {
		t.Errorf("output = %q, want the command's stdout and stderr", got)
	}
}

// TestContainerInitReportsAFailedStart: a command that cannot start is reported as status 127 with cinit's reason.
func TestContainerInitReportsAFailedStart(t *testing.T) {
	outR, outW, _ := os.Pipe()
	stR, stW, _ := os.Pipe()
	cmd := herdGuest("cinit", oci.ReportFlag, "--", "/no/such/entrypoint")
	cmd.ExtraFiles = []*os.File{outW, stW}
	if err := cmd.Run(); err == nil {
		t.Fatal("cinit exited 0 for a command that cannot start")
	}
	outW.Close()
	stW.Close()
	out, _ := io.ReadAll(outR)
	st, _ := io.ReadAll(stR)
	if string(st) != "exit 127\n" || !strings.Contains(string(out), "/no/such/entrypoint") {
		t.Errorf("status %q, output %q: want exit 127 naming the command", st, out)
	}
}
