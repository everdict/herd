package oci

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// Runtime runs OCI containers from bundles.
type Runtime interface {
	// Run creates and starts the container from bundle (a directory with config.json) and returns once its process
	// runs; the container keeps running after Run returns.
	Run(ctx context.Context, id, bundle string) error
	// Exec runs argv inside a running container. With detach it returns once the process started; without, once it
	// ended, with an error if it failed.
	Exec(ctx context.Context, id string, argv []string, detach bool) error
	// Kill sends sig to the container's init.
	Kill(ctx context.Context, id string, sig syscall.Signal) error
}

// Runner runs one command and returns its combined output. Tests replace it.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs commands with os/exec. Output goes to a temporary file, not a pipe: `crun run --detach` leaves the
// container holding the stdio it inherited, so with a pipe cmd.Run would wait for an EOF that comes only when the
// container exits (herd-guest hung at boot on real Cloud Hypervisor VMs, never serving its API).
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	f, err := os.CreateTemp("", "herd-oci-out-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = f, f
	runErr := cmd.Run()
	out, err := os.ReadFile(f.Name())
	if err != nil {
		return nil, errors.Join(runErr, err)
	}
	return out, runErr
}

// Crun is Runtime over the crun binary.
type Crun struct {
	// Binary is crun's path (default /sbin/crun, where the initramfs puts it).
	Binary string
	// StateRoot is crun's --root (default /run/crun).
	StateRoot string
	Runner    Runner
}

func (c *Crun) run(ctx context.Context, args ...string) error {
	bin, root, run := c.Binary, c.StateRoot, c.Runner
	if bin == "" {
		bin = "/sbin/crun"
	}
	if root == "" {
		root = "/run/crun"
	}
	if run == nil {
		run = ExecRunner
	}
	out, err := run(ctx, bin, append([]string{"--root", root}, args...)...)
	if err != nil {
		return fmt.Errorf("oci: crun %v: %w: %s", args, err, bytes.TrimSpace(out))
	}
	return nil
}

func (c *Crun) Run(ctx context.Context, id, bundle string) error {
	return c.run(ctx, "run", "--detach", "--bundle", bundle, id)
}

func (c *Crun) Exec(ctx context.Context, id string, argv []string, detach bool) error {
	args := []string{"exec"}
	if detach {
		args = append(args, "--detach")
	}
	return c.run(ctx, append(append(args, id), argv...)...)
}

func (c *Crun) Kill(ctx context.Context, id string, sig syscall.Signal) error {
	return c.run(ctx, "kill", id, strconv.Itoa(int(sig)))
}
