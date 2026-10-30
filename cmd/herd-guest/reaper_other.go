//go:build !linux

package main

import (
	"context"

	"github.com/everdict/herd/guest/oci"
)

// Outside linux herd-guest only serves -local, where it is not PID 1 and reaps nothing.
func startReaper() {}

func reapedRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return oci.ExecRunner(ctx, name, args...)
}
