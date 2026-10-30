//go:build darwin && arm64 && cgo

package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/everdict/herd/vmm/vz"
)

// newHost is Virtualization.framework (ADR R-D8): native vsock, virtio-fs, VOLUMES unless the Mac passed the
// save/restore probe.
func (h *Herdlet) newHost(ctx context.Context) (Backend, error) {
	host := h.MacHost
	if host == "" {
		n, err := os.Hostname()
		if err != nil {
			return Backend{}, err
		}
		host = strings.ToLower(strings.Split(n, ".")[0])
	}
	v, err := vz.New(vz.Config{StateDir: filepath.Join(h.StateDir, "vms"), SnapshotDir: filepath.Join(h.StateDir, "vm-snapshots"), Host: host,
		Probe: vz.ProbeConfig{Kernel: h.Kernel, Initrd: h.Initrd, Cmdline: h.Cmdline}}, vz.Native())
	if err != nil {
		return Backend{}, err
	}
	return Backend{VMM: v, Guests: v, Version: "virtualization.framework"}, nil
}

// vzBuilt reports whether this build can drive Virtualization.framework.
func vzBuilt() bool { return true }
