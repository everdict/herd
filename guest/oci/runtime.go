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
	// runs; the container keeps running after Run returns, and the Watch follows its main process.
	Run(ctx context.Context, id, bundle string) (*Watch, error)
	// Exec runs argv inside a running container. With detach it returns once the process started; without, once it
	// ended, with an error if it failed.
	Exec(ctx context.Context, id string, argv []string, detach bool) error
	// Kill sends sig to the container's init.
	Kill(ctx context.Context, id string, sig syscall.Signal) error
}

// Runner runs one command and returns its combined output. The command gets extra as its descriptors 3, 4, ... Tests
// replace it.
type Runner func(ctx context.Context, extra []*os.File, name string, args ...string) ([]byte, error)

// ExecRunner runs commands with os/exec. Output goes to a temporary file, not a pipe: `crun run --detach` leaves the
// container holding the stdio it inherited, so with a pipe cmd.Run would wait for an EOF that comes only when the
// container exits (herd-guest hung at boot on real Cloud Hypervisor VMs, never serving its API).
func ExecRunner(ctx context.Context, extra []*os.File, name string, args ...string) ([]byte, error) {
	f, err := os.CreateTemp("", "herd-oci-out-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.ExtraFiles = extra
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

func (c *Crun) run(ctx context.Context, args ...string) error { return c.runWith(ctx, nil, args...) }

func (c *Crun) runWith(ctx context.Context, extra []*os.File, args ...string) error {
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
	out, err := run(ctx, extra, bin, append([]string{"--root", root}, args...)...)
	if err != nil {
		return fmt.Errorf("oci: crun %v: %w: %s", args, err, bytes.TrimSpace(out))
	}
	return nil
}

// Run starts the container with two pipes as its init's descriptors OutputFD and StatusFD (crun --preserve-fds), and
// follows them: herd-guest holds only their read ends, so the status pipe closes when the init is gone.
func (c *Crun) Run(ctx context.Context, id, bundle string) (*Watch, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("oci: %w", err)
	}
	stR, stW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("oci: %w", err)
	}
	err = c.runWith(ctx, []*os.File{outW, stW}, "run", "--detach", "--preserve-fds", "2", "--bundle", bundle, id)
	outW.Close()
	stW.Close()
	if err != nil {
		outR.Close()
		stR.Close()
		return nil, err
	}
	return NewWatch(id, outR, stR), nil
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
