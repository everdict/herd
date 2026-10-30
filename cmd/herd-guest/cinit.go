package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// containerInit is PID 1 inside a container: it runs argv, forwards the signals it gets to it, reaps every orphan
// (exec shims are reparented here when crun exec returns) and exits with argv's status.
func containerInit(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "herd-guest cinit: no command")
		return 127
	}
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "herd-guest cinit:", err)
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
					return 128 + int(ws.Signal())
				}
				return ws.ExitStatus()
			}
		}
	}
}
