//go:build linux

package cloudhypervisor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// ProcSpec is a host process to start: cloud-hypervisor or virtiofsd.
type ProcSpec struct {
	Path string
	Args []string
	// Log receives the process's stdout and stderr.
	Log string
	// Marker is a string in the command line that identifies this process (its socket path), so that a pid found
	// again later is not mistaken for an unrelated process that reused it.
	Marker string
	// Cgroup, if set, is the cgroup v2 directory the process starts in (Cgroups.Create's), rather than the herdlet's own.
	Cgroup string
}

// Process is a started or found host process.
type Process interface {
	Pid() int
	Signal(sig syscall.Signal) error
	// Exited reports whether the process has been observed exited. An error means it could not be observed.
	Exited() (bool, error)
}

// Processes is the boundary to the host's process table. The exec implementation starts real processes; tests use
// one that starts an in-process fake Cloud Hypervisor.
type Processes interface {
	Start(p ProcSpec) (Process, error)
	// Find returns the process with pid whose command line contains marker, as recorded in a pid file.
	Find(pid int, marker string) Process
}

// ExecProcesses starts real processes with os/exec.
type ExecProcesses struct{}

func (ExecProcesses) Start(p ProcSpec) (Process, error) {
	log, err := os.OpenFile(p.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(p.Path, p.Args...)
	cmd.Stdout, cmd.Stderr = log, log
	// Its own process group: a signal to herdlet's group does not take the VM down with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if p.Cgroup != "" {
		// Started inside the VM's cgroup (clone3 CLONE_INTO_CGROUP): not one page is charged to the herdlet's.
		cg, err := os.Open(p.Cgroup)
		if err != nil {
			_ = log.Close()
			return nil, fmt.Errorf("cgroup: %w", err)
		}
		defer cg.Close()
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = true, int(cg.Fd())
		// Outside the herdlet's cgroup, a Nomad task stop or systemd unit stop no longer ends the VM with the herdlet,
		// and a restarted herdlet does not adopt the VMs it left (ADR A-19, Limits): the VM ends when the herdlet does.
		cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	}
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		return nil, err
	}
	c := &child{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		_ = log.Close()
		close(c.done)
	}()
	return c, nil
}

func (ExecProcesses) Find(pid int, marker string) Process {
	return procfsProcess{pid: pid, marker: marker}
}

// child is a process this herdlet started and reaps.
type child struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (c *child) Pid() int { return c.cmd.Process.Pid }

func (c *child) Signal(sig syscall.Signal) error {
	if err := c.cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (c *child) Exited() (bool, error) {
	select {
	case <-c.done:
		return true, nil
	default:
		return false, nil
	}
}

// ExitStatus is how the process ended ("exit status 1", "signal: killed"); empty while it runs.
func (c *child) ExitStatus() string {
	select {
	case <-c.done:
		if c.cmd.ProcessState != nil {
			return c.cmd.ProcessState.String()
		}
	default:
	}
	return ""
}

// procfsProcess is a process found again through a pid file (after a herdlet restart).
type procfsProcess struct {
	pid    int
	marker string
}

func (p procfsProcess) Pid() int { return p.pid }

func (p procfsProcess) Signal(sig syscall.Signal) error {
	if gone, err := p.Exited(); err != nil || gone {
		return err
	}
	if err := syscall.Kill(p.pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (p procfsProcess) Exited() (bool, error) {
	dir := "/proc/" + strconv.Itoa(p.pid)
	stat, err := os.ReadFile(dir + "/stat")
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("observe pid %d: %w", p.pid, err)
	}
	// The state follows the parenthesised command name: "pid (comm) S ...".
	if i := bytes.LastIndexByte(stat, ')'); i >= 0 && i+2 < len(stat) && (stat[i+2] == 'Z' || stat[i+2] == 'X') {
		return true, nil
	}
	cmdline, err := os.ReadFile(dir + "/cmdline")
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("observe pid %d: %w", p.pid, err)
	}
	// An empty command line (a process in the middle of exec) is no evidence of exit: it counts as running.
	if len(cmdline) > 0 && !bytes.Contains(cmdline, []byte(p.marker)) {
		// The pid now belongs to another process.
		return true, nil
	}
	return false, nil
}
