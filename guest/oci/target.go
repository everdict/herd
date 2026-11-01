package oci

import (
	"context"
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
	// User is the identity the container's processes run as (crun exec takes the container's): exec records are
	// handed to it.
	User User
	// Watch, if set, follows the container's main process.
	Watch *Watch
}

func (t *Target) Name() string    { return t.ContainerName }
func (t *Target) Rootfs() string  { return t.RootfsDir }
func (t *Target) Workdir() string { return t.Dir }

func (t *Target) VolumeDirs() map[string]string { return t.Volumes }

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
	return t.Runtime.Exec(ctx, t.ContainerName, []string{GuestBinary, "shim", dir}, true)
}

func (t *Target) Signal(ctx context.Context, pgid int, sig syscall.Signal) error {
	return t.Runtime.Exec(ctx, t.ContainerName, []string{GuestBinary, "signal", strconv.Itoa(pgid), strconv.Itoa(int(sig))}, false)
}
