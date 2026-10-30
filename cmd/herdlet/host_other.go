//go:build !linux && !(darwin && arm64 && cgo)

package main

import (
	"context"
	"fmt"
	"runtime"
)

// newHost: herd runs on Linux (Cloud Hypervisor) and Apple Silicon Macs (Virtualization.framework) only (R-D7).
func newHost(ctx context.Context, o options) (Host, error) {
	return Host{}, fmt.Errorf("no microVM backend on %s/%s (cgo needed on macOS): herd runs on Linux with KVM and Apple Silicon Macs", runtime.GOOS, runtime.GOARCH)
}
