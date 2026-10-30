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
}

func (t *Target) Name() string    { return t.ContainerName }
func (t *Target) Rootfs() string  { return t.RootfsDir }
func (t *Target) Workdir() string { return t.Dir }

func (t *Target) StartShim(ctx context.Context, dir string) error {
	return t.Runtime.Exec(ctx, t.ContainerName, []string{GuestBinary, "shim", dir}, true)
}

func (t *Target) Signal(ctx context.Context, pgid int, sig syscall.Signal) error {
	return t.Runtime.Exec(ctx, t.ContainerName, []string{GuestBinary, "signal", strconv.Itoa(pgid), strconv.Itoa(int(sig))}, false)
}
