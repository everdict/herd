//go:build !linux && !(darwin && arm64 && cgo)

package deploy

import (
	"context"
	"fmt"
	"runtime"
)

// newHost: herd runs on Linux (Cloud Hypervisor) and Apple Silicon Macs (Virtualization.framework) only (R-D7).
func (h *Herdlet) newHost(ctx context.Context) (Backend, error) {
	return Backend{}, fmt.Errorf("no microVM backend on %s/%s (cgo needed on macOS): herd runs on Linux with KVM and Apple Silicon Macs", runtime.GOOS, runtime.GOARCH)
}

// vzBuilt reports whether this build can drive Virtualization.framework.
func vzBuilt() bool { return false }
