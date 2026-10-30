//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// As PID 1 of the microVM herd-guest inherits every orphan (crun's detached container processes among them) and must
// reap them, without stealing the exit status of the commands it runs itself with os/exec. Its own children are
// registered under mu while they start; the reaper waits only for zombie children it does not own.
var (
	mu    sync.Mutex
	owned = map[int]bool{}
)

func reapedRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	// Output goes to a file, not a pipe: `crun run --detach` exits while the container keeps the stdio it inherited,
	// and with a pipe Wait would block until the container exits (herd-guest never served on a real VM).
	f, err := os.CreateTemp("", "herd-crun-out-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = f, f
	mu.Lock()
	err = cmd.Start()
	if err == nil {
		owned[cmd.Process.Pid] = true
	}
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	err = cmd.Wait()
	mu.Lock()
	delete(owned, cmd.Process.Pid)
	mu.Unlock()
	out, readErr := os.ReadFile(f.Name())
	if readErr != nil {
		return nil, errors.Join(err, readErr)
	}
	return out, err
}

func startReaper() {
	ch := make(chan os.Signal, 64)
	signal.Notify(ch, syscall.SIGCHLD)
	go func() {
		for range ch {
			reapOrphans()
		}
	}()
}

// reapOrphans waits for every zombie whose parent is this process and that os/exec does not own.
func reapOrphans() {
	mu.Lock()
	defer mu.Unlock()
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	self := strconv.Itoa(os.Getpid())
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || owned[pid] {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...; comm may contain spaces and parentheses.
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 || f[0] != "Z" || f[1] != self {
			continue
		}
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	}
}
