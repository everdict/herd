//go:build darwin && arm64 && cgo

package deploy

import (
	"context"

	"github.com/everdict/herd/vmm/vz"
)

// newHost is Virtualization.framework (ADR R-D8, A-21, A-22): native vsock, virtio-fs, each VM's NIC into the
// in-process herd-egress, VOLUMES unless the Mac passed the save/restore probe and its console is unlocked.
func (h *Herdlet) newHost(ctx context.Context) (Backend, error) {
	api := h.API
	if s, err := readState(h.StateDir); err == nil && api == "" {
		api = s.API
	}
	return h.macHost(api, vz.Native(), vz.IORegSession{})
}

// vzBuilt reports whether this build can drive Virtualization.framework.
func vzBuilt() bool { return true }
