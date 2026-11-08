package oci

import (
	"context"
	"fmt"
	"strconv"
	"syscall"
)

// Target is a running container as herd-guest's API sees it (it satisfies agent.Target): processes start through
// `herd-guest shim` and are signalled through `herd-guest signal`, both exec'd inside the container, so pids and
// paths are the container's own.
type Target struct {
	ContainerName string
	RootfsDir     string
	Dir           string
	Runtime       Runtime
	// Volumes maps each volume's mount path in the container to its directory in the guest (the share's): the binds are
	// in the container's mount namespace only, so the guest's file API reaches them there.
	Volumes map[string]string
	// User is the identity the container's processes run as: exec records are handed to it.
	User User
	// Process is the container's process (its config.json's): every exec runs as it does (Process.ExecProcess), with
	// its user, environment and capabilities.
	Process *Process
	// Watch, if set, follows the container's main process.
	Watch *Watch
	// LayerDirs are the container's writable directories in the guest's memory (its overlay upper and its /tmp), by
	// their names in its layer archive: Sync saves them for a snapshot (agent.LayerTarget).
	LayerDirs map[string]string
}

func (t *Target) Name() string    { return t.ContainerName }
func (t *Target) Rootfs() string  { return t.RootfsDir }
func (t *Target) Workdir() string { return t.Dir }

func (t *Target) VolumeDirs() map[string]string { return t.Volumes }

func (t *Target) Layers() map[string]string { return t.LayerDirs }

// Owner is the uid and gid the container's processes run as.
func (t *Target) Owner() (uid, gid int) { return int(t.User.UID), int(t.User.GID) }

// Exited answers the container's exit (a *ContainerExit) once its main process has ended, or nil while it runs.
func (t *Target) Exited() error {
	if t.Watch == nil {
		return nil
	}
	if e := t.Watch.Exited(); e != nil {
		return e
	}
	return nil
}

func (t *Target) StartShim(ctx context.Context, dir string) error {
	return t.exec(ctx, []string{GuestBinary, "shim", dir}, true)
}

func (t *Target) Signal(ctx context.Context, pgid int, sig syscall.Signal) error {
	return t.exec(ctx, []string{GuestBinary, "signal", strconv.Itoa(pgid), strconv.Itoa(int(sig))}, false)
}

// exec runs argv in the container as the container's process; a target without one runs nothing rather than an exec
// whose capabilities crun would choose.
func (t *Target) exec(ctx context.Context, argv []string, detach bool) error {
	if t.Process == nil {
		return fmt.Errorf("oci: container %q: no process to exec as", t.ContainerName)
	}
	return t.Runtime.Exec(ctx, t.ContainerName, t.Process.ExecProcess(argv), detach)
}
