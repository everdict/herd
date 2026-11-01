package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/everdict/herd/guest/oci"
)

// containerInit is PID 1 inside a container: it runs argv, forwards the signals it gets to it, reaps every orphan
// (exec shims are reparented here when crun exec returns) and exits with argv's status. With report (oci.ReportFlag)
// argv's stdout and stderr go to descriptor oci.OutputFD, and how argv ended is written to oci.StatusFD before
// containerInit returns: herd-guest learns from them that the container's main process is gone, and why.
func containerInit(argv []string, report bool) int {
	var out io.Writer = os.Stderr
	status := func(code int, sig syscall.Signal) {}
	stdout, stderr := os.Stdout, os.Stderr
	if report {
		o, s := os.NewFile(oci.OutputFD, "output"), os.NewFile(oci.StatusFD, "status")
		// Only this process holds them: argv inherits the output as its stdio, and nothing inherits the status, whose
		// close tells herd-guest this init is gone.
		syscall.CloseOnExec(oci.OutputFD)
		syscall.CloseOnExec(oci.StatusFD)
		out, stdout, stderr = o, o, o
		status = func(code int, sig syscall.Signal) { _, _ = io.WriteString(s, oci.StatusLine(code, sig)) }
	}
	if len(argv) == 0 {
		fmt.Fprintln(out, "herd-guest cinit: no command")
		status(127, 0)
		return 127
	}
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(out, "herd-guest cinit:", err)
		status(127, 0)
		return 127
	}
	main := cmd.Process.Pid
	for {
		sig := <-sigs
		if sig == syscall.SIGURG { // the Go runtime's own preemption signal
			continue
		}
		if sig != syscall.SIGCHLD {
			_ = cmd.Process.Signal(sig)
			continue
		}
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if err != nil || pid <= 0 {
				break
			}
			if pid == main {
				if ws.Signaled() {
					status(0, ws.Signal())
					return 128 + int(ws.Signal())
				}
				status(ws.ExitStatus(), 0)
				return ws.ExitStatus()
			}
		}
	}
}
