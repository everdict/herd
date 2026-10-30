package deploy

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/everdict/herd/vmm/cloudhypervisor"
)

// newHost is Cloud Hypervisor (ADR R-D8): one cloud-hypervisor process per VM, virtiofsd for the share, hybrid vsock.
func (h *Herdlet) newHost(ctx context.Context) (Backend, error) {
	v := cloudhypervisor.New(cloudhypervisor.Config{
		Binary: h.CHBinary, Virtiofsd: h.Virtiofsd, VirtiofsdArgs: strings.Fields(h.VirtiofsdArgs),
		StateDir: filepath.Join(h.StateDir, "vms"), SnapshotDir: filepath.Join(h.StateDir, "vm-snapshots"),
	})
	return Backend{VMM: v, Guests: v, Version: "cloud-hypervisor v54.0"}, nil
}

// vzBuilt reports whether this build can drive Virtualization.framework.
func vzBuilt() bool { return false }
