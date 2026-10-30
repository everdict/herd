// Package agent is herd-guest's API: agent-substrate/env's ProcessService and FileSystemService (ateenv.v1alpha,
// wire-compatible) plus herd's FileInfoService (stat, list), served over gRPC. Every call acts on one target
// container, chosen by the request metadata key x-herd-container (default: the first container). Processes are
// Everdict detached-exec records (package execrec) inside the target's filesystem, so the service keeps no state a
// restart could lose.
package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// ContainerHeader is the request metadata key that names the target container.
const ContainerHeader = "x-herd-container"

// Target is a container processes and files act on. The crun-backed implementation lives in package oci; Local
// runs on the guest's own kernel namespaces (tests, and herd-guest outside a microVM).
type Target interface {
	// Name is the container's name in the template.
	Name() string
	// Rootfs is the container's root filesystem as the guest sees it.
	Rootfs() string
	// Workdir is the container's working directory: the default cwd of a process and the base of relative paths.
	Workdir() string
	// StartShim starts `herd-guest shim <dir>` detached inside the container; dir is the record's path as the
	// container sees it. It returns once the shim has been started, not when it ends.
	StartShim(ctx context.Context, dir string) error
	// Signal delivers sig to process group pgid as numbered inside the container.
	Signal(ctx context.Context, pgid int, sig syscall.Signal) error
}

// VolumeTarget is a Target with DurableDir volumes bound into it. The binds live in the container's own mount
// namespace, so the guest does not see them under Rootfs: files below a volume's mount path are read and written in
// the volume's directory, where the container's processes (and the host's share) see them.
type VolumeTarget interface {
	Target
	// VolumeDirs maps each volume's mount path in the container (clean, absolute) to its directory in the guest.
	VolumeDirs() map[string]string
}

// Local is a Target whose processes share the guest's namespaces; its "container" is a directory.
type Local struct {
	ContainerName string
	Root          string
	Dir           string
	// Volumes, if set, are VolumeDirs: mount path in the container → directory.
	Volumes map[string]string
	// Shim is the command that runs the shim; the record directory is appended. Default: this executable with the
	// argument "shim".
	Shim []string
}

func (l *Local) Name() string    { return l.ContainerName }
func (l *Local) Rootfs() string  { return l.Root }
func (l *Local) Workdir() string { return l.Dir }

func (l *Local) VolumeDirs() map[string]string { return l.Volumes }

// ProcessPath maps a path in the container to the path its processes see: for a local target, the same path under
// Root, since nothing changes their root.
func (l *Local) ProcessPath(p string) string { return filepath.Join(l.Root, p) }

func (l *Local) StartShim(ctx context.Context, dir string) error {
	argv := l.Shim
	if len(argv) == 0 {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("agent: %w", err)
		}
		argv = []string{self, "shim"}
	}
	// The record path is the container's; for a local target the container root is a directory.
	cmd := exec.Command(argv[0], append(append([]string(nil), argv[1:]...), filepath.Join(l.Root, dir))...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("agent: starting shim: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func (l *Local) Signal(_ context.Context, pgid int, sig syscall.Signal) error {
	if pgid <= 1 {
		return fmt.Errorf("agent: refusing to signal process group %d", pgid)
	}
	return syscall.Kill(-pgid, sig)
}
